package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
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
	"mbgw/internal/protocol/akron"
	"mbgw/internal/scheduler"
	"mbgw/internal/session"
	"mbgw/internal/storage"
	sqliterepo "mbgw/internal/storage/sqlite"
	"mbgw/internal/transport"
	"mbgw/internal/web"
)

// collectAkronPassport делает один короткий обмен командой 101
// (идентификация) через УЖЕ ОТКРЫТЫЙ транспорт прибора и сохраняет
// результат (заводской номер, тип, версия прошивки) в БД как паспорт —
// см. подробное объяснение в месте вызова, в основном цикле регистрации
// приборов выше. Ошибка здесь НЕ прерывает запуск сервера и не мешает
// опросу самого прибора (текущие значения/архив всё равно будут
// работать) — только карьер для ЭС не сможет ответить на 101 без
// паспорта, о чём и так будет видно по логу самого carrier'а.
//
// Формат версии прошивки: строка "мажор.минор" (например "3.7"), СТАРШИЙ
// нибл байта = мажорная версия, младший = минорная — именно так, в
// обратную сторону, её потом собирает akron_live.go при ответе ЭС
// (firmwareToBCD: byte(maj<<4 | min)). Обычное BCD-разложение числа
// (десятки+единицы через codec.DecodeBCDByte) здесь НЕ подходит — даёт
// другое число (0x37 → «37», а не «3.7»), это не то же самое.
func collectAkronPassport(ctx context.Context, repo *sqliterepo.Repo, deviceID string, reader *pollcore.Reader) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := reader.Transact(probeCtx, akron.BuildIdentificationPDU())
	if err != nil {
		log.Printf("[ERROR] прибор %s: не удалось получить паспорт (команда 101): %v\n", deviceID, err)
		return
	}
	_, data, err := akron.ParseResponsePDU(resp)
	if err != nil || len(data) < 6 {
		log.Printf("[ERROR] прибор %s: не удалось разобрать ответ идентификации: %v\n", deviceID, err)
		return
	}

	devType := data[0]
	fwBCD := data[1]
	firmware := fmt.Sprintf("%d.%d", fwBCD>>4, fwBCD&0x0F)
	serial := uint32(data[2]) | uint32(data[3])<<8 | uint32(data[4])<<16 | uint32(data[5])<<24

	err = repo.SaveDevicePassport(ctx, storage.DevicePassport{
		DeviceID:   deviceID,
		Serial:     serial,
		DeviceType: devType,
		Firmware:   firmware,
		UpdatedAt:  time.Now(),
	})
	if err != nil {
		log.Printf("[ERROR] прибор %s: не удалось сохранить паспорт: %v\n", deviceID, err)
		return
	}
	log.Printf("[OK] прибор %s: паспорт собран (заводской №%d, тип=%d, прошивка=%s)\n", deviceID, serial, devType, firmware)
}

