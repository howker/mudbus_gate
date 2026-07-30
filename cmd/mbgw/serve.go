package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mbgw/internal/config"
	"mbgw/internal/device"
	"mbgw/internal/lease"
	"mbgw/internal/monitor"
	"mbgw/internal/northbound"
	"mbgw/internal/pollcore"
	"mbgw/internal/poller"
	"mbgw/internal/profile"
	"mbgw/internal/scheduler"
	"mbgw/internal/session"
	sqliterepo "mbgw/internal/storage/sqlite"
	"mbgw/internal/transport"
	"mbgw/internal/web"
)

// serve runs southbound polling (the same loop `run` drives) and the
// northbound Akron carrier (the same loop `runAkronLiveMode` drives) in
// ONE process, sharing one *sqliterepo.Repo/SQLite connection and one
// monitor.Bus — replacing the two separately-launched `mbgw run` /
// `mbgw northbound --serve-akron` processes used until now (see
// PROJECT_HANDOFF.md item 2, "объединить southbound+northbound").
//
// Deliberately additive: `run` and `northbound` are untouched and still
// work exactly as before, so the already-confirmed real-device setup is
// never put at risk by this change. serve only starts the northbound
// carrier if config.yaml has a northbound_akron section — a config.yaml
// without it behaves exactly like `run` (no northbound carrier, nothing
// new to break).
//
// Usage:
//
//	mbgw serve --config config.akron_real.yaml
func serve() {
	cfgPath := "config.yaml"
	for i := 2; i < len(os.Args); i++ {
		if os.Args[i] == "--config" && i+1 < len(os.Args) {
			cfgPath = os.Args[i+1]
			i++
		}
	}

	logFile, _ := os.OpenFile("mbgw.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	defer logFile.Close()
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
	log.SetFlags(log.Ldate | log.Ltime)
	log.Println("=== запуск шлюза mbgw (serve: southbound+northbound в одном процессе) ===")

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("[FATAL] ошибка конфигурации (%s): %v", cfgPath, err)
	}

	repo, err := sqliterepo.New(cfg.App.StoragePath)
	if err != nil {
		log.Fatalf("[FATAL] ошибка хранилища: %v", err)
	}
	if err := repo.InitSchema(context.Background()); err != nil {
		log.Fatalf("[FATAL] ошибка инициализации схемы: %v", err)
	}
	// Same InitArchiveSchema call `run` makes — needed before any archive
	// poll can SaveHourlyArchive, and idempotent if already applied.
	if err := repo.InitArchiveSchema(context.Background()); err != nil {
		log.Fatalf("[FATAL] ошибка инициализации схемы архива: %v", err)
	}
	log.Printf("[OK] Хранилище инициализировано (%s)\n", cfg.App.StoragePath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := cfg.App.WebPort
	if port == 0 {
		port = 8080
	}
	webServer := web.NewServer(repo, port)
	// Start deferred until after scheduler/devices exist (see run.go).

	leaseMgr := lease.New()
	eventBus := monitor.NewBus(nil)
	sched := scheduler.New(eventBus)
	devices := make(map[string]*device.Device)

	for _, devCfg := range cfg.Devices {
		p, err := profile.Parse(devCfg.Profile)
		if err != nil {
			log.Printf("[ERROR] прибор %s: ошибка профиля %s: %v\n", devCfg.ID, devCfg.Profile, err)
			continue
		}
		sess, err := session.NewFromProfile(p.Session)
		if err != nil {
			log.Printf("[ERROR] неизвестный тип сессии %s: %v\n", p.Session.Type, err)
			continue
		}

		trParams := transport.Params{
			Kind:            transport.Kind(devCfg.Transport.Kind),
			Host:            devCfg.Transport.Host,
			Port:            devCfg.Transport.Port,
			COM:             devCfg.Transport.COM,
			Baudrate:        devCfg.Transport.Baudrate,
			Parity:          devCfg.Transport.Parity,
			StopBits:        devCfg.Transport.StopBits,
			ResponseTimeout: time.Duration(devCfg.Transport.TimeoutMs) * time.Millisecond,
		}
		tr, err := transport.New(trParams)
		if err != nil {
			log.Printf("[ERROR] прибор %s: не удалось создать транспорт: %v\n", devCfg.ID, err)
			continue
		}
		if err := tr.Open(ctx); err != nil {
			log.Printf("[ERROR] прибор %s: не удалось открыть транспорт: %v\n", devCfg.ID, err)
			continue
		}
		if err := sess.Open(ctx, tr); err != nil {
			log.Printf("[ERROR] ошибка сессии %s: %v\n", devCfg.ID, err)
			continue
		}
		isTCP := devCfg.Transport.Kind == "modbus_tcp"

		unitID := devCfg.Transport.UnitID
		if unitID == 0 {
			unitID = 1
		}
		reader := pollcore.New(tr, isTCP, unitID)
		dev := device.New(devCfg.ID, p, reader, sess, repo, leaseMgr)
		dev.GapScanWindowHours = devCfg.Backfill.GapScanWindowOrDefault()
		devices[devCfg.ID] = dev

		// See cmd/mbgw/run.go's identical block for why this must be
		// blocking (not `go dev.BackfillArchives(...)`) and run before
		// sched.Register: it previously raced the scheduler's
		// due-immediately first archive tick for the same per-device
		// lease, silently costing an hour of collection at startup.
		if len(p.Archives) > 0 {
			dev.BackfillArchives(ctx, device.BackfillOptions{
				MaxDepthHours: devCfg.Backfill.MaxDepthHours,
			})
		}

		archiveInterval := time.Duration(0)
		if len(p.Archives) > 0 {
			archiveInterval = 1 * time.Hour
		}
		currentInterval := devCfg.CurrentPollInterval()
		sched.Register(devCfg.ID, currentInterval, archiveInterval, nil)
		log.Printf("[OK] прибор %s зарегистрирован (текущие каждые %s, архив каждые %s)\n",
			devCfg.ID, currentInterval, archiveInterval)
	}

	webServer.SetManualPoll(func() {
		log.Printf("[WEB] ручной опрос запрошен для %d прибор(ов)\n", len(devices))
		for id, d := range devices {
			sched.RequestManualPoll(id, scheduler.KindCurrent)
			if len(d.Profile.Archives) > 0 {
				go d.BackfillArchives(ctx, device.BackfillOptions{MaxDepthHours: 0})
			}
		}
	})
	go webServer.Start(ctx)

	pl := poller.New(sched, devices, 1*time.Second)
	go pl.Run(ctx)

	// Northbound Akron carrier — opt-in via config.yaml's northbound_akron
	// section, reusing this same `repo` (one SQLite connection, not the
	// two-process/two-connection setup `run` + `northbound --serve-akron`
	// needed the WAL/busy_timeout fix for).
	if na := cfg.NorthboundAkron; na != nil && na.Listen != "" && na.DeviceID != "" {
		logPath := na.Log
		if logPath == "" {
			logPath = "akron_carrier_live.jsonl"
		}
		dlog, err := northbound.NewDiscoveryLog(logPath)
		if err != nil {
			log.Fatalf("[FATAL] northbound_akron: %v", err)
		}
		defer dlog.Close()

		srv := northbound.NewAkronLiveServer(na.Listen, dlog, repo, na.DeviceID)
		go func() {
			log.Printf("[OK] northbound (Akron carrier): слушаем %s, прибор %s (лог: %s)\n",
				na.Listen, na.DeviceID, logPath)
			if err := srv.Listen(ctx); err != nil {
				log.Printf("[ERROR] northbound (Akron carrier): %v\n", err)
			}
		}()
	} else {
		log.Println("[INFO] northbound_akron не задан в конфиге — carrier не запущен (только southbound, как раньше в `run`)")
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("=== получен сигнал завершения. остановка... ===")
}
