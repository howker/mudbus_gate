package main

import (
	"context"
	"io"
	"log"
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
	"os"
	"os/signal"
	"syscall"
	"time"
)

func run() {
	logFile, _ := os.OpenFile("mbgw.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	defer logFile.Close()
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
	log.SetFlags(log.Ldate | log.Ltime)
	log.Println("=== запуск шлюза mbgw ===")
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("[FATAL] ошибка конфигурации: %v", err)
	}
	repo, err := sqliterepo.New(cfg.App.StoragePath)
	if err != nil {
		log.Fatalf("[FATAL] ошибка хранилища: %v", err)
	}
	if err := repo.InitSchema(context.Background()); err != nil {
		log.Fatalf("[FATAL] ошибка инициализации схемы: %v", err)
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
		trParams := transport.Params{
			Kind:            transport.Kind(devCfg.Transport.Kind),
			Host:            devCfg.Transport.Host,
			Port:            devCfg.Transport.Port,
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
		reader := pollcore.New(tr, isTCP, 1)
		dev := device.New(devCfg.ID, p, reader, sess, repo, leaseMgr)
		devices[devCfg.ID] = dev

		// One-time static identity read (Akron command 101) so the
		// upstream carrier can answer 101 with the real serial. Gated to
		// Akron devices inside DetectPassport; non-fatal. Lives here (not
		// in Device.Start) because the poll path uses internal/poller and
		// never calls Start.
		dev.DetectPassport(ctx)

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
