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
	vkmProbe := false
	numProbe := false
	numProbeLog := "vkm_numprobe.txt"

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
		case "--vkm-probe":
			vkmProbe = true
		case "--vkm-numprobe":
			numProbe = true
		case "--vkm-numprobe-log":
			if i+1 < len(os.Args) {
				numProbeLog = os.Args[i+1]
			}
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
		runVKMLiveMode(listenAddr, vkmString, logPath, dbPath, deviceID, vkmProbe, numProbe, numProbeLog)
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
		log.Fatalf("[КРИТИЧНО] северный интерфейс: %v", err)
	}

	repo, err := sqliterepo.New(dbPath)
	if err != nil {
		log.Fatalf("[КРИТИЧНО] северный интерфейс: ошибка хранилища: %v", err)
	}
	if err := repo.InitSchema(context.Background()); err != nil {
		log.Fatalf("[КРИТИЧНО] северный интерфейс: ошибка инициализации схемы: %v", err)
	}

	eventBus := monitor.NewBus(nil)
	srv := northbound.NewServer(uspd, repo, eventBus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "северный интерфейс")

	log.Printf("северный интерфейс: слушаем %s как Unit ID %d (прибор=%s, УСПД=%s)\n",
		uspd.Listen, uspd.UnitID, uspd.DeviceID, uspd.ID)

	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[КРИТИЧНО] северный интерфейс: %v", err)
	}
	log.Println("северный интерфейс: остановлен")
}

func runDiscoveryMode(listenAddr, fixturePath, logPath string) {
	if listenAddr == "" {
		fmt.Println("northbound --discovery: обязательно укажите --listen")
		os.Exit(1)
	}
	if logPath == "" {
		logPath = "discovery.jsonl"
	}

	fixture := northbound.DiscoveryFixture{}
	if fixturePath != "" {
		f, err := northbound.LoadDiscoveryFixture(fixturePath)
		if err != nil {
			log.Fatalf("[КРИТИЧНО] северный интерфейс --discovery: %v", err)
		}
		fixture = f
	}

	dlog, err := northbound.NewDiscoveryLog(logPath)
	if err != nil {
		log.Fatalf("[КРИТИЧНО] северный интерфейс --discovery: %v", err)
	}
	defer dlog.Close()

	srv := northbound.NewDiscoveryServer(listenAddr, fixture, dlog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "северный интерфейс --discovery")

	log.Printf("северный интерфейс --discovery (MBAP): слушаем %s (лог: %s)\n", listenAddr, logPath)

	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[КРИТИЧНО] северный интерфейс --discovery: %v", err)
	}
	log.Println("северный интерфейс --discovery: остановлен")
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
		log.Fatalf("[КРИТИЧНО] северный интерфейс --serve-akron: ошибка хранилища: %v", err)
	}
	// InitSchema is idempotent and also provisions the archive/passport
	// tables — safe when sharing the DB file with a running gateway.
	if err := repo.InitSchema(context.Background()); err != nil {
		log.Fatalf("[КРИТИЧНО] северный интерфейс --serve-akron: ошибка схемы: %v", err)
	}

	dlog, err := northbound.NewDiscoveryLog(logPath)
	if err != nil {
		log.Fatalf("[КРИТИЧНО] северный интерфейс --serve-akron: %v", err)
	}
	defer dlog.Close()

	srv := northbound.NewAkronLiveServer(listenAddr, dlog, repo, deviceID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "северный интерфейс --serve-akron")

	log.Printf("северный интерфейс --serve-akron: слушаем %s, прибор %s, база %s (лог: %s)\n",
		listenAddr, deviceID, dbPath, logPath)

	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[КРИТИЧНО] северный интерфейс --serve-akron: %v", err)
	}
	log.Println("северный интерфейс --serve-akron: остановлен")
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
func runVKMLiveMode(listenAddr, vkmString, logPath, dbPath, deviceID string, vkmProbe, numProbe bool, numProbeLog string) {
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
		log.Fatalf("[КРИТИЧНО] северный интерфейс --serve-vkm: %v", err)
	}
	defer dlog.Close()

	var newSrc func() northbound.VKMArchiveSource
	if dbPath != "" && deviceID != "" {
		repo, err := sqliterepo.New(dbPath)
		if err != nil {
			log.Fatalf("[КРИТИЧНО] северный интерфейс --serve-vkm: открытие БД: %v", err)
		}
		defer repo.Close()
		if err := repo.InitArchiveSchema(context.Background()); err != nil {
			log.Fatalf("[КРИТИЧНО] северный интерфейс --serve-vkm: инициализация схемы архива: %v", err)
		}
		src := northbound.NewDBVKMArchiveSource(repo, deviceID)
		src.ProbeVariants = vkmProbe
		src.NumFormatProbe = numProbe
		src.NumProbeLogPath = numProbeLog
		newSrc = func() northbound.VKMArchiveSource { return src }
		log.Printf("северный интерфейс --serve-vkm: боевой режим — источник архива: БД %s, прибор %s\n", dbPath, deviceID)
		if vkmProbe {
			log.Println("северный интерфейс --serve-vkm: режим перебора гипотез (--vkm-probe) включён:")
			log.Println("  пока ЭС повторяет запрос одного периода, каждая попытка получает СЛЕДУЮЩИЙ вариант строки;")
			log.Println("  когда ЭС примет период и пойдёт дальше — в логе будет видно, на каком варианте это случилось.")
		}
		if numProbe {
			log.Println("северный интерфейс --serve-vkm: режим перебора формата числа (--vkm-numprobe) включён:")
			log.Printf("  каждому экспоненциальному полю присвоен свой формат; карта пишется в %s\n", numProbeLog)
			log.Println("  посмотри в ЭС, какие каналы стали ненулевыми, и сопоставь с форматом по этому файлу.")
		}
	} else {
		// Archive string comes from vkm_config.txt when present, falling back
		// to --vkm-string (or the built-in default). This is what lets the
		// string be changed on the Энергосфера server by editing a text file
		// instead of rebuilding and re-uploading the binary.
		newSrc = func() northbound.VKMArchiveSource {
			return northbound.ConfigArchiveSource{Fallback: vkmString}
		}
		log.Println("северный интерфейс --serve-vkm: тестовый режим (нет --db/--device) — фиксированная/настраиваемая строка")
	}

	srv := northbound.NewVKMServer(listenAddr, newSrc)
	srv.Log = dlog

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "северный интерфейс --serve-vkm")

	log.Printf("северный интерфейс --serve-vkm: слушаем %s (лог: %s)\n", listenAddr, logPath)
	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[КРИТИЧНО] северный интерфейс --serve-vkm: %v", err)
	}
	log.Println("северный интерфейс --serve-vkm: остановлен")
}

