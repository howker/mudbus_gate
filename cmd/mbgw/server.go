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
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"mbgw/internal/dbg"
	"mbgw/internal/device"
	"mbgw/internal/integration"
	"mbgw/internal/lease"
	"mbgw/internal/logbuf"
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
	// Файл лога открываем ПЕРВЫМ делом, даже раньше проверки на двойной
	// запуск ниже (переставлено 2026-08-30, найдено оператором живьём):
	// если отказ в запуске случится у СЛУЖБЫ (без консоли, вывод в
	// stderr никто не увидит), причина должна остаться видна хотя бы в
	// mbgw_server.log — иначе диагностировать отказ можно было бы
	// только у процесса, запущенного вручную в открытой консоли, а это
	// именно тот случай, где нам нужнее всего понять причину, раз служба
	// работает без присмотра. Сам путь к файлу лога ("mbgw_server.log"
	// рядом с exe) не зависит ни от --db, ни от других флагов — их
	// разбор можно спокойно оставить ниже.
	//
	// rotatingFile (см. rotating_log.go) — раньше здесь был обычный
	// os.OpenFile с O_APPEND, растущий БЕСКОНЕЧНО без единого ограничения
	// на размер, пока процесс работает — найдено оператором живьём
	// (2026-08-29): mbgw_server.log и *_akron_live.jsonl на проде растут
	// без остановки, потому что перезапуск сервера (единственный момент,
	// когда файл раньше начинал расти "с нуля" — при старом os.OpenFile
	// он всё равно ДОПИСЫВАЛ поверх старого через O_APPEND, так что даже
	// перезапуск не помогал) происходит редко и не по расписанию.
	// 20 МБ на файл, храним последние 10 архивов — с запасом хватает на
	// много дней работы для диагностики, не давая диску заполниться при
	// долгой непрерывной работе без перезапуска.
	logWriter, err := newRotatingFile(nextToExe("mbgw_server.log"), 20*1024*1024, 10)
	if err != nil {
		log.Fatalf("[FATAL] не удалось открыть файл лога: %v", err)
	}
	defer logWriter.Close()
	// logbuf.Writer{} — третий получатель лога, наравне с os.Stdout и
	// файлом: кольцевой буфер в памяти для вкладки «Лог» в /admin
	// (добавлено 2026-08-30, прямой запрос оператора). См. internal/
	// logbuf/logbuf.go — не пишет на диск, переживать перезапуск ему
	// не нужно, для полной истории есть сам mbgw_server.log.
	multiWriter := io.MultiWriter(os.Stdout, logWriter, logbuf.Writer{})
	log.SetOutput(multiWriter)
	log.SetFlags(log.Ldate | log.Ltime)
	log.Println("=== запуск шлюза mbgw (server: единый процесс, конфигурация из БД) ===")

	// Защита от двойного запуска — сразу после открытия лога, до
	// регистрации в диспетчере служб и уж тем более до регистрации
	// приборов (добавлено 2026-08-30, прямой запрос оператора). Теперь,
	// когда появилось ДВА способа запустить один и тот же процесс
	// (служба Windows и обычный ручной запуск), нужна защита от
	// случайного одновременного запуска обоих — иначе оба экземпляра
	// начали бы опрашивать одни и те же приборы параллельно, мешая друг
	// другу физически на линии связи, плюс конфликт за порт
	// веб-интерфейса и одновременная запись в один и тот же файл лога/БД.
	// См. single_instance_windows.go — именованный Windows-мьютекс,
	// освобождается автоматически при завершении процесса-владельца.
	//
	// ИЗМЕНЕНО (2026-08-30, найдено оператором живьём): раньше при
	// ОШИБКЕ проверки (не при подтверждённом конфликте, а именно при
	// сбое самой проверки) код продолжал запуск БЕЗ защиты — с одной
	// лишь строкой предупреждения в логе. Ровно так и произошло на
	// практике: мьютекс, созданный службой (LocalSystem, изолированная
	// Session 0), не давал доступа ручному процессу администратора
	// (другая сессия/учётная запись) без явного дескриптора
	// безопасности — CreateMutex падал с "Access is denied", защита
	// молча отключалась, и оба процесса благополучно стартовали
	// параллельно (подтверждено живьём: одновременная регистрация
	// приборов и подключение к ЭС из обоих). Первопричина (отсутствие
	// дескриптора безопасности) исправлена в single_instance_windows.go
	// — но раз mbgw пишет в финансово значимую базу учёта (ЭС), для
	// ЛЮБОЙ ошибки самой проверки теперь выбран более строгий отказ:
	// лучше видимый, понятный простой (служба явно не стартует,
	// STATE виден в sc query, оператор идёт смотреть лог), чем
	// невидимая порча данных из-за незамеченной строки в логе при
	// автоматическом перезапуске без присмотра (например, после
	// планового обновления Windows).
	if ok, err := acquireSingleInstanceLock(); err != nil {
		log.Printf("[FATAL] не удалось проверить, не запущен ли уже другой экземпляр mbgw: %v\n", err)
		log.Println("        Это защита от конфликта двух процессов — раз проверить не удалось, безопаснее отказать в запуске, чем рискнуть двойным опросом приборов и порчей данных в ЭС.")
		os.Exit(1)
	} else if !ok {
		log.Println("[FATAL] mbgw.exe уже запущен (службой или вручную) — второй экземпляр не может стартовать одновременно с первым.")
		log.Println("        Остановите уже работающий процесс (вкладка «Служба» в /admin, либо sc stop mbgw_service, либо Ctrl+C в его консоли), прежде чем запускать снова.")
		os.Exit(1)
	}

	// Проверка/регистрация в диспетчере управления службами Windows —
	// ДОЛЖНА идти до открытия БД и уж тем более до регистрации
	// приборов (добавлено 2026-08-30, найдено оператором живьём: без
	// этого "sc start mbgw_service" падал с ошибкой 1053, "служба не
	// ответила на запрос своевременно" — SCM ждёт подтверждение "я
	// запущен" в течение ограниченного времени, а регистрация приборов,
	// как мы уже видели на живом примере с прибором osmos, может идти
	// очень долго). Если процесс запущен НЕ как служба (обычный ручной
	// запуск) — serviceStop будет nil, и весь код ниже ведёт себя ровно
	// как раньше, без единого изменения.
	serviceStop := runServerAsWindowsServiceIfApplicable()

	// webStop закрывается кнопкой «Остановить» на вкладке «Служба» в
	// /admin, КОГДА процесс запущен НЕ как служба Windows (обычный
	// ручной запуск) — добавлено 2026-08-30. Для случая "мы запущены
	// службой" веб-кнопка идёт другим, более правильным путём — через
	// SCM (см. stopSelfAsWindowsService в service_run_windows.go),
	// который в итоге закрывает serviceStop выше, а не webStop; этот
	// канал нужен именно для случая, когда никакого SCM вообще нет и
	// закрывать больше нечего, кроме как напрямую.
	webStop := make(chan struct{})

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
	// esSyncTriggers хранит канал внепланового запуска es-sync для
	// каждого прибора ВКМ (см. startESyncForDevice) — используется
	// кнопкой «Синхронизировать сейчас» и «Принудительным переопросом»,
	// чтобы новые данные появлялись в ЭС по ходу сбора, не дожидаясь
	// обычного часового цикла (добавлено 2026-08-27).
	esSyncTriggers := make(map[string]chan struct{})
	// devicesMu защищает три карты выше (devices/deviceKinds/
	// esSyncTriggers) от одновременного чтения и записи — НУЖНО именно
	// потому, что теперь веб-сервер запускается (см. go webServer.Start
	// ниже) ДО того, как цикл регистрации приборов их заполнит, а не
	// после, как было раньше. Без мьютекса это была бы гонка данных
	// (конкурентное чтение/запись обычной Go map — не просто гонка, а
	// потенциальный runtime-краш "concurrent map read and map write"),
	// если оператор откроет /admin и нажмёт что-то, использующее эти
	// карты (например, «Принудительный переопрос»), пока цикл ниже ещё
	// регистрирует следующий прибор.
	var devicesMu sync.Mutex

	// ИЗМЕНЕНО (2026-08-29, найдено оператором живьём): раньше
	// webServer.Start запускался В САМОМ КОНЦЕ этой функции, ПОСЛЕ всего
	// цикла регистрации приборов ниже — а внутри цикла для каждого
	// прибора СИНХРОННО, блокирующе выполняется dev.BackfillArchives
	// (дозабор при старте). Если у одного прибора дозабор долго и
	// безуспешно ломится (например, физически неисправный датчик —
	// каждый из сотен периодов не даёт данных, но всё равно требует
	// полного цикла запрос/ожидание/таймаут) — /admin был недоступен
	// ВООБЩЕ до конца всего цикла, включая ВСЕ остальные приборы после
	// проблемного. Запуск здесь, ДО цикла, устраняет эту зависимость:
	// /admin отвечает сразу, независимо от того, сколько времени займёт
	// дозабор любого количества приборов ниже.
	//
	// Порядок «дозабор блокирует ДО регистрации ЭТОГО ЖЕ прибора в
	// планировщике» (см. dev.BackfillArchives внутри цикла ниже) НЕ
	// затронут этим изменением — это два независимых момента: ЭТОТ
	// перенос касается только времени запуска HTTP-listener'а, не
	// порядка операций внутри цикла для одного прибора (см. комментарий
	// у dev.BackfillArchives ниже про инцидент 2026-07-29 — та гонка
	// была между дозабором и ПЛАНОВЫМ опросом ОДНОГО И ТОГО ЖЕ прибора
	// за lease, никак не связана с моментом запуска веб-сервера).
	go webServer.Start(ctx)

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

	// ИЗМЕНЕНО (2026-08-30, найдено оператором живьём): раньше приборы
	// регистрировались ПОСЛЕДОВАТЕЛЬНО, один за другим, в одном простом
	// for-цикле — а внутри каждой итерации дозабор (dev.BackfillArchives)
	// блокирующий. Если у ОДНОГО прибора дозабор идёт очень долго
	// (реальный случай: прибор "osmos" упёрся в длинный "мёртвый"
	// участок кольцевого буфера — записи с нечитаемой/нулевой датой,
	// которые НЕ считаются ни закрытием пропуска, ни концом архива, см.
	// backfillAkronHourly в internal/device/backfill.go, — и продолжал
	// методично перемалывать мусорные страницы) — КАЖДЫЙ прибор, идущий
	// в списке ПОСЛЕ него, физически не доходил до
	// sched.RegisterWithArchiveAnchor и потому не опрашивался вообще,
	// хотя сам процесс был жив и /admin отвечал (тот фикс уже сделан
	// раньше, 2026-08-29 — но он решил только доступность UI, не эту,
	// более глубокую проблему).
	//
	// Теперь регистрация каждого прибора идёт в СВОЕЙ горутине —
	// застрявший/медленный дозабор одного прибора больше не блокирует
	// вообще ничего для остальных, независимо от причины (мусор в
	// буфере, физически неисправный датчик, недоступный по сети прибор
	// и т.п.). Порядок ВНУТРИ одного прибора (дозабор ДО регистрации
	// ЭТОГО ЖЕ прибора в планировщике) не изменился — это по-прежнему
	// нужно, чтобы избежать инцидента 2026-07-29 (гонка дозабора с
	// плановым опросом ЗА ТОТ ЖЕ lease). wg.Wait() ниже гарантирует, что
	// poller.New(sched, devices, ...) получит карту devices только
	// после того, как ВСЕ горутины закончат в неё писать — без этого
	// была бы гонка данных на самой карте.
	var wg sync.WaitGroup
	for _, devRec := range deviceRecords {
		if !devRec.Enabled {
			log.Printf("[INFO] прибор %s отключён (enabled=0) — пропускаю\n", devRec.ID)
			continue
		}
		devRec := devRec // захват переменной цикла для горутины (go1.20 ещё требует явно)
		wg.Add(1)
		go func() {
			defer wg.Done()
			registerOneDevice(ctx, repo, devRec, leaseMgr, sched, dbPath, &devicesMu, devices, deviceKinds, esSyncTriggers)
		}()
	}
	wg.Wait()

	webServer.SetManualPoll(func() {
		// Снимок карты под мьютексом, а не итерация по ней напрямую —
		// сама итерация тоже требует блокировки на всё время цикла, а
		// это лишняя задержка HTTP-обработчика ради, по сути, короткой
		// операции (RequestManualPoll — быстрый неблокирующий вызов).
		devicesMu.Lock()
		snapshot := make(map[string]*device.Device, len(devices))
		for id, d := range devices {
			snapshot[id] = d
		}
		devicesMu.Unlock()

		log.Printf("[WEB] ручной опрос запрошен для %d прибор(ов)\n", len(snapshot))
		for id, d := range snapshot {
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
	//
	// Первый параметр — jobCtx, а не общий ctx процесса: веб-слой создаёт
	// СВОЙ, отменяемый контекст на каждый запуск переопроса (см.
	// api_reload.go), чтобы оператор мог прервать конкретный переопрос
	// кнопкой «Отменить», не влияя на остальной процесс (добавлено
	// 2026-08-27 — раньше общий ctx делал это в принципе невозможным).
	webServer.SetForceReload(func(jobCtx context.Context, deviceID string, from, to time.Time, onProgress func(done, total int)) (int, error) {
		devicesMu.Lock()
		dev, ok := devices[deviceID]
		kind := deviceKinds[deviceID]
		devicesMu.Unlock()
		if !ok {
			return 0, fmt.Errorf("прибор %s не найден среди работающих (сохранён ли он и запущен ли server? если прибор добавлен только что — дождитесь окончания стартовой регистрации всех приборов)", deviceID)
		}
		log.Printf("[WEB] принудительный переопрос архива %s (%s) с %s по %s\n",
			deviceID, kind, from.Format("02.01.2006 15:04"), to.Format("02.01.2006 15:04"))
		switch kind {
		case "akron":
			return dev.ForceReloadAkronHourly(jobCtx, from, onProgress)
		case "vkm360":
			return dev.ForceReloadVKMHourly(jobCtx, from, to, onProgress)
		default:
			return 0, fmt.Errorf("принудительный переопрос не реализован для типа прибора %q", kind)
		}
	})

	// «Синхронизировать сейчас» — просит уже работающий цикл es-sync
	// конкретного прибора сделать внеплановый проход немедленно, не
	// дожидаясь часового тикера (добавлено 2026-08-27). Неблокирующая
	// отправка в буферизованный канал — если проход уже "заказан" и ещё
	// не обработан, повторный клик просто ничего не делает, не копит
	// очередь.
	webServer.SetSyncNow(func(deviceID string) error {
		devicesMu.Lock()
		trigger, ok := esSyncTriggers[deviceID]
		devicesMu.Unlock()
		if !ok {
			return fmt.Errorf("для прибора %s es-sync не запущен (проверьте подключение к ЭС и каналы, либо дождитесь окончания стартовой регистрации приборов)", deviceID)
		}
		select {
		case trigger <- struct{}{}:
		default:
		}
		return nil
	})

	// Принудительная пересинхронизация с ЭС (см. internal/web/
	// api_es_resync.go, internal/integration/energosphere_sync.go's
	// ForceResyncRange) — добавлено 2026-08-30, прямой запрос
	// оператора: "бывает что с прибора попали искажённые данные и нужно
	// переопросить прибор и чтобы новые данные попали в эс". В отличие
	// от SetSyncNow выше (просит уже работающий цикл поторопиться со
	// «только новым»), здесь ОТКРЫВАЕТСЯ ОТДЕЛЬНОЕ подключение к БД ЭС
	// на время самой операции (не переиспользует подключение
	// работающего es-sync цикла) — это редкое, явно запрошенное
	// оператором действие с указанным диапазоном, а не часть обычного
	// расписания, так что отдельное подключение проще и не рискует
	// помешать штатному циклу синхронизации.
	webServer.SetForceResyncES(func(ctx context.Context, deviceID string, from, to time.Time) (updated, inserted, failed int, err error) {
		devicesMu.Lock()
		kind := deviceKinds[deviceID]
		devicesMu.Unlock()
		// ИЗМЕНЕНО (2026-08-31): раньше работало только для ВКМ — теперь
		// оба типа приборов пишут в ЭС одним и тем же путём
		// (buildIntegrationConfig сам умеет и в тот, и в другой), так что
		// ограничение убрано; неизвестный/пустой kind по-прежнему отказ.
		if kind != "vkm360" && kind != "akron" {
			return 0, 0, 0, fmt.Errorf("принудительная пересинхронизация с ЭС не поддерживается для типа прибора %q (прибор %s)", kind, deviceID)
		}

		cfg, found, cerr := buildIntegrationConfig(ctx, repo, deviceID, kind)
		if cerr != nil {
			return 0, 0, 0, cerr
		}
		if !found {
			return 0, 0, 0, fmt.Errorf("для прибора %s не настроено подключение к ЭС или точки", deviceID)
		}

		writer, werr := integration.OpenPointMainsWriter(integration.SQLServerConfig{
			Server: cfg.SQLServer, Database: cfg.SQLDatabase,
			User: cfg.SQLUser, Password: cfg.SQLPassword, Port: cfg.SQLPort,
		})
		if werr != nil {
			return 0, 0, 0, fmt.Errorf("подключение к БД ЭС: %w", werr)
		}
		defer writer.Close()

		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if perr := writer.Ping(pingCtx); perr != nil {
			return 0, 0, 0, fmt.Errorf("проверка подключения к БД ЭС: %w", perr)
		}

		log.Printf("[WEB] принудительная пересинхронизация с ЭС: прибор %s, %s..%s\n",
			deviceID, from.Format("02.01.2006 15:04"), to.Format("02.01.2006 15:04"))
		return integration.ForceResyncRange(ctx, repo, writer, cfg, from, to)
	})

	// Вкладка «Служба» в /admin (добавлено 2026-08-30, прямой запрос
	// оператора: "автоматизировать в юай" статус/остановку). Работает
	// на любой платформе — queryWindowsServiceStatus/isRunningAsWindowsService
	// имеют заглушки для не-Windows сборок (см. service_run_other.go),
	// возвращающие "нет службы" вместо ошибки компиляции.
	webServer.SetServiceStatus(func() (goos string, runningAsService, serviceInstalled bool, serviceState string) {
		goos = runtime.GOOS
		runningAsService = isRunningAsWindowsService()
		if goos != "windows" {
			return goos, runningAsService, false, ""
		}
		status, err := queryWindowsServiceStatus()
		if err != nil {
			log.Printf("[ERROR] не удалось узнать статус службы: %v\n", err)
			return goos, runningAsService, false, ""
		}
		return goos, runningAsService, status.Installed, status.State
	})

	// «Остановить» на вкладке «Служба» — способ зависит от того, КАК мы
	// сейчас запущены: если службой — команда идёт через SCM (тот же
	// путь, что и "sc stop mbgw_service", попадает в уже проверенный
	// обработчик mbgwServiceHandler.Execute, который закрывает
	// serviceStop выше); если обычным ручным запуском — закрываем
	// webStop напрямую, тем же путём, что и Ctrl+C.
	webServer.SetServiceStop(func() {
		if isRunningAsWindowsService() {
			if err := stopSelfAsWindowsService(); err != nil {
				log.Printf("[ERROR] не удалось остановить службу через SCM: %v\n", err)
			}
			return
		}
		close(webStop)
	})

	pl := poller.New(sched, devices, 1*time.Second)
	go pl.Run(ctx)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sigChan:
		log.Println("=== получен сигнал завершения (Ctrl+C). остановка... ===")
	case <-serviceStop:
		log.Println("=== получен запрос на остановку от диспетчера служб Windows. остановка... ===")
	case <-webStop:
		log.Println("=== получен запрос на остановку через веб-интерфейс. остановка... ===")
	}
}

// registerOneDevice делает всё, что раньше было одной итерацией
// последовательного цикла в runServer: открывает транспорт/сессию,
// собирает паспорт (Akron), выполняет БЛОКИРУЮЩИЙ стартовый дозабор
// архива, регистрирует прибор в планировщике и запускает northbound/
// es-sync. Теперь вызывается в СВОЕЙ горутине на каждый прибор (см.
// комментарий в runServer у wg.Wait()) — ошибка/долгий дозабор одного
// прибора здесь никак не влияет на остальные горутины, вызванные для
// других приборов.
//
// devicesMu защищает devices/deviceKinds/esSyncTriggers — эти три карты
// теперь пишутся ИЗ РАЗНЫХ горутин одновременно (раньше — из одной,
// строго последовательно), так что блокировка обязательна на каждую
// запись, не только ради HTTP-обработчиков, как было раньше.
func registerOneDevice(ctx context.Context, repo *sqliterepo.Repo, devRec sqliterepo.DeviceRecord, leaseMgr *lease.LocalLease, sched *scheduler.Scheduler, dbPath string, devicesMu *sync.Mutex, devices map[string]*device.Device, deviceKinds map[string]string, esSyncTriggers map[string]chan struct{}) {
	p, err := profile.Parse(devRec.Profile)
	if err != nil {
		log.Printf("[ERROR] прибор %s: ошибка профиля %s: %v\n", devRec.ID, devRec.Profile, err)
		return
	}
	sess, err := session.NewFromProfile(p.Session)
	if err != nil {
		log.Printf("[ERROR] неизвестный тип сессии %s: %v\n", p.Session.Type, err)
		return
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
		return
	}
	if err := tr.Open(ctx); err != nil {
		log.Printf("[ERROR] прибор %s: не удалось открыть транспорт: %v\n", devRec.ID, err)
		return
	}
	if err := sess.Open(ctx, tr); err != nil {
		log.Printf("[ERROR] ошибка сессии %s: %v\n", devRec.ID, err)
		return
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
	devicesMu.Lock()
	devices[devRec.ID] = dev
	deviceKinds[devRec.ID] = devRec.Kind
	devicesMu.Unlock()

	// Same blocking-backfill-before-scheduler-register reasoning as
	// run.go/serve.go — see those files' identical comment for the
	// 2026-07-29 incident this order avoids. Долгий дозабор здесь
	// по-прежнему блокирует РЕГИСТРАЦИЮ ЭТОГО прибора в планировщике —
	// это не изменилось и не должно меняться — но теперь блокирует
	// только ЭТУ горутину, не остальные приборы.
	if len(p.Archives) > 0 {
		dev.BackfillArchives(ctx, device.BackfillOptions{
			MaxDepthHours: devRec.BackfillMaxDepthHours,
		})
	}

	// Интервал опроса архива зависит от того, КАК прибор сам делит
	// свой архив на записи — не универсальная константа. ВКМ360
	// физически отдаёт получасовки (см. vkm_hourly.go, vkmArchivePeriod);
	// опрос раз в час (как для Akron, у которого архив честно
	// часовой) СИСТЕМАТИЧЕСКИ терял каждую вторую получасовку — на
	// каждом часовом тике pollVKMHourlyLatest видит только ОДНУ,
	// последнюю завершённую получасовку, а не обе, что успели
	// закрыться с прошлого тика. Подтверждено живьём (2026-08-25):
	// час опроса ловил стабильно только записи на ":30", записи на
	// ":00" не собирались НИКОГДА обычным циклом (только дозабором
	// при старте) — а прибор, судя по всему, копит показания между
	// успешными опросами, из-за чего следующая пойманная получасовка
	// выходила завышенной (несла в себе накопленное за оба
	// пропущенных получаса), а не только за свои 30 минут.
	archiveInterval := time.Duration(0)
	if len(p.Archives) > 0 {
		if devRec.Kind == "vkm360" {
			archiveInterval = 30 * time.Minute
		} else {
			archiveInterval = 1 * time.Hour
		}
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
	// registered for southbound polling.
	//
	// ИЗМЕНЕНО (2026-08-31, прямой запрос оператора: "переделать опрос
	// акрона... сделать также как вкм... убрать как атавизм... опрос
	// через драйвер энергосферы"): раньше Akron получал данные в ЭС
	// СОВСЕМ ДРУГИМ путём — эмуляцией физического прибора для родного
	// драйвера ЭС (startAkronNorthboundForDevice, определена ниже,
	// оставлена в коде НЕЗАКОММЕНТИРОВАННОЙ как функция — но её ВЫЗОВ
	// здесь закомментирован, вдруг понадобится вернуться к этому пути).
	// Теперь оба типа приборов идут через ОДИН общий механизм прямой
	// записи в PointMains (startESyncForDevice) — единственная разница
	// между ними теперь в самом механизме — сколько точек и откуда
	// берутся исходные данные (см. buildIntegrationConfig,
	// internal/integration/energosphere_sync.go).
	switch devRec.Kind {
	case "akron", "vkm360":
		// startAkronNorthboundForDevice(ctx, repo, devRec.ID) — старый
		// путь через эмуляцию прибора для драйвера ЭС, закомментирован,
		// см. пояснение выше.
		if trigger := startESyncForDevice(ctx, repo, devRec.ID, devRec.Kind, dbPath); trigger != nil {
			devicesMu.Lock()
			esSyncTriggers[devRec.ID] = trigger
			devicesMu.Unlock()
		}
	}
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
//
// ОТКЛЮЧЕНО (2026-08-31, прямой запрос оператора: "переделать опрос
// акрона - убрать как атавизм (закомментировать код может когда-то
// понадобится вернуться к опросу через драйвер энергосферы)"). Функция
// НЕ УДАЛЕНА и по-прежнему компилируется — только единственный вызов
// в registerOneDevice закомментирован (ищите "startAkronNorthboundForDevice(ctx,
// repo, devRec.ID) — старый путь" в этом же файле). Akron теперь
// получает данные в ЭС ТЕМ ЖЕ путём, что и ВКМ — прямой записью в
// PointMains через startESyncForDevice, см. buildIntegrationConfig
// выше. Если когда-нибудь понадобится вернуться к эмуляции прибора для
// родного драйвера ЭС — раскомментировать тот один вызов, эта функция
// уже готова к работе как есть.
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
//
// dbPath — путь к ЕДИНОЙ базе процесса server (та же, что открыта в
// runServer как repo), а не отдельный "mbgw_vkm.db". Раньше здесь стоял
// захардкоженный "mbgw_vkm.db" — рабочий путь в старой схеме "четыре
// окна", где southbound ВКМ реально писал в отдельный файл с этим именем.
// В единой базе server всё (включая archive_vkm_raw) пишется в ОДИН
// файл, путь к которому передаётся через --db при запуске — es-sync
// обязан читать оттуда же, иначе получает "no such table: archive_vkm_raw"
// (подтверждено живьём, 2026-08-23).
//
// Возвращает канал-триггер внепланового прохода (см. RunEnergosphereSync)
// — nil, если es-sync для этого прибора не запустился (не настроено
// подключение/каналы). Вызывающий код регистрирует его в общей карте,
// чтобы кнопка «Синхронизировать сейчас» и «Принудительный переопрос»
// могли попросить внеплановый проход, не дожидаясь часового тикера
// (добавлено 2026-08-27).
// buildIntegrationConfig собирает integration.Config из текущих
// настроек в БД (подключение к ЭС + точки для конкретного прибора) —
// вынесено из startESyncForDevice в отдельную функцию (2026-08-30),
// чтобы её же мог переиспользовать колбэк «Принудительной
// пересинхронизации с ЭС» (см. SetForceResyncES ниже): та операция
// должна использовать САМЫЕ СВЕЖИЕ настройки (например, только что
// изменённый множитель точки), а не что-то закэшированное при старте
// процесса — поэтому читает из БД заново при каждом вызове, точно так
// же, как это делает startESyncForDevice при регистрации прибора.
//
// ИЗМЕНЕНО (2026-08-31, прямой запрос оператора: "переделать опрос
// акрона... сделать также как вкм"): теперь работает ОДИНАКОВО для
// обоих типов приборов — принимает kind явным параметром (кладётся в
// cfg.Kind, дальше используется в internal/integration для выбора
// источника исходных данных). Таблица es_vkm_channels в SQLite физически
// НЕ ограничена типом прибора (просто device_id/tag/es_channel_id/
// factor, без привязки к kind — проверено в самой схеме, 2026-08-31),
// так что GetVKMChannels можно смело переиспользовать и для Akron,
// несмотря на «VKM» в названии — переименовывать сам метод/таблицу не
// стали, это внутренняя деталь реализации, не видимая оператору.
//
// found=false означает, что для этого прибора нет ни подключения к ЭС,
// ни настроенных точек — не ошибка, просто «для этого прибора
// интеграция с ЭС не настроена».
func buildIntegrationConfig(ctx context.Context, repo *sqliterepo.Repo, deviceID, kind string) (cfg integration.Config, found bool, err error) {
	conn, connFound, err := repo.GetESConnection(ctx)
	if err != nil {
		return integration.Config{}, false, fmt.Errorf("чтение параметров подключения к БД ЭС: %w", err)
	}
	if !connFound {
		return integration.Config{}, false, nil
	}

	channels, err := repo.GetVKMChannels(ctx, deviceID)
	if err != nil {
		return integration.Config{}, false, fmt.Errorf("чтение точек ЭС: %w", err)
	}
	if len(channels) == 0 {
		return integration.Config{}, false, nil
	}

	cfg = integration.Config{
		SQLServer:   conn.SQLServer,
		SQLDatabase: conn.SQLDatabase,
		SQLUser:     conn.SQLUser,
		SQLPassword: conn.SQLPassword,
		SQLPort:     conn.SQLPort,
		DeviceID:    deviceID,
		Pipe:        1,
		Kind:        kind,
		// Сдвиг метки времени при записи в PointMains — берётся из
		// настроек подключения к ЭС (вкладка «Подключение к ЭС» в
		// /admin), см. ESConnection.TimeShiftMinutes.
		TimeShiftMinutes: conn.TimeShiftMinutes,
	}

	humanLabels := map[string]string{
		"ST": "тепло", "S": "масса", "T": "температура", "Pi": "давление",
		"V": "объём",
	}
	for _, ch := range channels {
		factor := ch.Factor
		if factor == 0 {
			factor = 1.0
		}
		label := humanLabels[ch.Tag]
		if label == "" {
			label = ch.Tag
		}
		cfg.Points = append(cfg.Points, integration.PointMapping{
			Tag: ch.Tag, PointID: ch.ESChannelID, Factor: factor, Label: label,
		})
	}
	cfg.IntervalSec = 60
	// 90 суток (2160 часов), не 168 (7 суток) — с 7 сутками es-sync
	// физически не мог увидеть данные, собранные "Принудительным
	// переопросом" глубже недели назад (подтверждено живьём, 2026-08-27:
	// переопрос ВКМ за месяц завершился, но месячная история в ЭС не
	// появилась — обычный цикл синхронизации просто не заглядывал так
	// далеко назад). Более широкое окно не создаёт лишних записей —
	// проверка на существование точки (PointExists) по-прежнему
	// пропускает уже отправленное, просто теперь реально проверяется
	// более глубокая история, а не отбрасывается ещё до проверки.
	cfg.BackfillHours = 2160

	return cfg, true, nil
}

func startESyncForDevice(ctx context.Context, repo *sqliterepo.Repo, deviceID, kind, dbPath string) chan struct{} {
	cfg, found, err := buildIntegrationConfig(ctx, repo, deviceID, kind)
	if err != nil {
		log.Printf("[ERROR] прибор %s: %v — es-sync не запущен\n", deviceID, err)
		return nil
	}
	if !found {
		log.Printf("[INFO] прибор %s: подключение к БД ЭС или точки не настроены — es-sync не запущен\n", deviceID)
		return nil
	}

	trigger := make(chan struct{}, 1)
	go func() {
		log.Printf("[OK] es-sync для %s (%s): старт (сервер БД ЭС=%s, база=%s)\n", deviceID, kind, cfg.SQLServer, cfg.SQLDatabase)
		if err := integration.RunEnergosphereSync(ctx, dbPath, cfg, trigger); err != nil {
			log.Printf("[ERROR] es-sync %s: %v\n", deviceID, err)
		}
	}()
	return trigger
}
