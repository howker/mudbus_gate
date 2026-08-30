//go:build windows

package main

import (
	"log"

	"golang.org/x/sys/windows/svc"
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
