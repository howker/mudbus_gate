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

	"mbgw/internal/buildinfo"
	"mbgw/internal/codec"
	"mbgw/internal/dbg"
	"mbgw/internal/device"
	"mbgw/internal/devicestatus"
	"mbgw/internal/health"
	"mbgw/internal/integration"
	"mbgw/internal/lease"
	"mbgw/internal/logbuf"
	"mbgw/internal/monitor"
	"mbgw/internal/pollcore"
	"mbgw/internal/poller"
	"mbgw/internal/profile"
	"mbgw/internal/protocol/akron"
	"mbgw/internal/scheduler"
	"mbgw/internal/servicelog"
	"mbgw/internal/session"
	"mbgw/internal/storage"
	sqliterepo "mbgw/internal/storage/sqlite"
	"mbgw/internal/transport"
	"mbgw/internal/web"
)

// collectAkronPassport делает один короткий обмен командой 101
// (идентификация) через Reader. Для serial Reader сам открывает физический
// канал только на время транзакции и сразу освобождает его, затем сохраняет
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

// collectProfileSerialPassport читает заводской номер из профильной точки
// serial_number. Для ИВК-ТЭР это безопасный read-only IR 0x8002; никакого
// перехода прибора в сервисный режим для чтения номера не требуется.
func collectProfileSerialPassport(ctx context.Context, repo *sqliterepo.Repo, deviceID string, p *profile.Profile, reader *pollcore.Reader) {
	if p == nil {
		return
	}
	var serialPoint *profile.Point
	for i := range p.Points {
		pt := &p.Points[i]
		if pt.Name == "serial_number" && pt.Access != "write" && pt.Type == "uint32" {
			serialPoint = pt
			break
		}
	}
	if serialPoint == nil {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := reader.ReadRaw(probeCtx, serialPoint.Space, serialPoint.AddrOrZero(), serialPoint.Type)
	if err != nil {
		log.Printf("[ОШИБКА] прибор %s: не удалось прочитать заводской номер: %v\n", deviceID, err)
		return
	}
	serial, err := codec.DecodeUint32(raw, p.Codec.WordOrder32)
	if err != nil || serial == 0 || serial == 0xFFFFFFFF {
		log.Printf("[ОШИБКА] прибор %s: некорректный заводской номер: %v, значение=%d\n", deviceID, err, serial)
		return
	}
	if err := repo.SaveDevicePassport(ctx, storage.DevicePassport{
		DeviceID: deviceID, Serial: serial, UpdatedAt: time.Now(),
	}); err != nil {
		log.Printf("[ОШИБКА] прибор %s: не удалось сохранить заводской номер: %v\n", deviceID, err)
		return
	}
	log.Printf("[ОК] прибор %s: заводской номер прочитан и сохранён: %d\n", deviceID, serial)
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
// profilePathNextToExe resolves every RELATIVE device-profile path from
// the executable directory, not from the process working directory.
// This is essential for Windows Service mode: SCM commonly starts a
// service with a working directory such as C:\Windows\System32, while
// the DB stores paths like "profiles/vkm360.yaml". Manual console runs
// from the mbgw folder happened to work; the same relative path under
// SCM did not.
func profilePathNextToExe(name string) string {
	if name == "" || filepath.IsAbs(name) {
		return name
	}
	exePath, err := os.Executable()
	if err != nil {
		return name
	}
	exeDir, err := filepath.Abs(filepath.Dir(exePath))
	if err != nil {
		return name
	}
	return filepath.Join(exeDir, name)
}

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
// southbound polling for every enabled device and direct-DB sync to
// Энергосфера for configured VKM/Akron points — so the Web UI
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
	core := serverCoreFunc(runServerCore)
	handled, err := runServerAsWindowsServiceIfApplicable(core)
	if handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка Windows-службы: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := runServerCore(context.Background(), nil, false); err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка МодбасШлюза: %v\n", err)
		os.Exit(1)
	}
}

