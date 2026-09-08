package watchdog

import (
	"fmt"
	"sort"
	"time"

	"mbgw/internal/health"
)

// Result описывает только факт зависания. Ошибки связи с приборами сюда
// намеренно не входят: watchdog следит за жизнью механизма опроса, а не за
// тем, ответил ли конкретный Modbus-прибор.
type Result struct {
	GlobalStall  bool
	PollerStall  bool
	StuckDevices []string
	Reason       string
}

// Evaluate безопасно различает три ситуации:
//  1. центральный цикл poller перестал подавать heartbeat — глобальное зависание;
//  2. один/несколько приборов застряли в активной операции — только предупреждение;
//  3. ВСЕ зарегистрированные приборы одновременно застряли — глобальное зависание.
//
// В течение startup grace (равного timeout) аварийный вывод не делается.
func Evaluate(now, startedAt time.Time, timeout time.Duration, activeDeviceIDs []string, snapshot health.Snapshot) Result {
	if timeout <= 0 {
		return Result{}
	}
	if now.Sub(startedAt) < timeout {
		return Result{}
	}

	if snapshot.PollerLastCycle.IsZero() || now.Sub(snapshot.PollerLastCycle) > timeout {
		return Result{
			GlobalStall: true,
			PollerStall: true,
			Reason:      fmt.Sprintf("центральный цикл опроса не проявлял активности более %s", humanDuration(timeout)),
		}
	}
	ids := append([]string(nil), activeDeviceIDs...)
	sort.Strings(ids)
	stuck := make([]string, 0)
	for _, id := range ids {
		st, ok := snapshot.Devices[id]
		if !ok {
			continue
		}
		stale := false
		if st.PollInProgress {
			last := st.PollLastProgressAt
			if last.IsZero() {
				last = st.PollStartedAt
			}
			stale = !last.IsZero() && now.Sub(last) > timeout
		}
		// Старая задача в FIFO сама по себе не означает зависание, если
		// прямо сейчас для этого прибора идёт и продвигается длительный
		// backfill/архив. Очередь проверяем только когда активной операции нет.
		if !st.PollInProgress && st.PollQueueDepth > 0 && !st.PollQueuedAt.IsZero() {
			stale = now.Sub(st.PollQueuedAt) > timeout
		}
		if stale {
			stuck = append(stuck, id)
		}
	}

	result := Result{StuckDevices: stuck}
	if len(ids) > 0 && len(stuck) == len(ids) {
		result.GlobalStall = true
		result.Reason = fmt.Sprintf("опрос завис одновременно по всем %d прибор(ам) более %s", len(ids), humanDuration(timeout))
	}
	return result
}

func humanDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%d мин", int(d/time.Minute))
	}
	return d.Round(time.Second).String()
}
