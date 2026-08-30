//go:build windows

package main

import (
	"fmt"
	"log"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// mbgwServiceName должно СОВПАДАТЬ с именем, под которым служба
// регистрируется через install-service (см. service_windows.go:
// "sc", "create", "mbgw_service", ...) — иначе Windows не сможет
// сопоставить запущенный процесс с описанием службы, и повторится та
// же ошибка 1053.
const mbgwServiceName = "mbgw_service"

// runServerAsWindowsServiceIfApplicable проверяет, запущен ли процесс
// диспетчером управления службами Windows (SCM), а не вручную из
// консоли или двойным кликом (добавлено 2026-08-30, найдено оператором
// живьём: "sc start mbgw_service" заканчивался ошибкой 1053 — "служба
// не ответила на запрос своевременно"). Реальный процесс при этом мог
// быть полностью исправен и работать — просто он никогда не отправлял
// SCM подтверждение "я запущен и готов" по правильному протоколу
// (StartServiceCtrlDispatcher), и SCM решал, что служба зависла, и
// убивал её по таймауту.
//
// ВАЖНО: этот вызов должен идти в САМОМ НАЧАЛЕ runServer(), ДО долгой
// регистрации приборов — та же логика, что и в сегодняшнем более раннем
// фиксе с ранним запуском веб-сервера: SCM должен получить
// подтверждение "работает" СРАЗУ, а не после того, как отработает
// дозабор архива у всех приборов (который, как мы уже видели на живом
// примере с прибором osmos, может идти очень долго).
//
// Возвращает канал, который ЗАКРЫВАЕТСЯ, когда SCM присылает запрос на
// остановку (Stop/Shutdown) — runServer() слушает его наравне с обычным
// Ctrl+C. Если процесс запущен НЕ как служба (обычный ручной запуск из
// консоли) — возвращает nil: канал nil в select просто никогда не
// срабатывает, так что весь остальной код продолжает работать в точности
// как раньше, без единого изменения поведения при ручном запуске.
func runServerAsWindowsServiceIfApplicable() <-chan struct{} {
	isService, err := svc.IsWindowsService()
	if err != nil {
		log.Printf("[ERROR] не удалось определить, запущены ли мы как служба Windows: %v — продолжаю как обычный процесс\n", err)
		return nil
	}
	if !isService {
		return nil
	}

	stopChan := make(chan struct{})
	handler := &mbgwServiceHandler{stopChan: stopChan}
	go func() {
		// svc.Run блокирует до тех пор, пока служба не остановится —
		// именно поэтому запускаем его в отдельной горутине, а не
		// напрямую: остальной код runServer() (открытие БД, регистрация
		// приборов и т.д.) должен продолжать выполняться параллельно,
		// а не ждать здесь.
		if err := svc.Run(mbgwServiceName, handler); err != nil {
			log.Fatalf("[FATAL] ошибка работы как службы Windows: %v", err)
		}
	}()
	return stopChan
}

// mbgwServiceHandler реализует интерфейс svc.Handler — протокол
// взаимодействия со службами Windows. Сразу подтверждает SCM, что
// служба запущена (Running), НЕ дожидаясь завершения регистрации
// приборов — это отдельный, более ранний и быстрый шаг, чем настоящая
// готовность самого сервера опроса. Слушает запросы на остановку
// (Stop/Shutdown от SCM, например через "Службы Windows" или
// "sc stop mbgw_service") и по такому запросу закрывает stopChan,
// сигнализируя основному коду runServer() начать штатную остановку —
// тот же путь остановки, что и при обычном Ctrl+C.
type mbgwServiceHandler struct {
	stopChan chan struct{}
}

func (h *mbgwServiceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const acceptedCommands = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}
	changes <- svc.Status{State: svc.Running, Accepts: acceptedCommands}

	for req := range r {
		switch req.Cmd {
		case svc.Interrogate:
			// SCM периодически спрашивает "как дела" — просто повторяем
			// последний известный статус, как и требует протокол.
			changes <- req.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			close(h.stopChan)
			changes <- svc.Status{State: svc.Stopped}
			return false, 0
		}
	}
	return false, 0
}

