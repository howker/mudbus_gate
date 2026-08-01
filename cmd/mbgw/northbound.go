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
	serveAkron := false
	var deviceID string
	serveVKM := false
	var vkmString string

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
		case "--serve-akron":
			serveAkron = true
		case "--device":
			if i+1 < len(os.Args) {
				deviceID = os.Args[i+1]
				i++
			}
		case "--serve-vkm":
			serveVKM = true
		case "--vkm-string":
			if i+1 < len(os.Args) {
				vkmString = os.Args[i+1]
				i++
			}
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

	if serveAkron {
		runAkronLiveMode(listenAddr, dbPath, deviceID, logPath)
		return
	}

	if serveVKM {
		runVKMLiveMode(listenAddr, vkmString, logPath, dbPath, deviceID)
		return
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

// runAkronLiveMode starts the PRODUCTION raw-RTU carrier: the discovery
// transport with the database-backed Akron responder
// (northbound.NewAkronLiveServer). This is what actually stands in front
// of Энергосфера as the АКРОН-01-1-type УСПД in model B.
func runAkronLiveMode(listenAddr, dbPath, deviceID, logPath string) {
	if listenAddr == "" || dbPath == "" || deviceID == "" {
		fmt.Println("northbound --serve-akron: --listen, --db и --device обязательны")
		os.Exit(1)
	}
	if logPath == "" {
		logPath = "akron_live.jsonl"
	}

	repo, err := sqliterepo.New(dbPath)
	if err != nil {
		log.Fatalf("[FATAL] northbound --serve-akron: ошибка хранилища: %v", err)
	}
	// InitSchema is idempotent and also provisions the archive/passport
	// tables — safe when sharing the DB file with a running gateway.
	if err := repo.InitSchema(context.Background()); err != nil {
		log.Fatalf("[FATAL] northbound --serve-akron: ошибка схемы: %v", err)
	}

	dlog, err := northbound.NewDiscoveryLog(logPath)
	if err != nil {
		log.Fatalf("[FATAL] northbound --serve-akron: %v", err)
	}
	defer dlog.Close()

	srv := northbound.NewAkronLiveServer(listenAddr, dlog, repo, deviceID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "northbound --serve-akron")

	log.Printf("northbound --serve-akron: слушаем %s, прибор %s, база %s (лог: %s)\n",
		listenAddr, deviceID, dbPath, logPath)

	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[FATAL] northbound --serve-akron: %v", err)
	}
	log.Println("northbound --serve-akron: остановлен")
}

// runVKMLiveMode starts the MBAP (Modbus TCP) carrier that lets mbgw stand
// in front of Энергосфера as a УВП-280А-shaped device serving ВКМ-360
// archives (northbound.VKMServer). Confirmed against the live ЭС config:
// this record uses plain Modbus TCP, not raw RTU — unlike the Akron
// carrier.
//
// vkmString остаётся резервным вариантом для отладки механики протокола
// без БД/живого прибора (--vkm-string, без --db/--device). Боевой режим —
// DBVKMArchiveSource, читает реально собранные архивы из БД (см.
// internal/device/vkm_hourly.go и internal/northbound/vkm_live.go).
//
// dbPath/deviceID, если заданы, включают боевой режим: строка архива
// берётся из реально собранных данных (internal/device/vkm_hourly.go),
// а не из фиксированной/настраиваемой заглушки. Без них поведение
// прежнее — для отладки механики протокола без живого прибора.
func runVKMLiveMode(listenAddr, vkmString, logPath, dbPath, deviceID string) {
	if listenAddr == "" {
		fmt.Println("northbound --serve-vkm: --listen обязателен")
		os.Exit(1)
	}
	// No built-in default string: leaving it empty lets the carrier build a
	// spec-compliant archive string (period + parameters + NUL, per the
	// ЭЛЕМЕР-ВКМ-360 register map p.7). --vkm-string remains available to
	// force a raw string for experiments.
	if logPath == "" {
		logPath = "vkm_live.jsonl"
	}

	dlog, err := northbound.NewDiscoveryLog(logPath)
	if err != nil {
		log.Fatalf("[FATAL] northbound --serve-vkm: %v", err)
	}
	defer dlog.Close()

	var newSrc func() northbound.VKMArchiveSource
	if dbPath != "" && deviceID != "" {
		repo, err := sqliterepo.New(dbPath)
		if err != nil {
			log.Fatalf("[FATAL] northbound --serve-vkm: открытие БД: %v", err)
		}
		defer repo.Close()
		if err := repo.InitArchiveSchema(context.Background()); err != nil {
			log.Fatalf("[FATAL] northbound --serve-vkm: инициализация схемы архива: %v", err)
		}
		src := northbound.NewDBVKMArchiveSource(repo, deviceID)
		newSrc = func() northbound.VKMArchiveSource { return src }
		log.Printf("northbound --serve-vkm: боевой режим — источник архива: БД %s, прибор %s\n", dbPath, deviceID)
	} else {
		// Archive string comes from vkm_config.txt when present, falling back
		// to --vkm-string (or the built-in default). This is what lets the
		// string be changed on the Энергосфера server by editing a text file
		// instead of rebuilding and re-uploading the binary.
		newSrc = func() northbound.VKMArchiveSource {
			return northbound.ConfigArchiveSource{Fallback: vkmString}
		}
		log.Println("northbound --serve-vkm: тестовый режим (нет --db/--device) — фиксированная/настраиваемая строка")
	}

	srv := northbound.NewVKMServer(listenAddr, newSrc)
	srv.Log = dlog

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "northbound --serve-vkm")

	log.Printf("northbound --serve-vkm: слушаем %s (лог: %s)\n", listenAddr, logPath)
	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[FATAL] northbound --serve-vkm: %v", err)
	}
	log.Println("northbound --serve-vkm: остановлен")
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
	fmt.Println("  mbgw northbound --serve-akron --listen <addr> --db <path> --device <id> [--log <p.jsonl>]")
	fmt.Println("  mbgw northbound --serve-vkm --listen <addr> --db <p> --device <id> [--log <p.jsonl>]   (боевой режим)")
	fmt.Println("  mbgw northbound --serve-vkm --listen <addr> [--vkm-string <s>] [--log <p.jsonl>]        (тестовый режим, без --db/--device)")
	fmt.Println("      Raw-TCP discovery: log bare Modbus RTU and answer as an Akron (M3).")
	fmt.Println("      --sim <file>  YAML controlling responder behaviour (edit on server, no rebuild):")
	fmt.Println("                    cmd110, live_clock, identity, per-command overrides.")
	fmt.Println("      --cmd110 <m>  quick override of the command-110 reply: nil|empty|ready|zero|echo")
	fmt.Println()
	fmt.Println("      Discovery modes never touch a device or the SQLite DB. Use an isolated")
	fmt.Println("      --listen address, separate from any production УСПД port.")
}
