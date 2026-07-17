package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"mbgw/internal/monitor"
	"mbgw/internal/northbound"
	"mbgw/internal/simulator"
	sqliterepo "mbgw/internal/storage/sqlite"
)

func runNorthbound() {
	if len(os.Args) < 3 {
		printNorthboundUsage()
		os.Exit(1)
	}

	var cfgPath, dbPath, listenAddr, fixturePath, logPath, simPath, cmd110 string
	discovery := false
	raw := false

	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--config":
			if i+1 < len(os.Args) {
				cfgPath = os.Args[i+1]
				i++
			}
		case "--db":
			if i+1 < len(os.Args) {
				dbPath = os.Args[i+1]
				i++
			}
		case "--discovery":
			discovery = true
		case "--raw":
			raw = true
		case "--listen":
			if i+1 < len(os.Args) {
				listenAddr = os.Args[i+1]
				i++
			}
		case "--fixture":
			if i+1 < len(os.Args) {
				fixturePath = os.Args[i+1]
				i++
			}
		case "--log":
			if i+1 < len(os.Args) {
				logPath = os.Args[i+1]
				i++
			}
		case "--sim":
			if i+1 < len(os.Args) {
				simPath = os.Args[i+1]
				i++
			}
		case "--cmd110":
			if i+1 < len(os.Args) {
				cmd110 = os.Args[i+1]
				i++
			}
		case "--help", "-h":
			printNorthboundUsage()
			os.Exit(0)
		}
	}

	if discovery {
		if raw {
			runRawDiscoveryMode(listenAddr, logPath, simPath, cmd110)
		} else {
			runDiscoveryMode(listenAddr, fixturePath, logPath)
		}
		return
	}

	if cfgPath == "" || dbPath == "" {
		printNorthboundUsage()
		os.Exit(1)
	}
	runServeMode(cfgPath, dbPath)
}

func runServeMode(cfgPath, dbPath string) {
	uspd, err := northbound.LoadUSPDConfig(cfgPath)
	if err != nil {
		log.Fatalf("[FATAL] northbound: %v", err)
	}

	repo, err := sqliterepo.New(dbPath)
	if err != nil {
		log.Fatalf("[FATAL] northbound: ошибка хранилища: %v", err)
	}
	if err := repo.InitSchema(context.Background()); err != nil {
		log.Fatalf("[FATAL] northbound: ошибка инициализации схемы: %v", err)
	}

	eventBus := monitor.NewBus(nil)
	srv := northbound.NewServer(uspd, repo, eventBus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "northbound")

	log.Printf("northbound: слушаем %s как unit %d (устройство=%s, uspd=%s)\n",
		uspd.Listen, uspd.UnitID, uspd.DeviceID, uspd.ID)

	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[FATAL] northbound: %v", err)
	}
	log.Println("northbound: остановлен")
}

func runDiscoveryMode(listenAddr, fixturePath, logPath string) {
	if listenAddr == "" {
		fmt.Println("northbound --discovery: --listen is required")
		os.Exit(1)
	}
	if logPath == "" {
		logPath = "discovery.jsonl"
	}

	fixture := northbound.DiscoveryFixture{}
	if fixturePath != "" {
		f, err := northbound.LoadDiscoveryFixture(fixturePath)
		if err != nil {
			log.Fatalf("[FATAL] northbound --discovery: %v", err)
		}
		fixture = f
	}

	dlog, err := northbound.NewDiscoveryLog(logPath)
	if err != nil {
		log.Fatalf("[FATAL] northbound --discovery: %v", err)
	}
	defer dlog.Close()

	srv := northbound.NewDiscoveryServer(listenAddr, fixture, dlog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "northbound --discovery")

	log.Printf("northbound --discovery (MBAP): слушаем %s (лог: %s)\n", listenAddr, logPath)

	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[FATAL] northbound --discovery: %v", err)
	}
	log.Println("northbound --discovery: остановлен")
}

func runRawDiscoveryMode(listenAddr, logPath, simPath, cmd110 string) {
	if listenAddr == "" {
		fmt.Println("northbound --discovery --raw: --listen is required")
		os.Exit(1)
	}
	if logPath == "" {
		logPath = "discovery_raw.jsonl"
	}

	// Config precedence: --sim file if given, else defaults; --cmd110 is a
	// shortcut override applied on top (handy for a quick probe without a file).
	cfg := simulator.DefaultAkronSimConfig()
	if simPath != "" {
		loaded, err := northbound.LoadAkronSimConfig(simPath)
		if err != nil {
			log.Fatalf("[FATAL] northbound --discovery --raw: %v", err)
		}
		cfg = loaded
	}
	if cmd110 != "" {
		cfg.Cmd110 = cmd110
	}
	simulator.SetAkronSimConfig(cfg)

	dlog, err := northbound.NewDiscoveryLog(logPath)
	if err != nil {
		log.Fatalf("[FATAL] northbound --discovery --raw: %v", err)
	}
	defer dlog.Close()

	srv := northbound.NewRawDiscoveryServer(listenAddr, dlog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "northbound --discovery --raw")

	log.Printf("northbound --discovery --raw: слушаем %s (лог: %s) [%s]\n",
		listenAddr, logPath, simulator.ActiveAkronSummary())

	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[FATAL] northbound --discovery --raw: %v", err)
	}
	log.Println("northbound --discovery --raw: остановлен")
}

func waitForShutdownSignal(cancel context.CancelFunc, label string) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Printf("%s: получен сигнал завершения, останавливаемся...\n", label)
		cancel()
	}()
}

func printNorthboundUsage() {
	fmt.Println("Usage:")
	fmt.Println("  mbgw northbound --config <uspd.yaml> --db <path.sqlite>")
	fmt.Println("      Serve current values upward as a Modbus TCP slave (M1).")
	fmt.Println()
	fmt.Println("  mbgw northbound --discovery --listen <addr> [--fixture <f.yaml>] [--log <p.jsonl>]")
	fmt.Println("      MBAP discovery: log every inbound Modbus TCP frame, reply with a stub (M3).")
	fmt.Println()
	fmt.Println("  mbgw northbound --discovery --raw --listen <addr> [--log <p.jsonl>] [--sim <s.yaml>] [--cmd110 <mode>]")
	fmt.Println("      Raw-TCP discovery: log bare Modbus RTU and answer as an Akron (M3).")
	fmt.Println("      --sim <file>  YAML controlling responder behaviour (edit on server, no rebuild):")
	fmt.Println("                    cmd110, live_clock, identity, per-command overrides.")
	fmt.Println("      --cmd110 <m>  quick override of the command-110 reply: nil|empty|ready|zero|echo")
	fmt.Println()
	fmt.Println("      Discovery modes never touch a device or the SQLite DB. Use an isolated")
	fmt.Println("      --listen address, separate from any production УСПД port.")
}
