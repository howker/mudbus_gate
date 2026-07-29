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
	"mbgw/internal/pollcore"
	"mbgw/internal/poller"
	"mbgw/internal/profile"
	"mbgw/internal/scheduler"
	"mbgw/internal/session"
	sqliterepo "mbgw/internal/storage/sqlite"
	"mbgw/internal/transport"
	"mbgw/internal/web"
)

// run starts the gateway. --config <path> selects the config file
// (default "config.yaml"). Added because a config.yaml already exists in
// the project for the 4-mock-device smoke-test setup (TEST_STRATEGY.md
// S3/S6) — a real deployment (e.g. our real Акрон-01 on COM105) needs its
// own config file without overwriting or conflicting with that one.
func run() {
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
	log.Println("=== запуск шлюза mbgw ===")
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
	// InitArchiveSchema creates archive_hourly (see
	// internal/storage/sqlite/repo_archive.go) — separate from InitSchema
	// because it was added later, alongside the M4 Akron work. Missing
	// this call worked by accident so far only because mbgw.db already
	// had the table from earlier seedakron/testing use of the same file;
	// on a genuinely fresh database, PollArchives' SaveHourlyArchive call
	// would fail with "no such table: archive_hourly".
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
	go webServer.Start(ctx)
	leaseMgr := lease.New()

	// eventBus feeds the future SSE /monitor/stream endpoint (T13) and
	// gives channel/scheduler events a real destination instead of the
	// NoopEventRecorder default - events currently have no durable
	// persistence (internal/monitor's comm_events table does not exist
	// yet, see monitor.Persister's doc comment) but ARE delivered to any
	// live subscriber.
	eventBus := monitor.NewBus(nil)

	// Devices are now polled centrally through internal/scheduler +
	// internal/poller (see IMPLEMENTATION_BACKLOG.md T11), replacing the
	// earlier one-goroutine-per-device dev.Start() ticker loop - this is
	// what makes manual polls, priority, deferred retry, and a bounded
	// queue actually apply across the whole gateway, not just per device.
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

		// Transport parameters: COM/Baudrate/Parity/StopBits matter for
		// rtu_serial and tcp_serial (a real or converter-emulated COM
		// port, e.g. our real Акрон-01 on COM105) - Host/Port matter for
		// modbus_tcp. Building both sets unconditionally is harmless:
		// transport.New only reads the fields relevant to Kind (see
		// internal/transport/serial.go / tcp.go).
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

		// Modbus slave/bus address - configurable per device now
		// (previously hardcoded to 1 for every device); defaults to 1
		// when unset in config.yaml, matching the old behaviour.
		unitID := devCfg.Transport.UnitID
		if unitID == 0 {
			unitID = 1
		}
		reader := pollcore.New(tr, isTCP, unitID)
		dev := device.New(devCfg.ID, p, reader, sess, repo, leaseMgr)
		dev.GapScanWindowHours = devCfg.Backfill.GapScanWindowOrDefault()
		devices[devCfg.ID] = dev

		// Startup archive catch-up: pull whatever history the device holds
		// that we don't have yet (variant "В"), or down to a configured
		// depth (variant "Б"). Runs once, before the periodic poller takes
		// over, so a gateway that was off for hours/days/weeks fills its
		// gap from the device's ring buffer at startup. Non-blocking so a
		// slow deep sweep doesn't hold up the rest of bring-up.
		if len(p.Archives) > 0 {
			go dev.BackfillArchives(ctx, device.BackfillOptions{
				MaxDepthHours: devCfg.Backfill.MaxDepthHours,
			})
		}

		archiveInterval := time.Duration(0)
		if len(p.Archives) > 0 {
			archiveInterval = 1 * time.Hour
		}
		sched.Register(devCfg.ID, 3*time.Second, archiveInterval, nil)
	}

	p := poller.New(sched, devices, 1*time.Second)
	go p.Run(ctx)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("=== получен сигнал завершения. остановка... ===")
}
