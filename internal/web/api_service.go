package web

import (
	"net/http"
	"strconv"
)

type serviceStatusJSON struct {
	GOOS             string             `json:"goos"`
	RunningAsService bool               `json:"running_as_service"`
	ServiceInstalled bool               `json:"service_installed"`
	ServiceState     string             `json:"service_state,omitempty"`
	Watchdog         WatchdogStatusJSON `json:"watchdog"`
}

// WatchdogStatusJSON — runtime-состояние механизма защиты от зависания.
// Поля полностью русифицируются в UI; здесь английские json-ключи — только
// технический API-контракт.
type WatchdogStatusJSON struct {
	Enabled          bool     `json:"enabled"`
	TimeoutMinutes   int      `json:"timeout_minutes"`
	State            string   `json:"state"`
	Message          string   `json:"message,omitempty"`
	LastCheck        string   `json:"last_check,omitempty"`
	LastActivity     string   `json:"last_activity,omitempty"`
	StuckDevices     []string `json:"stuck_devices,omitempty"`
	RunningAsService bool     `json:"running_as_service"`
}

type ServiceLogEntry struct {
	Time     string `json:"time"`
	Level    string `json:"level"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

type ServiceStatusFunc func() (goos string, runningAsService, serviceInstalled bool, serviceState string)
type ServiceStopFunc func()
type ServiceLogFunc func(limit int) ([]ServiceLogEntry, error)
type WatchdogStatusFunc func() WatchdogStatusJSON

func (s *Server) handleServiceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}
	if s.onServiceStatus == nil {
		writeError(w, http.StatusServiceUnavailable, "получение статуса службы не подключено в этом режиме запуска")
		return
	}
	goos, runningAsService, serviceInstalled, serviceState := s.onServiceStatus()
	wd := WatchdogStatusJSON{Enabled: false, State: "не подключён", RunningAsService: runningAsService}
	if s.onWatchdogStatus != nil {
		wd = s.onWatchdogStatus()
	}
	writeJSON(w, http.StatusOK, serviceStatusJSON{
		GOOS: goos, RunningAsService: runningAsService, ServiceInstalled: serviceInstalled,
		ServiceState: serviceState, Watchdog: wd,
	})
}

func (s *Server) handleServiceLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}
	if s.onServiceLog == nil {
		writeError(w, http.StatusServiceUnavailable, "журнал службы не подключён")
		return
	}
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}
	rows, err := s.onServiceLog(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось прочитать журнал службы: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// /api/health даёт машинно-читаемый статус watchdog и удобен для внешней
// диагностики. При ручном запуске подтверждённое зависание здесь остаётся
// видимым, но процесс не завершается автоматически.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}
	if s.onWatchdogStatus == nil {
		writeJSON(w, http.StatusOK, WatchdogStatusJSON{Enabled: false, State: "не подключён"})
		return
	}
	writeJSON(w, http.StatusOK, s.onWatchdogStatus())
}

func (s *Server) handleServiceStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "используйте POST")
		return
	}
	if s.onServiceStop == nil {
		writeError(w, http.StatusServiceUnavailable, "остановка через веб-интерфейс не подключена в этом режиме запуска")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "остановка запрошена"})
	go s.onServiceStop()
}