// windowsServiceStatus — то, что показывает вкладка «Служба» в /admin
// (добавлено 2026-08-30, прямой запрос оператора: "автоматизировать в
// юай" остановку/просмотр статуса службы). Installed=false означает,
// что служба вообще не зарегистрирована в Windows (install-service ещё
// ни разу не запускали) — отдельное, более раннее состояние, чем
// "остановлена".
type windowsServiceStatus struct {
	Installed bool
	State     string // человекочитаемое состояние на русском, см. serviceStateToRussian
}

// queryWindowsServiceStatus узнаёт текущее состояние службы через API
// диспетчера управления службами (mgr/svc) — НЕ через запуск sc.exe
// подпроцессом и разбор текстового вывода: так надёжнее (нет риска
// напороться на то, что "sc" в PowerShell — это алиас для Set-Content,
// а не вызов настоящего sc.exe, — ровно та ловушка, в которую живьём
// попал оператор 2026-08-30 при ручной проверке) и не зависит от языка
// системы, на котором sc.exe печатает свой текстовый вывод.
func queryWindowsServiceStatus() (windowsServiceStatus, error) {
	m, err := mgr.Connect()
	if err != nil {
		return windowsServiceStatus{}, fmt.Errorf("подключение к диспетчеру служб: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(mbgwServiceName)
	if err != nil {
		// Открыть описание службы не удалось — почти наверняка потому,
		// что она ещё не установлена (install-service не запускали).
		// Это НЕ ошибка в смысле "что-то сломалось", а нормальное,
		// ожидаемое состояние для свежего сервера.
		return windowsServiceStatus{Installed: false}, nil
	}
	defer s.Close()

	status, err := s.Query()
	if err != nil {
		return windowsServiceStatus{}, fmt.Errorf("опрос состояния службы: %w", err)
	}
	return windowsServiceStatus{Installed: true, State: serviceStateToRussian(status.State)}, nil
}

func serviceStateToRussian(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "остановлена"
	case svc.StartPending:
		return "запускается"
	case svc.StopPending:
		return "останавливается"
	case svc.Running:
		return "работает"
	case svc.ContinuePending:
		return "возобновляется"
	case svc.PausePending:
		return "приостанавливается"
	case svc.Paused:
		return "приостановлена"
	default:
		return "неизвестно"
	}
}

// stopSelfAsWindowsService отправляет службе mbgw_service команду
// остановки ЧЕРЕЗ ДИСПЕТЧЕР УПРАВЛЕНИЯ СЛУЖБАМИ (тот же путь, что и
// "sc stop mbgw_service" или кнопка «Остановить» в services.msc) — а
// НЕ напрямую завершает текущий процесс изнутри себя. Это важно: раз
// запрос идёт через SCM, он попадает в тот же самый обработчик
// mbgwServiceHandler.Execute выше (в канал r), который уже правильно
// обрабатывает штатную остановку (StopPending -> закрытие stopChan,
// которое слушает runServer -> Stopped) — используется УЖЕ
// подтверждённый рабочий путь остановки, без дублирования логики.
//
// Используется кнопкой «Остановить» на вкладке «Служба» в /admin —
// вызывается ИЗ обработчика веб-запроса ЭТОГО ЖЕ процесса, который сам
// себя просит остановиться через SCM; это нормально и работает, потому
// что HTTP-ответ браузеру отправляется веб-слоем ДО того, как процесс
// реально завершится (см. api_service.go).
func stopSelfAsWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("подключение к диспетчеру служб: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(mbgwServiceName)
	if err != nil {
		return fmt.Errorf("открытие описания службы: %w", err)
	}
	defer s.Close()

	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("отправка команды остановки: %w", err)
	}
	return nil
}

// isRunningAsWindowsService сообщает, запущен ли ТЕКУЩИЙ процесс
// диспетчером управления службами Windows ПРЯМО СЕЙЧАС — используется
// веб-обработчиком «Остановить» на вкладке «Служба» (см. api_service.go
// через cmd/mbgw/server.go), чтобы решить, каким путём останавливаться:
// через SCM (stopSelfAsWindowsService — правильный путь, когда мы
// реально служба) или напрямую, закрыв локальный канал остановки без
// участия SCM (для случая обычного ручного запуска, где никакого SCM
// вообще нет).
func isRunningAsWindowsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return isService
}
