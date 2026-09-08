//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const mbgwServiceName = "mbgw_service"

// serverCoreFunc — единое ядро сервера. И ручной запуск, и Windows-служба
// запускают ОДНУ И ТУ ЖЕ функцию; различается только владелец context.
type serverCoreFunc func(ctx context.Context, onReady func(), runningAsService bool) error

// runServerAsWindowsServiceIfApplicable перехватывает запуск, если процесс
// действительно создан SCM. В отличие от прежней схемы svc.Run НЕ живёт в
// отдельной горутине: Service Handler сам владеет жизненным циклом ядра.
func runServerAsWindowsServiceIfApplicable(core serverCoreFunc) (handled bool, err error) {
	isService, err := svc.IsWindowsService()
	if err != nil {
		log.Printf("[ОШИБКА] не удалось определить режим запуска Windows-службы: %v — продолжаю как обычный процесс\n", err)
		return false, nil
	}
	if !isService {
		return false, nil
	}
	if err := svc.Run(mbgwServiceName, &mbgwServiceHandler{core: core}); err != nil {
		return true, fmt.Errorf("ошибка диспетчера службы Windows: %w", err)
	}
	return true, nil
}

type mbgwServiceHandler struct {
	core serverCoreFunc
}

// Execute держит SCM в StartPending до фактического запуска poller, а при
// Stop/Shutdown остаётся в StopPending до полного завершения server core.
// Никакого фиксированного «через 10 секунд всё равно Stopped» больше нет:
// новый процесс не должен пересечься со старым на физической линии.
func (h *mbgwServiceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{})
	var readyCh <-chan struct{} = ready
	coreDone := make(chan error, 1)
	go func() {
		coreDone <- h.core(ctx, func() {
			select {
			case <-ready:
			default:
				close(ready)
			}
		}, true)
	}()

	changes <- svc.Status{State: svc.StartPending, WaitHint: 30000}
	starting := true
	var stopRequested bool
	var checkpoint uint32 = 1
	stopTicker := time.NewTicker(time.Second)
	defer stopTicker.Stop()

	for {
		select {
		case err := <-coreDone:
			if stopRequested {
				changes <- svc.Status{State: svc.Stopped}
				return false, 0
			}
			if err != nil {
				log.Printf("[КРИТИЧНО] ядро службы завершилось с ошибкой: %v\n", err)
				return false, 1
			}
			// Самопроизвольное штатное завершение без команды SCM — тоже
			// считаем отказом, чтобы политика Recovery могла восстановить опрос.
			log.Printf("[КРИТИЧНО] ядро службы завершилось без команды остановки\n")
			return false, 1

		case <-readyCh:
			// Закрытый ready всегда готов к чтению. После первого сигнала
			// обязательно отключаем эту ветку select, иначе Service Handler
			// начнёт крутиться вхолостую и может задерживать Stop/coreDone.
			readyCh = nil
			if starting && !stopRequested {
				starting = false
				changes <- svc.Status{State: svc.Running, Accepts: accepted}
			}

		case req, ok := <-r:
			if !ok {
				// Закрытый канал всегда готов к чтению. Отключаем эту ветку
				// select, иначе обработчик может крутиться вхолостую и мешать
				// принять coreDone во время штатного завершения службы.
				stopRequested = true
				cancel()
				r = nil
				continue
			}
			switch req.Cmd {
			case svc.Interrogate:
				if stopRequested {
					changes <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 30000}
				} else if starting {
					changes <- svc.Status{State: svc.StartPending, CheckPoint: checkpoint, WaitHint: 30000}
				} else {
					changes <- svc.Status{State: svc.Running, Accepts: accepted}
				}
			case svc.Stop, svc.Shutdown:
				if !stopRequested {
					stopRequested = true
					checkpoint++
					changes <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 30000}
					cancel()
				}
			}

		case <-stopTicker.C:
			if stopRequested {
				checkpoint++
				changes <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: 30000}
			} else if starting {
				checkpoint++
				changes <- svc.Status{State: svc.StartPending, CheckPoint: checkpoint, WaitHint: 30000}
			}
		}
	}
}

type windowsServiceStatus struct {
	Installed bool
	State     string
}

func queryWindowsServiceStatus() (windowsServiceStatus, error) {
	m, err := mgr.Connect()
	if err != nil {
		return windowsServiceStatus{}, fmt.Errorf("подключение к диспетчеру служб: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(mbgwServiceName)
	if err != nil {
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

func isRunningAsWindowsService() bool {
	isService, err := svc.IsWindowsService()
	return err == nil && isService
}