func runServerCore(parentCtx context.Context, onReady func(), runningAsService bool) error {
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
	// (2026-08-29): mbgw_server.log на проде мог расти без ограничения
	// без остановки, потому что перезапуск сервера (единственный момент,
	// когда файл раньше начинал расти "с нуля" — при старом os.OpenFile
	// он всё равно ДОПИСЫВАЛ поверх старого через O_APPEND, так что даже
	// перезапуск не помогал) происходит редко и не по расписанию.
	// 20 МБ на файл, храним последние 10 архивов — с запасом хватает на
	// много дней работы для диагностики, не давая диску заполниться при
	// долгой непрерывной работе без перезапуска.
	logWriter, err := newRotatingFile(nextToExe("mbgw_server.log"), 20*1024*1024, 10)
	if err != nil {
		return fmt.Errorf("не удалось открыть файл лога: %w", err)
	}
	defer logWriter.Close()
	// logbuf.Writer{} — третий получатель лога, наравне с os.Stdout и
	// файлом: кольцевой буфер в памяти для вкладки «Лог» в /admin
	// (добавлено 2026-08-30, прямой запрос оператора). См. internal/
	// logbuf/logbuf.go — не пишет на диск, переживать перезапуск ему
	// не нужно, для полной истории есть сам mbgw_server.log.
	multiWriter := io.MultiWriter(newNonBlockingWriter(os.Stdout, 512), logWriter, logbuf.Writer{})
	// Все операторские копии (консоль, файл и web-log) получают одинаковые
	// русские служебные метки. Технические идентификаторы не переводятся.
	log.SetOutput(newRussianLogWriter(multiWriter))
	log.SetFlags(log.Ldate | log.Ltime)
	log.Println("=== запуск шлюза mbgw (сервер: единый процесс, конфигурация из БД) ===")
	bi := buildinfo.Current()
	revision := bi.Revision
	if revision == "" {
		revision = "не определён"
	}
	buildTime := bi.BuildTime
	if buildTime == "" {
		buildTime = "не указано"
	}
	log.Printf("[СБОРКА] commit=%s, изменённые исходники=%v, время сборки=%s, Go=%s\n",
		revision, bi.Modified, buildTime, bi.GoVersion)

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
		log.Printf("[КРИТИЧНО] не удалось проверить, не запущен ли уже другой экземпляр mbgw: %v\n", err)
		return fmt.Errorf("защита от двойного запуска: %w", err)
	} else if !ok {
		log.Println("[КРИТИЧНО] mbgw.exe уже запущен (службой или вручную) — второй экземпляр не запускается.")
		return fmt.Errorf("уже запущен другой экземпляр mbgw")
	}

	// В режиме Windows-службы жизненным циклом этого ядра уже владеет
	// Service Handler: он держит SCM в StartPending до вызова onReady и
	// в StopPending до полного возврата этой функции. При ручном запуске
	// используется тот же код, но context принадлежит обычному процессу.

	// webStop закрывается кнопкой «Остановить» на вкладке «Служба» в
	// /admin, КОГДА процесс запущен НЕ как служба Windows (обычный
	// ручной запуск) — добавлено 2026-08-30. Для случая "мы запущены
	// службой" веб-кнопка идёт другим, более правильным путём — через
	// SCM (см. stopSelfAsWindowsService в service_run_windows.go),
	// который отменяет общий context через Service Handler; этот
	// канал нужен именно для случая, когда никакого SCM вообще нет и
	// закрывать больше нечего, кроме как напрямую.
	webStop := make(chan struct{})
	var webStopOnce sync.Once

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

	// Отдельный диагностический журнал службы намеренно не хранится в
	// mbgw_server.db: если основная SQLite БД заблокирована или повреждена,
	// причина сбоя должна остаться доступной оператору. Ошибка открытия
	// этого журнала не запрещает основной опрос.
	serviceLog, serviceLogErr := servicelog.Open(nextToExe("mbgw_service_log.db"))
	if serviceLogErr != nil {
		log.Printf("[ПРЕДУПРЕЖДЕНИЕ] отдельный журнал службы недоступен: %v\n", serviceLogErr)
	} else {
		defer serviceLog.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := serviceLog.Cleanup(cleanupCtx, time.Now().AddDate(0, 0, -30)); err != nil {
			log.Printf("[ПРЕДУПРЕЖДЕНИЕ] не удалось очистить старые записи журнала службы: %v\n", err)
		}
		cleanupCancel()
		writeServiceEvent(serviceLog, "информация", "запуск и остановка", "Запуск процесса МодбасШлюза.")
	}

	repo, err := sqliterepo.New(dbPath)
	if err != nil {
		writeServiceEvent(serviceLog, "ошибка", "запуск и остановка", "Не удалось открыть основную БД МодбасШлюза: "+err.Error())
		return fmt.Errorf("ошибка хранилища: %w", err)
	}
	defer repo.Close()
	if err := repo.InitSchema(context.Background()); err != nil {
		return fmt.Errorf("ошибка инициализации схемы: %w", err)
	}
	if err := repo.InitArchiveSchema(context.Background()); err != nil {
		return fmt.Errorf("ошибка инициализации схемы архива: %w", err)
	}
	if err := repo.InitDeviceConfigSchema(context.Background()); err != nil {
		return fmt.Errorf("ошибка инициализации схемы конфигурации приборов: %w", err)
	}
	if err := repo.InitAppSettingsSchema(context.Background()); err != nil {
		return fmt.Errorf("ошибка инициализации схемы настроек: %w", err)
	}
	if err := repo.InitESForceResyncAuditSchema(context.Background()); err != nil {
		return fmt.Errorf("ошибка инициализации журнала аудита принудительной пересинхронизации ЭС: %w", err)
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
	watchdogTimeoutMinutes := 10
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
		if settings.WatchdogTimeoutMinutes >= 2 {
			watchdogTimeoutMinutes = settings.WatchdogTimeoutMinutes
		}
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

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	if serviceLog != nil {
		go func() {
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
					err := serviceLog.Cleanup(cleanupCtx, time.Now().AddDate(0, 0, -30))
					cleanupCancel()
					if err != nil {
						log.Printf("[ПРЕДУПРЕЖДЕНИЕ] не удалось очистить старые записи журнала службы: %v\n", err)
					}
				}
			}
		}()
	}

	// SQLite health heartbeat: one immediate check, then every 10 seconds.
	// This is an actual DB Ping, not "the process is alive", so the
	// dashboard can distinguish a live process from a broken local store.
	go func() {
		check := func() {
			checkedAt := time.Now()
			err := repo.Ping(ctx)
			health.SetSQLite(checkedAt, err == nil, err)
		}
		check()

		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()

	webServer := web.NewServer(repo, webPort)

	leaseMgr := lease.New()
	eventBus := monitor.NewBus(nil)
	sched := scheduler.New(eventBus)
	devices := make(map[string]*device.Device)
	// transports владеет всеми успешно открытыми транспортами production
	// server. Штатная остановка закрывает их только после завершения poller
	// и физических Web-операций; watchdog использует тот же registry для
	// ограниченной по времени best-effort попытки перед SCM Recovery.
	transports := newTransportRegistry()
	defer func() {
		logTransportCloseSummary("завершение server core", transports.CloseAll(5*time.Second))
	}()

	// deviceKinds хранит тип каждого прибора (akron/vkm360) отдельно от
	// devices — нужно колбэку принудительного переопроса (ниже), чтобы
	// решить, какой именно метод вызывать (ForceReloadAkronHourly или
	// ForceReloadVKMHourly), сам *device.Device своего "типа" не хранит.
	deviceKinds := make(map[string]string)
	// esSyncTriggers хранит канал внепланового запуска синхронизации с ЭС
	// для каждого прибора (см. startESyncForDevice). Он используется только
	// явным запросом синхронизации с ЭС. Принудительный переопрос архива
	// намеренно НЕ запускает ЭС: его контракт — прибор -> локальная БД
	// МодбасШлюза; пересинхронизация с ЭС выполняется отдельной командой.
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
	webDone := make(chan struct{})
	go func() {
		defer close(webDone)
		webServer.Start(ctx)
	}()

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
		return fmt.Errorf("не удалось прочитать список приборов из БД: %w", err)
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
			registerOneDevice(ctx, repo, devRec, leaseMgr, sched, dbPath, &devicesMu, devices, deviceKinds, esSyncTriggers, transports)
		}()
	}
	wg.Wait()

	// Watchdog использует только фактически зарегистрированные приборы.
	// Недоступный при старте прибор не превращается в ложное «зависание»:
	// ошибки связи диагностируются отдельно, а watchdog следит за жизнью
	// механизма опроса.
	activeDeviceIDs := func() []string {
		devicesMu.Lock()
		ids := make([]string, 0, len(devices))
		for id := range devices {
			ids = append(ids, id)
		}
		devicesMu.Unlock()
		return ids
	}
	wd := newServiceWatchdog(watchdogTimeoutMinutes, runningAsService, serviceLog, activeDeviceIDs, transports.CloseAll)
	webServer.SetWatchdogStatus(wd.Status)
	webServer.SetWatchdogTimeout(wd.SetTimeoutMinutes)
	webServer.SetServiceLog(func(limit int) ([]web.ServiceLogEntry, error) {
		return serviceLogEntries(serviceLog, limit)
	})

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
			if len(d.Profile.Archives) > 0 {
				sched.RequestManualPoll(id, scheduler.KindBackfill)
			}
		}
	})

	// Ручной архивный опрос ОДНОГО прибора из «Монитора опроса».
	// Запрос кладётся в ту же per-device FIFO, что и плановые задания.
	// Сам serial-транспорт открывается Reader'ом только на время конкретной
	// транзакции; общий physical-I/O lock не допускает пересечения обменов
	// разных приборов, сидящих на одном COM-порту.
	webServer.SetManualDevicePoll(func(deviceID string) error {
		devicesMu.Lock()
		dev, ok := devices[deviceID]
		devicesMu.Unlock()
		if !ok {
			return fmt.Errorf("прибор %s не зарегистрирован в работающей службе (он отключён или не открыл канал связи при старте)", deviceID)
		}
		if dev.Profile == nil || len(dev.Profile.Archives) == 0 {
			return fmt.Errorf("у прибора %s в профиле нет архивов для опроса", deviceID)
		}
		sched.RequestManualPoll(deviceID, scheduler.KindManualArchive)
		log.Printf("[%s] [WEB] ручной архивный опрос поставлен в очередь; он выполнится без пересечения с плановым опросом\n", deviceID)
		return nil
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
			deviceID, deviceKindLabelRU(kind), from.Format("02.01.2006 15:04"), to.Format("02.01.2006 15:04"))
		// Принудительный переопрос идёт в обход poller, поэтому контроль
		// времени вызываем здесь тем же общим методом. Архивный переопрос
		// продолжается даже если проверка часов не удалась.
		dev.CheckTime(jobCtx)
		switch kind {
		case "akron":
			return dev.ForceReloadAkronHourly(jobCtx, from, to, onProgress)
		case "vkm360":
			return dev.ForceReloadVKMHourly(jobCtx, from, to, onProgress)
		case "ivk-ter":
			return dev.ForceReloadFunc65Hourly(jobCtx, from, to, onProgress)
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
			return fmt.Errorf("для прибора %s es-sync не запущен (проверьте подключение к ЭС и точки, либо дождитесь окончания стартовой регистрации приборов)", deviceID)
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
	webServer.SetForceResyncES(func(ctx context.Context, deviceID string, from, to time.Time, action string) (updated, inserted, failed int, err error) {
		// Durable audit is written for BOTH preview and execute, including
		// failed attempts. Keep this defer at the top so every return path
		// is covered (bad device/config, SQL connection error, validation
		// error inside integration, etc.).
		defer func() {
			auditErr := repo.AddESForceResyncAudit(context.Background(), sqliterepo.ESForceResyncAudit{
				DeviceID: deviceID,
				From:     from,
				To:       to,
				Action:   action,
				Updated:  updated,
				Inserted: inserted,
				Failed:   failed,
				OK:       err == nil,
				Error: func() string {
					if err != nil {
						return err.Error()
					}
					return ""
				}(),
			})
			if auditErr != nil {
				log.Printf("[ERROR] не удалось записать журнал аудита принудительной пересинхронизации ЭС (%s, %s): %v\n",
					deviceID, action, auditErr)
			}
		}()

		devicesMu.Lock()
		kind := deviceKinds[deviceID]
		devicesMu.Unlock()
		if kind != "vkm360" && kind != "akron" && kind != "ivk-ter" && kind != "ivk_ter" {
			err = fmt.Errorf("принудительная пересинхронизация с ЭС не поддерживается для типа прибора %q (прибор %s)", kind, deviceID)
			return
		}

		var cfg integration.Config
		var found bool
		cfg, found, err = buildIntegrationConfig(ctx, repo, deviceID, kind)
		if err != nil {
			return
		}
		if !found {
			err = fmt.Errorf("для прибора %s не настроено подключение к ЭС или точки", deviceID)
			return
		}

		var writer *integration.PointMainsWriter
		writer, err = integration.OpenPointMainsWriter(integration.SQLServerConfig{
			Server: cfg.SQLServer, Database: cfg.SQLDatabase,
			User: cfg.SQLUser, Password: cfg.SQLPassword, Port: cfg.SQLPort,
		})
		if err != nil {
			err = fmt.Errorf("подключение к БД ЭС: %w", err)
			return
		}
		defer writer.Close()

		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if perr := writer.Ping(pingCtx); perr != nil {
			err = fmt.Errorf("проверка подключения к БД ЭС: %w", perr)
			return
		}

		switch action {
		case "preview":
			log.Printf("[WEB] предпросмотр принудительной пересинхронизации с ЭС: прибор %s, %s..%s\n",
				deviceID, from.Format("02.01.2006 15:04"), to.Format("02.01.2006 15:04"))
			updated, inserted, failed, err = integration.PreviewForceResyncRange(ctx, repo, writer, cfg, from, to)
			return
		case "execute":
			log.Printf("[WEB] ВЫПОЛНЕНИЕ принудительной пересинхронизации с ЭС: прибор %s, %s..%s\n",
				deviceID, from.Format("02.01.2006 15:04"), to.Format("02.01.2006 15:04"))
			updated, inserted, failed, err = integration.ForceResyncRange(ctx, repo, writer, cfg, from, to)
			return
		default:
			err = fmt.Errorf("неизвестное действие принудительной пересинхронизации %q", action)
			return
		}
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
	// общий context службы; если обычным ручным запуском — закрываем
	// webStop напрямую, тем же путём, что и Ctrl+C.
	webServer.SetServiceStop(func() {
		if isRunningAsWindowsService() {
			if err := stopSelfAsWindowsService(); err != nil {
				log.Printf("[ERROR] не удалось остановить службу через SCM: %v\n", err)
			}
			return
		}
		webStopOnce.Do(func() { close(webStop) })
	})

	pl := poller.New(sched, devices, 1*time.Second)
	pollerDone := make(chan struct{})
	go func() {
		defer close(pollerDone)
		pl.Run(ctx)
	}()
	go wd.Run(ctx)
	writeServiceEvent(serviceLog, "успешно", "запуск и остановка", "Основной цикл опроса запущен.")

	// До этой точки дошли только после открытия БД, запуска web UI,
	// регистрации доступных приборов и фактического запуска poller.
	// Только теперь Windows Service может честно перейти из StartPending
	// в Running. При обычном консольном запуске закрытие этого канала
	// никому не мешает.
	if onReady != nil {
		onReady()
	}
	log.Println("[ОК] основной цикл опроса запущен")

	if runningAsService {
		<-ctx.Done()
		log.Println("=== получена команда остановки от диспетчера служб Windows. остановка... ===")
		writeServiceEvent(serviceLog, "информация", "запуск и остановка", "Получена команда остановки от диспетчера служб Windows.")
	} else {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-sigChan:
			log.Println("=== получен сигнал завершения (Ctrl+C). остановка... ===")
		case <-webStop:
			log.Println("=== получен запрос на остановку через веб-интерфейс. остановка... ===")
		case <-ctx.Done():
			log.Println("=== получена команда остановки. остановка... ===")
		}
		signal.Stop(sigChan)
	}

	// Сначала реально останавливаем рабочие циклы, и только после этого
	// mbgwServiceHandler сообщает SCM состояние Stopped. Раньше порядок
	// был обратным: SCM уже видел "остановлена", пока runServer ещё
	// только начинал завершение.
	cancel()

	// Poller.Run теперь сам ждёт все per-device worker'ы. Для службы это
	// принципиально: SCM не увидит «остановлена», пока активный обмен с
	// прибором реально не закончен/не отменён context.
	<-pollerDone
	<-webDone

	// К этому моменту poller и все физические HTTP-операции уже завершены,
	// поэтому Close не пересекается со штатным обменом. Явное закрытие до
	// состояния SCM Stopped снижает риск быстрого рестарта на ещё занятом COM.
	logTransportCloseSummary("штатная остановка", transports.CloseAll(5*time.Second))

	writeServiceEvent(serviceLog, "успешно", "запуск и остановка", "МодбасШлюз штатно остановлен; активные операции завершены, транспорты закрыты.")
	log.Println("=== МодбасШлюз остановлен ===")
	return nil
}

// registerOneDevice делает всё, что раньше было одной итерацией
// последовательного цикла в runServer: открывает транспорт/сессию,
// собирает паспорт (Akron), регистрирует прибор в планировщике,
// ставит стартовый дозабор в очередь и запускает es-sync.
// Теперь вызывается в СВОЕЙ горутине на каждый прибор (см.
// комментарий в runServer у wg.Wait()) — ошибка/долгий дозабор одного
// прибора здесь никак не влияет на остальные горутины, вызванные для
// других приборов.
//
// devicesMu защищает devices/deviceKinds/esSyncTriggers — эти три карты
// теперь пишутся ИЗ РАЗНЫХ горутин одновременно (раньше — из одной,
// строго последовательно), так что блокировка обязательна на каждую
// запись, не только ради HTTP-обработчиков, как было раньше.
func deviceKindLabelRU(kind string) string {
	switch kind {
	case "vkm360":
		return "ВКМ-360"
	case "akron":
		return "Акрон"
	case "ivk-ter", "ivk_ter":
		return "ИВК-ТЭР"
	default:
		return kind
	}
}

func archiveScheduleWindow(start, end string) (*scheduler.Window, error) {
	if start == "" && end == "" {
		return nil, nil
	}
	if start == "" || end == "" {
		return nil, fmt.Errorf("начало и конец окна должны быть заданы вместе")
	}
	parse := func(v string) (int, int, error) {
		t, err := time.Parse("15:04", v)
		if err != nil {
			return 0, 0, err
		}
		return t.Hour(), t.Minute(), nil
	}
	sh, sm, err := parse(start)
	if err != nil {
		return nil, fmt.Errorf("начало %q: %w", start, err)
	}
	eh, em, err := parse(end)
	if err != nil {
		return nil, fmt.Errorf("конец %q: %w", end, err)
	}
	return &scheduler.Window{StartHour: sh, StartMinute: sm, EndHour: eh, EndMinute: em}, nil
}

func registerOneDevice(ctx context.Context, repo *sqliterepo.Repo, devRec sqliterepo.DeviceRecord, leaseMgr *lease.LocalLease, sched *scheduler.Scheduler, dbPath string, devicesMu *sync.Mutex, devices map[string]*device.Device, deviceKinds map[string]string, esSyncTriggers map[string]chan struct{}, transports *transportRegistry) {
	profilePath := profilePathNextToExe(devRec.Profile)
	p, err := profile.Parse(profilePath)
	if err != nil {
		log.Printf("[ERROR] прибор %s: ошибка профиля %s (разрешённый путь %s): %v\n",
			devRec.ID, devRec.Profile, profilePath, err)
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

	// Физический канал принадлежит ШИНЕ, а не прибору. Несколько
	// Modbus-адресов могут жить на одном COM-порту. Поэтому даже стартовое
	// Open/session выполняется под тем же bus-lock, что и боевой Reader.
	// После session.Open RTU/TCP-serial освобождается; дальнейшие транзакции
	// будут открывать его только на время одного обмена.
	ioLockKey := pollcore.PhysicalIOLockKey(trParams, devRec.ID)
	unlockIO := pollcore.LockKey(ioLockKey)
	openErr := tr.Open(ctx)
	if openErr != nil {
		_ = transport.ReleaseIdle(tr)
		unlockIO()
		_ = tr.Close()
		switch devRec.TransportKind {
		case "modbus_tcp":
			log.Printf("[НЕТ СВЯЗИ] прибор %s (%s:%d): прибор не отвечает. Возможно, он выключен или недоступен по сети.\n",
				devRec.ID, devRec.Host, devRec.Port)
		default:
			endpoint := devRec.COM
			if endpoint == "" {
				endpoint = devRec.TransportKind
			}
			log.Printf("[НЕТ СВЯЗИ] прибор %s (%s): не удалось открыть подключение: %v. Проверьте питание прибора, кабель и настройки подключения.\n",
				devRec.ID, endpoint, openErr)
		}
		return
	}

	sessionErr := sess.Open(ctx, tr)
	releaseErr := transport.ReleaseIdle(tr)
	unlockIO()
	if sessionErr != nil {
		_ = tr.Close()
		log.Printf("[ERROR] ошибка сессии %s: %v\n", devRec.ID, sessionErr)
		return
	}
	if releaseErr != nil {
		_ = tr.Close()
		log.Printf("[ERROR] прибор %s: не удалось освободить физический канал после открытия сессии: %v\n", devRec.ID, releaseErr)
		return
	}
	if transports == nil || !transports.Add(devRec.ID, tr) {
		_ = tr.Close()
		log.Printf("[INFO] прибор %s: регистрация отменена, сервер уже завершает работу\n", devRec.ID)
		return
	}
	isTCP := devRec.TransportKind == "modbus_tcp"

	unitID := devRec.UnitID
	if unitID == 0 {
		unitID = 1
	}
	reader := pollcore.NewWithLockKey(tr, isTCP, uint8(unitID), ioLockKey)

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
	// Akron-прибора — используя тот же объект транспорта; физический COM
	// Reader откроет только на время команды 101 и сразу освободит.
	if devRec.Kind == "akron" {
		collectAkronPassport(ctx, repo, devRec.ID, reader)
	} else {
		collectProfileSerialPassport(ctx, repo, devRec.ID, p, reader)
	}

	dev := device.New(devRec.ID, p, reader, sess, repo, leaseMgr)
	if devRec.GapScanWindowHours > 0 {
		dev.GapScanWindowHours = devRec.GapScanWindowHours
	}
	dev.BackfillMaxDepthHours = devRec.BackfillMaxDepthHours

	// Individual VKM-360 clock-correction safety limits configured in the
	// device row. Existing devices keep all-zero values after migration, so
	// automatic correction remains disabled until the operator explicitly
	// enables it in the device settings.
	if devRec.Kind == "vkm360" {
		dev.TimeCorrectionDeadbandSeconds = devRec.TimeCorrectionDeadbandSeconds
		dev.TimeCorrectionMaxStepSeconds = devRec.TimeCorrectionMaxStepSeconds
		dev.TimeCorrectionDailyLimitSeconds = devRec.TimeCorrectionDailyLimitSeconds
		if devRec.TimeCorrectionMaxStepSeconds > 0 && devRec.TimeCorrectionDailyLimitSeconds > 0 {
			devicestatus.SetCorrectionMode(devRec.ID, "vkm_enabled")
		} else {
			devicestatus.SetCorrectionMode(devRec.ID, "vkm_disabled")
		}
		// Последняя успешная коррекция хранится в основной SQLite БД.
		// Восстанавливаем её в runtime-монитор после каждого рестарта.
		if rec, found, corrErr := repo.LastTimeCorrection(ctx, devRec.ID); corrErr != nil {
			log.Printf("[ПРЕДУПРЕЖДЕНИЕ] прибор %s: не удалось восстановить последнюю коррекцию времени: %v\n", devRec.ID, corrErr)
		} else if found {
			devicestatus.MarkCorrectionApplied(devRec.ID, rec.CorrectionSeconds, rec.CorrectedAt)
		}
	} else if devRec.Kind == "ivk-ter" || devRec.Kind == "ivk_ter" || p.Meta.Model == "IVK-TER" {
		devicestatus.SetCorrectionMode(devRec.ID, "manual_service")
	} else {
		devicestatus.SetCorrectionMode(devRec.ID, "not_implemented")
	}

	devicesMu.Lock()
	devices[devRec.ID] = dev
	deviceKinds[devRec.ID] = devRec.Kind
	devicesMu.Unlock()

	// Базовый архивный период теперь является свойством ПРОФИЛЯ прибора,
	// а не switch по kind. Это важно для зоопарка: новый профиль сам
	// объявляет 15/30/60-минутный период и не требует правки server.go.
	archivePeriod := time.Duration(0)
	if len(p.Archives) > 0 {
		archivePeriod = p.ArchivePeriod()
	}
	archiveAtMinute := devRec.ArchiveAtMinute
	if archiveAtMinute < 0 {
		archiveAtMinute = 5
	}
	archiveEvery := devRec.ArchiveEveryPeriods
	if archiveEvery <= 0 {
		archiveEvery = 1
	}
	archiveDaysMask := uint8(devRec.ArchiveDaysMask)
	if archiveDaysMask == 0 {
		archiveDaysMask = 0x7f
	}
	archiveWindow, windowErr := archiveScheduleWindow(devRec.ArchiveWindowStart, devRec.ArchiveWindowEnd)
	if windowErr != nil {
		log.Printf("[ERROR] прибор %s: некорректное окно архивного опроса: %v\n", devRec.ID, windowErr)
		return
	}
	if archivePeriod > 0 {
		sched.RegisterArchiveCalendar(devRec.ID, archivePeriod, archiveEvery, archiveDaysMask, archiveWindow, archiveAtMinute)
	}
	log.Printf("[OK] прибор %s (%s) зарегистрирован (архив: период=%s, каждые %d период(а), сдвиг +%d мин, дни=0x%02X)\n",
		devRec.ID, deviceKindLabelRU(devRec.Kind), archivePeriod, archiveEvery, archiveAtMinute, archiveDaysMask)

	// Startup-backfill — отдельная операция восстановления пропусков и
	// поэтому запускается сразу. Регулярный архивный опрос НЕ ставится
	// "сейчас": Scheduler ждёт ближайшую разрешённую календарную границу.
	if len(p.Archives) > 0 {
		sched.RequestManualPoll(devRec.ID, scheduler.KindBackfill)
		log.Printf("[ИНФО] прибор %s: проверка и восстановление недостающих архивных данных поставлены в очередь; следующий штатный опрос — по календарному расписанию\n", devRec.ID)
	}

	// Upstream delivery: VKM, Akron and IVK-TER use the same direct write
	// path to PointMains. The old Akron device-emulation carrier is not
	// part of the current `mbgw server` path.
	switch devRec.Kind {
	case "akron", "vkm360", "ivk-ter", "ivk_ter":
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

// startESyncForDevice starts the ВКМ→Энергосфера direct-DB sync loop for
// one device, IF the operator has configured both the SQL Server
// connection (es_connection) AND at least one channel mapping
// (es_vkm_channels) for it — same "opt-in, missing config = skip with a
// log line, not a fatal error" principle as the Akron branch above.
//
// В production-режиме `mbgw server` цикл синхронизации получает уже
// открытый общий Repo процесса, а не открывает тот же SQLite-файл заново.
// Это важно для параллельного опроса: несколько независимых connection
// pools к одному файлу дали живой SQLITE_BUSY 07.09.2026.
//
// Возвращает канал-триггер внепланового прохода (см. RunEnergosphereSync)
// — nil, если es-sync для этого прибора не запустился (не настроено
// подключение/каналы). Вызывающий код регистрирует его в общей карте,
// чтобы явная команда синхронизации с ЭС могла попросить внеплановый
// проход, не дожидаясь обычного тикера. Принудительный переопрос архива
// этот trigger больше не вызывает.
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
		"V":      "объём",
		"v_plus": "объём прямой", "v_minus": "объём обратный",
		"q_avg": "средний расход", "resistance": "сопротивление",
		"errors": "код ошибок", "comm_fail_time": "нет связи",
		"flowmeter_type": "тип расходомера", "downtime": "простой",
		"power_loss_time": "нет питания",
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
			MinValue: ch.MinValue, MaxValue: ch.MaxValue,
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

func startESyncForDevice(ctx context.Context, repo *sqliterepo.Repo, deviceID, kind, _ string) chan struct{} {
	cfg, found, err := buildIntegrationConfig(ctx, repo, deviceID, kind)
	if err != nil {
		log.Printf("[ОШИБКА] прибор %s: %v — синхронизация с ЭС не запущена\n", deviceID, err)
		return nil
	}
	if !found {
		log.Printf("[ИНФО] прибор %s: подключение к БД ЭС или точки не настроены — синхронизация с ЭС не запущена\n", deviceID)
		return nil
	}

	trigger := make(chan struct{}, 1)
	go func() {
		log.Printf("[ОК] синхронизация с ЭС для %s (%s): запуск (сервер БД ЭС=%s, база=%s)\n", deviceID, deviceKindLabelRU(kind), cfg.SQLServer, cfg.SQLDatabase)
		// В режиме `mbgw server` все фоновые циклы используют ТОТ ЖЕ Repo,
		// который уже открыт процессом. Отдельный sqliterepo.New для каждого
		// прибора создавал независимые SQLite connection pools к одному файлу
		// и в живой работе 07.09.2026 приводил к SQLITE_BUSY во время
		// параллельного дозабора/синхронизации.
		if err := integration.RunEnergosphereSyncWithRepo(ctx, repo, cfg, trigger); err != nil {
			log.Printf("[ОШИБКА] синхронизация с ЭС %s: %v\n", deviceID, err)
		}
	}()
	return trigger
}
