package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"mbgw/internal/health"
	"mbgw/internal/servicelog"
	"mbgw/internal/watchdog"
	"mbgw/internal/web"
)

type serviceWatchdog struct {
	mu sync.RWMutex

	startedAt        time.Time
	timeout          time.Duration
	runningAsService bool
	activeDeviceIDs  func() []string
	serviceLog       *servicelog.Log
	status           web.WatchdogStatusJSON
	lastEventKey     string
}

func newServiceWatchdog(minutes int, runningAsService bool, serviceLog *servicelog.Log, activeDeviceIDs func() []string) *serviceWatchdog {
	if minutes < 2 {
		minutes = 10
	}
	return &serviceWatchdog{
		startedAt: time.Now(), timeout: time.Duration(minutes) * time.Minute,
		runningAsService: runningAsService, serviceLog: serviceLog, activeDeviceIDs: activeDeviceIDs,
		status: web.WatchdogStatusJSON{
			Enabled: true, TimeoutMinutes: minutes, State: "норма",
			Message: "контроль зависания активен", RunningAsService: runningAsService,
		},
	}
}

func (w *serviceWatchdog) SetTimeoutMinutes(minutes int) {
	if minutes < 2 {
		minutes = 2
	}
	w.mu.Lock()
	w.timeout = time.Duration(minutes) * time.Minute
	w.status.TimeoutMinutes = minutes
	w.mu.Unlock()
	w.event("информация", "контроль зависания", fmt.Sprintf("Таймаут контроля зависания изменён на %d мин.", minutes))
}

func (w *serviceWatchdog) Status() web.WatchdogStatusJSON {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := w.status
	out.StuckDevices = append([]string(nil), w.status.StuckDevices...)
	return out
}

func (w *serviceWatchdog) Run(ctx context.Context) {
	w.event("успешно", "контроль зависания", fmt.Sprintf("Контроль зависания опроса запущен. Таймаут: %d мин.", w.Status().TimeoutMinutes))
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			w.check(now)
		}
	}
}

func (w *serviceWatchdog) check(now time.Time) {
	w.mu.RLock()
	timeout := w.timeout
	w.mu.RUnlock()
	ids := w.activeDeviceIDs()
	snapshot := health.Get()
	result := watchdog.Evaluate(now, w.startedAt, timeout, ids, snapshot)

	lastActivity := snapshot.PollerLastCycle
	for _, st := range snapshot.Devices {
		if st.PollLastProgressAt.After(lastActivity) {
			lastActivity = st.PollLastProgressAt
		}
	}

	state := "норма"
	message := "опрос и планировщик проявляют активность"
	if result.GlobalStall {
		state = "зависание"
		message = result.Reason
	} else if len(result.StuckDevices) > 0 {
		state = "предупреждение"
		message = "Возможно завис опрос отдельных приборов: " + strings.Join(result.StuckDevices, ", ")
	}

	w.mu.Lock()
	w.status.State = state
	w.status.Message = message
	w.status.LastCheck = now.Format("02.01.2006 15:04:05")
	if !lastActivity.IsZero() {
		w.status.LastActivity = lastActivity.Format("02.01.2006 15:04:05")
	}
	w.status.StuckDevices = append([]string(nil), result.StuckDevices...)
	key := state + "|" + message
	changed := key != w.lastEventKey
	w.lastEventKey = key
	serviceMode := w.runningAsService
	w.mu.Unlock()

	if changed {
		switch state {
		case "норма":
			w.event("успешно", "контроль зависания", "Контроль опроса: состояние нормализовалось.")
		case "предупреждение":
			w.event("предупреждение", "контроль зависания", message+" Весь шлюз не перезапускается, потому что остальные приборы/планировщик продолжают работу.")
		case "зависание":
			w.event("критично", "контроль зависания", message)
		}
	}

	if result.GlobalStall && serviceMode {
		critical := message + ". Процесс запущен как служба Windows — запрошен аварийный перезапуск через политику восстановления SCM."
		log.Printf("[КРИТИЧНО] %s\n", critical)
		// Отдельная БД журнала получает максимум 2 секунды. Даже если диск/БД
		// сами зависли, watchdog не должен зависнуть вместе с ними.
		if w.serviceLog != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = w.serviceLog.Append(ctx, "критично", "контроль зависания", critical)
			cancel()
		}
		os.Exit(1)
	}
}

func (w *serviceWatchdog) event(level, category, message string) {
	log.Printf("[СЛУЖБА] %s\n", message)
	if w.serviceLog == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.serviceLog.Append(ctx, level, category, message); err != nil {
		log.Printf("[ПРЕДУПРЕЖДЕНИЕ] журнал службы: %v\n", err)
	}
}

func writeServiceEvent(logDB *servicelog.Log, level, category, message string) {
	if logDB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := logDB.Append(ctx, level, category, message); err != nil {
		log.Printf("[ПРЕДУПРЕЖДЕНИЕ] журнал службы: %v\n", err)
	}
}

func serviceLogEntries(logDB *servicelog.Log, limit int) ([]web.ServiceLogEntry, error) {
	if logDB == nil {
		return []web.ServiceLogEntry{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := logDB.List(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]web.ServiceLogEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, web.ServiceLogEntry{Time: r.Time, Level: r.Level, Category: r.Category, Message: r.Message})
	}
	return out, nil
}
