package web

import (
	"net/http"
)

// api_service.go implements the «Служба» tab in /admin (добавлено
// 2026-08-30, прямой запрос оператора: "автоматизировать в юай" статус
// службы Windows и её остановку). Два эндпоинта:
//
//   - GET /api/service/status — текущее состояние: работаем ли мы под
//     Windows вообще (GOOS), запущены ли СЕЙЧАС как служба или обычным
//     процессом, и (если под Windows) состояние самой службы
//     mbgw_service, если она установлена.
//   - POST /api/service/stop — просит процесс остановиться. Способ
//     зависит от того, КАК процесс сейчас запущен (см. onServiceStop в
//     cmd/mbgw/server.go): если службой — команда идёт через
//     диспетчер управления службами Windows (тот же путь, что и
//     "sc stop mbgw_service"); если обычным ручным запуском — процесс
//     останавливается напрямую, тем же путём, что и Ctrl+C.
//
// «Запустить» здесь СОЗНАТЕЛЬНО не реализовано — см. подробное
// объяснение в api_admin_ui.go, вкладка «Служба»: веб-интерфейс
// обслуживается ЭТИМ ЖЕ процессом, так что если процесс остановлен —
// обслуживать HTTP-запрос "запустить" физически некому. Запуск после
// остановки — это `sc start mbgw_service`, "Службы Windows"
// (services.msc), или собственный автозапуск службы при следующей
// загрузке сервера (start=auto, уже настроено install-service).
type serviceStatusJSON struct {
	// GOOS — "windows" | "linux" | ... (см. runtime.GOOS) — вкладка
	// «Служба» в UI показывает содержимое только для windows; для
	// прочих ОС предполагается собственная вкладка в будущем, если
	// когда-нибудь появится реальная необходимость (сейчас проект
	// развёрнут только под Windows).
	GOOS string `json:"goos"`
	// RunningAsService — запущены ли МЫ СЕЙЧАС диспетчером управления
	// службами Windows (а не вручную из консоли/двойным кликом).
	RunningAsService bool `json:"running_as_service"`
	// ServiceInstalled — установлена ли служба mbgw_service в принципе
	// (install-service хотя бы раз запускали) — не путать с
	// RunningAsService: служба может быть установлена, но СЕЙЧАС
	// процесс запущен вручную (или наоборот, что невозможно, но на
	// всякий случай различаем это явно, а не одним булевым флагом).
	ServiceInstalled bool `json:"service_installed"`
	// ServiceState — человекочитаемое состояние службы на русском
	// ("работает", "остановлена" и т.п.) — пусто, если служба не
	// установлена или мы не на Windows.
	ServiceState string `json:"service_state,omitempty"`
}

// ServiceStatusFunc и ServiceStopFunc — типы колбэков, которые подключает
// cmd/mbgw/server.go через SetServiceStatus/SetServiceStop (см.
// server.go). ServiceStatusFunc возвращает простые значения (а не
// serviceStatusJSON напрямую) по тому же принципу, что и
// ForceResyncESFunc в api_es_resync.go — serviceStatusJSON не
// экспортируется из пакета web, так что cmd/mbgw (пакет main) не смог
// бы его напрямую сконструировать; handleServiceStatus ниже сам
// собирает JSON-ответ из этих значений.
type ServiceStatusFunc func() (goos string, runningAsService, serviceInstalled bool, serviceState string)
type ServiceStopFunc func()

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
	writeJSON(w, http.StatusOK, serviceStatusJSON{
		GOOS:             goos,
		RunningAsService: runningAsService,
		ServiceInstalled: serviceInstalled,
		ServiceState:     serviceState,
	})
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
	// Отвечаем браузеру СРАЗУ, ДО того как реально запустим остановку —
	// иначе браузер может не успеть получить ответ вообще, если процесс
	// завершится быстрее, чем HTTP-ответ уйдёт по сети (доказано на
	// практике для похожих случаев в этом проекте — см. handleForceReload
	// в api_reload.go, тот же приём "сначала ответить, потом делать
	// долгую/необратимую работу в фоне").
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "остановка запрошена"})
	go s.onServiceStop()
}