// nextToExe resolves a bare filename (e.g. "mbgw_server.db") to a path
// next to the running executable, so every file this process creates
// (database, log, web-address marker) lands in whatever folder the
// operator installed the exe into — NOT wherever the process happened to
// be started from. This matters specifically for the Windows Service
// case: a service's working directory is not guaranteed to be the exe's
// own folder, so relying on relative paths + cwd was fragile. If a
// caller passes an ALREADY-absolute or already-directory-qualified path
// (e.g. --db D:\somewhere\custom.db), that explicit choice is respected
// as-is — this only fills in a directory for a bare filename.
//
// Practical effect: install mbgw_vkm.exe into its own folder (e.g.
// C:\mbgw\), and every file it creates (mbgw_server.db, mbgw_server.log,
// mbgw_web_address.txt) appears right there next to it — no scattered
// files in C:\, no extra flags to remember, regardless of how the
// process is launched (double-click, PowerShell from any directory, or
// as a Windows Service).
func nextToExe(name string) string {
	if filepath.IsAbs(name) || filepath.Dir(name) != "." {
		return name // caller gave an explicit path — leave it alone
	}
	exePath, err := os.Executable()
	if err != nil {
		return name // fall back to cwd-relative if we can't even find ourselves
	}
	exeDir, err := filepath.Abs(filepath.Dir(exePath))
	if err != nil {
		return name
	}
	return filepath.Join(exeDir, name)
}

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
//	mbgw server
//	mbgw server --db mbgw_server.db --port 8080
//
// --db and --port are both optional (defaults: mbgw_server.db, port
// 8080). A bare filename for --db (no directory) resolves next to the
// executable (see nextToExe) — same for the log file and the
// mbgw_web_address.txt marker this writes at startup. Practical effect:
// install the exe into its OWN folder (e.g. C:\mbgw\, not C:\ directly)
// and every file this process creates appears right there, regardless of
// how it's launched.
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

	dbPath = nextToExe(dbPath)

	logFile, _ := os.OpenFile(nextToExe("mbgw_server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
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
	addrFile := nextToExe("mbgw_web_address.txt")
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
	// deviceKinds хранит тип каждого прибора (akron/vkm360) отдельно от
	// devices — нужно колбэку принудительного переопроса (ниже), чтобы
	// решить, какой именно метод вызывать (ForceReloadAkronHourly или
	// ForceReloadVKMHourly), сам *device.Device своего "типа" не хранит.
	deviceKinds := make(map[string]string)

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

		retries := devRec.Retries
		if retries <= 0 {
			retries = 3 // matches devices table's DEFAULT and protocol/modbus/core.go's own fallback expectation
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
			Retries:         retries,
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

		// Сбор паспорта прибора (заводской номер, тип, версия прошивки) —
		// ОБЯЗАТЕЛЬНЫЙ шаг для Akron перед запуском приёма данных для ЭС:
		// northbound-эмулятор (internal/northbound/akron_live.go) отвечает
		// на команду идентификации (101) ТОЛЬКО если паспорт уже сохранён
		// в БД — иначе молча игнорирует запрос, и ЭС никогда не проходит
		// дальше первого шага опроса (см. живой лог 2026-08-23: ЭС раз за
		// разом шлёт 101, carrier печатает «паспорт ещё не собран — на 101
		// молчим»). Раньше паспорт собирала отдельная ручная утилита
		// (akronread --save-passport); в едином процессе server этот шаг
		// нужно делать здесь, автоматически, при каждой регистрации
		// Akron-прибора — используя уже открытый транспорт, без отдельного
		// подключения.
		if devRec.Kind == "akron" {
			collectAkronPassport(ctx, repo, devRec.ID, reader)
		}

		dev := device.New(devRec.ID, p, reader, sess, repo, leaseMgr)
		if devRec.GapScanWindowHours > 0 {
			dev.GapScanWindowHours = devRec.GapScanWindowHours
		}
		devices[devRec.ID] = dev
		deviceKinds[devRec.ID] = devRec.Kind

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
			startESyncForDevice(ctx, repo, devRec.ID, dbPath)
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

	// Принудительный переопрос архива с UI (см. internal/device/
	// akron_reload.go, internal/device/vkm_reload.go и internal/web/
	// api_reload.go) — идёт НАПРЯМУЮ к конкретному прибору, в обход
	// планировщика: это разовое, явно запрошенное оператором действие с
	// указанным периодом, а не часть обычного расписания опроса. Работает
	// для обоих типов приборов — какой метод вызвать, решаем по
	// deviceKinds.
	webServer.SetForceReload(func(deviceID string, from, to time.Time) (int, error) {
		dev, ok := devices[deviceID]
		if !ok {
			return 0, fmt.Errorf("прибор %s не найден среди работающих (сохранён ли он и запущен ли server?)", deviceID)
		}
		kind := deviceKinds[deviceID]
		log.Printf("[WEB] принудительный переопрос архива %s (%s) с %s по %s\n",
			deviceID, kind, from.Format("02.01.2006 15:04"), to.Format("02.01.2006 15:04"))
		switch kind {
		case "akron":
			return dev.ForceReloadAkronHourly(ctx, from)
		case "vkm360":
			return dev.ForceReloadVKMHourly(ctx, from, to)
		default:
			return 0, fmt.Errorf("принудительный переопрос не реализован для типа прибора %q", kind)
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
// startESyncForDevice starts the ВКМ→Энергосфера direct-DB sync loop for
// one device, IF the operator has configured both the SQL Server
// connection (es_connection) AND at least one channel mapping
// (es_vkm_channels) for it — same "opt-in, missing config = skip with a
// log line, not a fatal error" principle as the Akron branch above.
//
// dbPath — путь к ЕДИНОЙ базе процесса server (та же, что открыта в
// runServer как repo), а не отдельный "mbgw_vkm.db". Раньше здесь стоял
// захардкоженный "mbgw_vkm.db" — рабочий путь в старой схеме "четыре
// окна", где southbound ВКМ реально писал в отдельный файл с этим именем.
// В единой базе server всё (включая archive_vkm_raw) пишется в ОДИН
// файл, путь к которому передаётся через --db при запуске — es-sync
// обязан читать оттуда же, иначе получает "no such table: archive_vkm_raw"
// (подтверждено живьём, 2026-08-23).
func startESyncForDevice(ctx context.Context, repo *sqliterepo.Repo, deviceID, dbPath string) {
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
		if err := integration.RunEnergosphereSync(ctx, dbPath, cfg); err != nil {
			log.Printf("[ERROR] es-sync %s: %v\n", deviceID, err)
		}
	}()
}
