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
	sqliterepo "mbgw/internal/storage/sqlite"
)

// runNorthbound dispatches to one of two modes:
//
//   - normal (M1): `mbgw northbound --config <uspd.yaml> --db <path.sqlite>`
//     serves current values read from the SQLite database — the same one
//     `mbgw run` writes to.
//
//   - discovery (M3): `mbgw northbound --discovery --listen <addr>
//     [--fixture <fixture.yaml>] [--log <path.jsonl>]` is a "black box"
//     that logs every inbound frame and answers with a generic stub, per
//     TRD addendum §B.4. It never touches storage.Repo or a device —
//     --db/--config (register map) are not used in this mode at all.
func runNorthbound() {
	if len(os.Args) < 3 {
		printNorthboundUsage()
		os.Exit(1)
	}

	var cfgPath, dbPath, listenAddr, fixturePath, logPath string
	discovery := false

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
		case "--help", "-h":
			printNorthboundUsage()
			os.Exit(0)
		}
	}

	if discovery {
		runDiscoveryMode(listenAddr, fixturePath, logPath)
		return
	}

	if cfgPath == "" || dbPath == "" {
		printNorthboundUsage()
		os.Exit(1)
	}
	runServeMode(cfgPath, dbPath)
}

// runServeMode is the M1 normal-serving path: register map + SQLite Repo.
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

// runDiscoveryMode is the M3 discovery path: no Repo, no device — logs
// every frame and answers from a fixture (see internal/northbound's
// discovery.go doc comment for the safety rationale).
func runDiscoveryMode(listenAddr, fixturePath, logPath string) {
	if listenAddr == "" {
		fmt.Println("northbound --discovery: --listen is required (use an isolated address, not the production УСПД port)")
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

	log.Printf("northbound --discovery: слушаем %s (лог: %s) — к приборам НЕ обращаемся, только логируем и отвечаем заглушкой\n",
		listenAddr, logPath)

	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[FATAL] northbound --discovery: %v", err)
	}
	log.Println("northbound --discovery: остановлен")
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
	fmt.Println("  mbgw northbound --discovery --listen <addr> [--fixture <fixture.yaml>] [--log <path.jsonl>]")
	fmt.Println("      Discovery mode: log every inbound frame and reply with a stub (M3).")
	fmt.Println("      Never touches a device or the SQLite DB. Use an isolated --listen")
	fmt.Println("      address, separate from any production УСПД port.")
	fmt.Println("      --log defaults to discovery.jsonl if omitted.")
}
