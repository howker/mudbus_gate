package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"mbgw/internal/dbg"
	"mbgw/internal/device"
	"mbgw/internal/integration"
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

// server is the target single-process command (T14 minimal-slice step 2):
// ONE mbgw process that reads its device list from the DATABASE (not
// config.yaml) and runs everything the four separate windows used to —
// southbound polling for every enabled device, the Akron northbound
// carrier, and ВКМ→Энергосфера direct-DB sync — so the future Web UI
// (step 4) can add/edit devices and channel mappings and have them take
// effect without the operator hand-editing text files or juggling
// windows.
//
// Deliberately additive, same principle serve() already established:
// `run`, `serve`, and `northbound --serve-akron/--serve-tsrv` are
// UNTOUCHED and keep working exactly as before — this is a new command,
// not a replacement, so the already-confirmed real-device setup is never
// put at risk. An operator can keep using the four-window setup
// indefinitely if they prefer it; `server` is opt-in.
//
// What this does NOT do yet (later steps in the same T14 slice):
//   - No REST API for the Web UI to call (step 3).
//   - No Web UI screens beyond the existing read-only dashboard (step 4).
//   - Devices are read ONCE at startup, not hot-reloaded when the (not-
//     yet-existing) UI edits them — see the doc comment on the startup
//     loop below for exactly what that means today.
//
// Usage:
//
//	mbgw server --db mbgw_server.db --port 8080
//
// --port is optional (default 8080) — override it if that port conflicts
// with something else already running on the server (this exact machine
// has previously hit a "bind: access forbidden" conflict on a different
// port — see mbgw.log history — so this is not a hypothetical concern).
func runServer() {
	dbPath := "mbgw_server.db"
	// portFlagGiven distinguishes "--port was explicitly typed" from "not
	// passed at all" — this matters because the port-selection rule below
	// treats them very differently (see the doc comment further down).
	portFlagGiven := false
	portFlagValue := 8080
	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--db":
			if i+1 < len(os.Args) {
				dbPath = os.Args[i+1]
				i++
			}
		case "--port":
			if i+1 < len(os.Args) {
				if v, err := strconv.Atoi(os.Args[i+1]); err == nil && v > 0 {
					portFlagValue = v
					portFlagGiven = true
				}
				i++
			}
		}
	}

	logFile, _ := os.OpenFile("mbgw_server.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	defer logFile.Close()
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
	log.SetFlags(log.Ldate | log.Ltime)
	log.Println("=== запуск шлюза mbgw (server: единый процесс, конфигурация из БД) ===")

	repo, err := sqliterepo.New(dbPath)
	if err != nil {
		log.Fatalf("[FATAL] ошибка хранилища: %v", err)
	}
	if err := repo.InitSchema(context.Background()); err != nil {
		log.Fatalf("[FATAL] ошибка инициализации схемы: %v", err)
	}
	if err := repo.InitArchiveSchema(context.Background()); err != nil {
		log.Fatalf("[FATAL] ошибка инициализации схемы архива: %v", err)
	}
	if err := repo.InitDeviceConfigSchema(context.Background()); err != nil {
		log.Fatalf("[FATAL] ошибка инициализации схемы конфигурации приборов: %v", err)
	}
	if err := repo.InitAppSettingsSchema(context.Background()); err != nil {
		log.Fatalf("[FATAL] ошибка инициализации схемы настроек: %v", err)
	}
	log.Printf("[OK] Хранилище инициализировано (%s)\n", dbPath)

	// PORT SELECTION RULE: the DB (app_settings.configured_port) is
	// authoritative once a row exists — this is what makes "change the
	// port from the Web UI, restart the service, it actually changes"
	// work, even though the Windows Service's own command line (baked in
	// once by sc.exe at install time, see service_windows.go) may still
	// contain an old --port value from whenever it was installed. The
	// --port flag ONLY seeds the very first bootstrap row, when no
	// settings exist yet in the DB at all. Passing --port on every later
	// run is harmless but ignored once a row exists — this is
	// deliberate, not a bug: without this rule, a UI-driven port change
	// would get silently overwritten back to the service's originally
	// installed --port value on every restart.
	settings, found, err := repo.GetAppSettings(context.Background())
	var configuredPort int
	if !found {
		configuredPort = 8080
		if portFlagGiven {
			configuredPort = portFlagValue
		}
		if err := repo.SetConfiguredPort(context.Background(), configuredPort); err != nil {
			log.Printf("[ERROR] не удалось сохранить начальный порт: %v\n", err)
		}
		log.Printf("[INFO] первый запуск: порт настроен на %d (сохранено в БД)\n", configuredPort)
	} else {
		configuredPort = settings.ConfiguredPort
		if portFlagGiven && portFlagValue != configuredPort {
			log.Printf("[INFO] флаг --port %d проигнорирован — порт уже настроен через БД/UI (%d); чтобы изменить порт, используйте вкладку «Настройки» в /admin\n",
				portFlagValue, configuredPort)
		}
		dbg.Enabled = settings.DebugLogEnabled
	}
	if err != nil {
		log.Printf("[ERROR] не удалось прочитать настройки: %v\n", err)
	}
	log.Printf("[INFO] отладочный лог: %v (переключается на вкладке «Настройки» в /admin)\n", dbg.Enabled)

	// Port-occupied check + automatic fallback: a busy configuredPort
	// (another service, a leftover process, anything already bound to
	// it) must not simply crash the whole gateway on startup — southbound
	// polling and es-sync/northbound delivery are far more important than
	// the diagnostic/admin web UI being reachable on one EXACT port. Scan
	// upward for a free one instead, log it loudly, and persist what was
	// ACTUALLY used so GET /api/settings (and the /admin UI) can show the
	// operator the truth even if it differs from what they configured.
	webPort := findFreePort(configuredPort, 20)
	if webPort != configuredPort {
		log.Printf("[WARN] порт %d занят — сервер запущен на порту %d вместо него. Откройте http://127.0.0.1:%d/admin и при желании смените порт на вкладке «Настройки».\n",
			configuredPort, webPort, webPort)
	}
	if err := repo.SetActualPort(context.Background(), webPort); err != nil {
		log.Printf("[ERROR] не удалось сохранить фактический порт: %v\n", err)
	}

	// A log line is not enough for the very-first-run case: if the
	// configured port was already occupied and nobody knows the actual
	// port yet, the operator would have to already know to go dig
	// through mbgw_server.log to find out where the UI even is — not
	// something a "product", as opposed to a debugging session, should
	// require. Write the live address to a small, fixed-name file next
	// to the exe every time the process starts, so "where's the UI?" is
	// always answerable by opening ONE known file, regardless of whether
	// this is the first run, a port-conflict fallback, or a deliberate
	// port change from Settings.
	addrFile := "mbgw_web_address.txt"
	addrLine := fmt.Sprintf("http://127.0.0.1:%d/admin\n", webPort)
	if err := os.WriteFile(addrFile, []byte(addrLine), 0644); err != nil {
		log.Printf("[ERROR] не удалось записать %s: %v\n", addrFile, err)
	} else {
		log.Printf("[INFO] адрес веб-интерфейса записан в %s (рядом с exe)\n", addrFile)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	webServer := web.NewServer(repo, webPort)

	leaseMgr := lease.New()
	eventBus := monitor.NewBus(nil)
	sched := scheduler.New(eventBus)
	devices := make(map[string]*device.Device)

	// Devices come from the devices table (UpsertDevice/ListDevices,
	// internal/storage/sqlite/repo_device_config.go) instead of
	// config.yaml's Devices []DeviceConfig. Read ONCE here at startup —
	// there is no hot-reload yet (the not-yet-built Web UI's "add
	// device" action will require a restart of this process to take
	// effect until a future step wires a reload/re-register path; adding
	// that now would be speculative against a UI that doesn't exist yet
	// — see IMPLEMENTATION_BACKLOG.md's general caution against building
	// ahead of a confirmed need).
	deviceRecords, err := repo.ListDevices(context.Background())
	if err != nil {
		log.Fatalf("[FATAL] не удалось прочитать список приборов из БД: %v", err)
	}
	if len(deviceRecords) == 0 {
		log.Println("[INFO] в БД не настроено ни одного прибора — сервер запущен, но опрашивать нечего")
	}

	for _, devRec := range deviceRecords {
		if !devRec.Enabled {
			log.Printf("[INFO] прибор %s отключён (enabled=0) — пропускаю\n", devRec.ID)
			continue
		}

		p, err := profile.Parse(devRec.Profile)
		if err != nil {
			log.Printf("[ERROR] прибор %s: ошибка профиля %s: %v\n", devRec.ID, devRec.Profile, err)
			continue
		}
		sess, err := session.NewFromProfile(p.Session)
		if err != nil {
			log.Printf("[ERROR] неизвестный тип сессии %s: %v\n", p.Session.Type, err)
			continue
		}

		trParams := transport.Params{
			Kind:            transport.Kind(devRec.TransportKind),
			Host:            devRec.Host,
			Port:            devRec.Port,
			COM:             devRec.COM,
			Baudrate:        devRec.Baudrate,
			Parity:          devRec.Parity,
			StopBits:        devRec.StopBits,
			ResponseTimeout: time.Duration(devRec.TimeoutMs) * time.Millisecond,
		}
		tr, err := transport.New(trParams)
		if err != nil {
			log.Printf("[ERROR] прибор %s: не удалось создать транспорт: %v\n", devRec.ID, err)
			continue
		}
		if err := tr.Open(ctx); err != nil {
			log.Printf("[ERROR] прибор %s: не удалось открыть транспорт: %v\n", devRec.ID, err)
			continue
		}
		if err := sess.Open(ctx, tr); err != nil {
			log.Printf("[ERROR] ошибка сессии %s: %v\n", devRec.ID, err)
			continue
		}
		isTCP := devRec.TransportKind == "modbus_tcp"

		unitID := devRec.UnitID
		if unitID == 0 {
			unitID = 1
		}
		reader := pollcore.New(tr, isTCP, uint8(unitID))
		dev := device.New(devRec.ID, p, reader, sess, repo, leaseMgr)
		if devRec.GapScanWindowHours > 0 {
			dev.GapScanWindowHours = devRec.GapScanWindowHours
		}
		devices[devRec.ID] = dev

		// Same blocking-backfill-before-scheduler-register reasoning as
		// run.go/serve.go — see those files' identical comment for the
		// 2026-07-29 incident this order avoids.
		if len(p.Archives) > 0 {
			dev.BackfillArchives(ctx, device.BackfillOptions{
				MaxDepthHours: devRec.BackfillMaxDepthHours,
			})
		}

		archiveInterval := time.Duration(0)
		if len(p.Archives) > 0 {
			archiveInterval = 1 * time.Hour
		}
		currentInterval := time.Duration(devRec.CurrentPollSeconds) * time.Second
		if devRec.CurrentPollSeconds <= 0 {
			currentInterval = 3600 * time.Second
		}
		archiveAtMinute := devRec.ArchiveAtMinute
		if archiveAtMinute < 0 {
			archiveAtMinute = 5
		}
		sched.RegisterWithArchiveAnchor(devRec.ID, currentInterval, archiveInterval, nil, archiveAtMinute)
		log.Printf("[OK] прибор %s (%s) зарегистрирован (текущие каждые %s, архив каждые %s в HH:%02d)\n",
			devRec.ID, devRec.Kind, currentInterval, archiveInterval, archiveAtMinute)

		// Per-kind upstream delivery, started right after the device is
		// registered for southbound polling — same "additive, opt-in per
		// row present in the DB" principle as serve()'s
		// cfg.NorthboundAkron check.
		switch devRec.Kind {
		case "akron":
			startAkronNorthboundForDevice(ctx, repo, devRec.ID)
		case "vkm360":
			startESyncForDevice(ctx, repo, devRec.ID)
		}
	}

	webServer.SetManualPoll(func() {
		log.Printf("[WEB] ручной опрос запрошен для %d прибор(ов)\n", len(devices))
		for id, d := range devices {
			sched.RequestManualPoll(id, scheduler.KindCurrent)
			if len(d.Profile.Archives) > 0 {
				sched.RequestManualPoll(id, scheduler.KindBackfill)
			}
		}
	})
	go webServer.Start(ctx)

	pl := poller.New(sched, devices, 1*time.Second)
	go pl.Run(ctx)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("=== получен сигнал завершения. остановка... ===")
}

// findFreePort returns preferred if it's currently bindable, or the
// first free port in (preferred, preferred+span] otherwise. Checks by
// briefly binding and immediately closing a TCP listener — the same
// check-then-use pattern has an inherent (tiny, accepted) race if
// something else grabs the port between the check and the real
// server starting, same as any "is this port free" check anywhere.
func findFreePort(preferred, span int) int {
	if isPortFree(preferred) {
		return preferred
	}
	for p := preferred + 1; p <= preferred+span; p++ {
		if isPortFree(p) {
			return p
		}
	}
	// Every candidate in the scanned range was occupied — extremely
	// unlikely in practice, but return the original rather than 0 so the
	// caller's error path (a real bind failure from ListenAndServe) is
	// what the operator sees, with a sensible port number in the log.
	return preferred
}

func isPortFree(port int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// startAkronNorthboundForDevice starts the Akron raw-RTU northbound
// carrier for one device, IF a listen address has been configured for it
// (es_akron_northbound row) — mirrors serve()'s cfg.NorthboundAkron
// opt-in check, just sourced from the DB instead of config.yaml. Runs in
// its own goroutine; a missing/misconfigured address logs and skips
// rather than failing the whole server startup, so one bad device
// doesn't take down polling for every other device.
func startAkronNorthboundForDevice(ctx context.Context, repo *sqliterepo.Repo, deviceID string) {
	addr, found, err := repo.GetAkronNorthboundAddr(ctx, deviceID)
	if err != nil {
		log.Printf("[ERROR] прибор %s: ошибка чтения адреса northbound: %v\n", deviceID, err)
		return
	}
	if !found || addr == "" {
		log.Printf("[INFO] прибор %s: адрес northbound не настроен — carrier не запущен\n", deviceID)
		return
	}

	logPath := deviceID + "_akron_live.jsonl"
	dlog, err := northbound.NewDiscoveryLog(logPath)
	if err != nil {
		log.Printf("[ERROR] прибор %s: northbound: %v\n", deviceID, err)
		return
	}

	srv := northbound.NewAkronLiveServer(addr, dlog, repo, deviceID)
	go func() {
		defer dlog.Close()
		log.Printf("[OK] northbound (Akron carrier) для %s: слушаем %s (лог: %s)\n", deviceID, addr, logPath)
		if err := srv.Listen(ctx); err != nil {
			log.Printf("[ERROR] northbound (Akron carrier) %s: %v\n", deviceID, err)
		}
	}()
}

// startESyncForDevice starts the ВКМ→Энергосфера direct-DB sync loop for
// one device, IF the operator has configured both the SQL Server
// connection (es_connection) AND at least one channel mapping
// (es_vkm_channels) for it — same "opt-in, missing config = skip with a
// log line, not a fatal error" principle as the Akron branch above.
func startESyncForDevice(ctx context.Context, repo *sqliterepo.Repo, deviceID string) {
	conn, found, err := repo.GetESConnection(ctx)
	if err != nil {
		log.Printf("[ERROR] прибор %s: ошибка чтения параметров подключения к БД ЭС: %v\n", deviceID, err)
		return
	}
	if !found {
		log.Printf("[INFO] прибор %s: подключение к БД ЭС не настроено — es-sync не запущен\n", deviceID)
		return
	}

	channels, err := repo.GetVKMChannels(ctx, deviceID)
	if err != nil {
		log.Printf("[ERROR] прибор %s: ошибка чтения каналов ЭС: %v\n", deviceID, err)
		return
	}
	if len(channels) == 0 {
		log.Printf("[INFO] прибор %s: каналы ЭС не настроены — es-sync не запущен\n", deviceID)
		return
	}

	cfg := integration.Config{
		SQLServer:   conn.SQLServer,
		SQLDatabase: conn.SQLDatabase,
		SQLUser:     conn.SQLUser,
		SQLPassword: conn.SQLPassword,
		SQLPort:     conn.SQLPort,
		DeviceID:    deviceID,
		Pipe:        1,
	}
	for _, ch := range channels {
		factor := ch.Factor
		if factor == 0 {
			factor = 1.0
		}
		switch ch.Tag {
		case "ST":
			cfg.ChanHeat, cfg.FactorHeat = ch.ESChannelID, factor
		case "S":
			cfg.ChanMass, cfg.FactorMass = ch.ESChannelID, factor
		case "T":
			cfg.ChanTemp, cfg.FactorTemp = ch.ESChannelID, factor
		case "Pi":
			cfg.ChanPressure, cfg.FactorPressure = ch.ESChannelID, factor
		}
	}
	cfg.IntervalSec = 60
	cfg.BackfillHours = 168

	go func() {
		log.Printf("[OK] es-sync для %s: старт (сервер БД ЭС=%s, база=%s)\n", deviceID, conn.SQLServer, conn.SQLDatabase)
		// NOTE: RunEnergosphereSync currently opens its OWN
		// mbgw_vkm.db/repo connection internally rather than sharing this
		// process's repo — matches its existing signature
		// (RunEnergosphereSync(ctx, sqlitePath, cfg)) unchanged from the
		// standalone `mbgw es-sync` command, so as not to touch a module
		// that is already confirmed working in production. Sharing the
		// connection is a reasonable future cleanup, not required for
		// this step to work.
		if err := integration.RunEnergosphereSync(ctx, "mbgw_vkm.db", cfg); err != nil {
			log.Printf("[ERROR] es-sync %s: %v\n", deviceID, err)
		}
	}()
}