func runRawDiscoveryMode(listenAddr, logPath, simPath, cmd110 string) {
	if listenAddr == "" {
		fmt.Println("northbound --discovery --raw: обязательно укажите --listen")
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
			log.Fatalf("[КРИТИЧНО] северный интерфейс --discovery --raw: %v", err)
		}
		cfg = loaded
	}
	if cmd110 != "" {
		cfg.Cmd110 = cmd110
	}
	simulator.SetAkronSimConfig(cfg)

	dlog, err := northbound.NewDiscoveryLog(logPath)
	if err != nil {
		log.Fatalf("[КРИТИЧНО] северный интерфейс --discovery --raw: %v", err)
	}
	defer dlog.Close()

	srv := northbound.NewRawDiscoveryServer(listenAddr, dlog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "северный интерфейс --discovery --raw")

	log.Printf("северный интерфейс --discovery --raw: слушаем %s (лог: %s) [%s]\n",
		listenAddr, logPath, simulator.ActiveAkronSummary())

	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[КРИТИЧНО] северный интерфейс --discovery --raw: %v", err)
	}
	log.Println("северный интерфейс --discovery --raw: остановлен")
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
	fmt.Println("Использование:")
	fmt.Println("  mbgw northbound --config <uspd.yaml> --db <path.sqlite>")
	fmt.Println("      Отдаёт текущие значения наружу как ведомое устройство Modbus TCP (M1).")
	fmt.Println()
	fmt.Println("  mbgw northbound --discovery --listen <addr> [--fixture <f.yaml>] [--log <p.jsonl>]")
	fmt.Println("      Диагностика MBAP: журналирует каждый входящий кадр Modbus TCP и отвечает заглушкой (M3).")
	fmt.Println()
	fmt.Println("  mbgw northbound --discovery --raw --listen <addr> [--log <p.jsonl>] [--sim <s.yaml>] [--cmd110 <mode>]")
	fmt.Println("  mbgw northbound --serve-akron --listen <addr> --db <path> --device <id> [--log <p.jsonl>]")
	fmt.Println("  mbgw northbound --serve-vkm --listen <addr> --db <p> --device <id> [--log <p.jsonl>]   (боевой режим)")
	fmt.Println("  mbgw northbound --serve-vkm --listen <addr> [--vkm-string <s>] [--log <p.jsonl>]        (тестовый режим, без --db/--device)")
	fmt.Println("      Диагностика Raw TCP: журналирует Modbus RTU без обёртки и отвечает как Акрон (M3).")
	fmt.Println("      --sim <file>  YAML с настройками поведения ответчика (можно менять на сервере без пересборки):")
	fmt.Println("                    cmd110, живые часы, идентификация, переопределения отдельных команд.")
	fmt.Println("      --cmd110 <m>  быстрое переопределение ответа команды 110: nil|empty|ready|zero|echo")
	fmt.Println()
	fmt.Println("      Режимы диагностики не обращаются к прибору и базе SQLite. Используйте отдельный")
	fmt.Println("      адрес --listen, не совпадающий с рабочим портом УСПД.")
}
