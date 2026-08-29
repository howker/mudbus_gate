package web

import (
	"context"
	"net/http"
	"time"

	"mbgw/internal/devicestatus"
)

// api_dashboard.go implements GET /api/dashboard — one row per configured
// device, combining three independently-sourced facts into a single
// table the operator can sort (см. вкладку «Главная» в api_admin_ui.go,
// добавлено 2026-08-29 по прямому запросу оператора: "мы никак не
// отслеживаем какое время сейчас в приборе... нужен дашборд"):
//
//  1. LagHours — сколько последних периодов архива ещё не собрано,
//     выраженное в часах (для ВКМ с получасовым периодом 2 пропущенных
//     периода = 1 час, для Akron с часовым — 1 пропущенный период =
//     1 час). Считается ЧИСТО из уже сохранённой истории (archive_hourly)
//     — не нужен доступ к живым приборам/девайсам процесса, только repo.
//  2. TimeDrift* — дрейф часов ПРИБОРА относительно сервера, из
//     internal/devicestatus (заполняется живьём в
//     internal/device/vkm_hourly.go при каждом сборе архива ВКМ; для
//     Akron источника пока нет — Akron архив (User-Defined команда 104,
//     см. profiles/akron01.yaml) отдаёт только BCD, отдельного текстового
//     диапазона времени с обеих границ периода, как у ВКМ, там нет).
//  3. SyncSupported — только у ВКМ есть прямая запись в БД ЭС
//     (internal/integration/energosphere_sync.go), соответственно только
//     для него имеет смысл кнопка «Синхронизировать сейчас» (перенесена
//     сюда со вкладки «Подключение к ЭС», 2026-08-29 — было неочевидно,
//     что кнопка вообще есть, раз она физически далеко от общего
//     обзора приборов).
type dashboardDeviceStatus struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`

	// LagKnown=false означает "архив для этого прибора не собирался НИ
	// РАЗУ" — отдельное, более серьёзное состояние, чем просто большое
	// число часов отставания, поэтому фронтенд должен показывать его
	// отдельным текстом ("нет данных"), а не как "-9999ч" или похожую
	// подмену.
	LagHours float64 `json:"lag_hours"`
	LagKnown bool    `json:"lag_known"`

	// LastPeriod — метка последнего периода архива, реально сохранённого
	// в archive_hourly ("" если архив не собирался ни разу). NextPollAt
	// — время следующего ПЛАНОВОГО опроса архива по расписанию (граница
	// периода + ArchiveAtMinute) — добавлено 2026-08-30 по прямому
	// запросу оператора: "непонятно когда следующий опрос запланирован".
	// Оба поля — просто человекочитаемое объяснение того, что уже
	// участвует в расчёте LagHours (см. computeArchiveInfo) — отдельная
	// система отслеживания для них не нужна.
	LastPeriod string `json:"last_period,omitempty"`
	NextPollAt string `json:"next_poll_at,omitempty"`

	TimeDriftSeconds   float64 `json:"time_drift_seconds,omitempty"`
	TimeDriftKnown     bool    `json:"time_drift_known"`
	TimeDriftReliable  bool    `json:"time_drift_reliable"`
	TimeDriftNote      string  `json:"time_drift_note,omitempty"`
	TimeDriftCheckedAt string  `json:"time_drift_checked_at,omitempty"`

	SyncSupported bool `json:"sync_supported"`
}

func (s *Server) handleDashboardStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}

	devices, err := s.repo.ListDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ошибка чтения приборов: "+err.Error())
		return
	}

	drifts := devicestatus.All()

	out := make([]dashboardDeviceStatus, 0, len(devices))
	for _, dev := range devices {
		st := dashboardDeviceStatus{
			ID:            dev.ID,
			Name:          dev.Name,
			Kind:          dev.Kind,
			Enabled:       dev.Enabled,
			SyncSupported: dev.Kind == "vkm360",
		}

		period, param := archivePeriodAndParamForKind(dev.Kind)
		if period > 0 {
			info := s.computeArchiveInfo(r.Context(), dev.ID, param, period, dev.ArchiveAtMinute)
			st.LagHours = info.LagHours
			st.LagKnown = info.LagKnown
			st.LastPeriod = info.LastPeriod
			st.NextPollAt = info.NextPollAt
		}

		if d, ok := drifts[dev.ID]; ok {
			st.TimeDriftKnown = true
			st.TimeDriftReliable = d.Reliable
			st.TimeDriftNote = d.Note
			st.TimeDriftCheckedAt = d.CheckedAt.Format("02.01.2006 15:04:05")
			if d.Reliable {
				st.TimeDriftSeconds = d.DriftSeconds
			}
		}

		out = append(out, st)
	}

	writeJSON(w, http.StatusOK, out)
}

// archivePeriodAndParamForKind returns the archive period length and a
// representative archive_hourly param to query for "latest collected
// period" per device kind — small, deliberately duplicated table rather
// than importing internal/device (which would risk an import cycle:
// internal/device already needs internal/web-adjacent things through
// other paths in this project's dependency graph, and this handler only
// needs two constants, not the whole package).
//
//	vkm360: 30 минут (vkmArchivePeriod в internal/device/vkm_hourly.go),
//	        представительный параметр "S" (масса — есть у любого рабочего
//	        ВКМ, в отличие от T/Pi, которые могут отсутствовать на
//	        неподключённой трубе).
//	akron:  1 час, представительный параметр "V" (объём — единственный
//	        параметр профиля akron01.yaml).
//
// Неизвестный/неподдерживаемый kind → period=0, вызывающая сторона тогда
// пропускает расчёт отставания (LagKnown остаётся false).
func archivePeriodAndParamForKind(kind string) (time.Duration, string) {
	switch kind {
	case "vkm360":
		return 30 * time.Minute, "S"
	case "akron":
		return time.Hour, "V"
	default:
		return 0, ""
	}
}

// archiveInfo — то, что computeArchiveInfo вычисляет за один проход:
// отставание архива, метка последнего реально собранного периода и
// время следующего планового опроса по расписанию.
type archiveInfo struct {
	LagHours   float64
	LagKnown   bool
	LastPeriod string
	NextPollAt string
}

// computeArchiveInfo считает отставание архива (в часах — см. прежний
// doc-комментарий ниже, логика не изменилась) И, ДОБАВЛЕНО 2026-08-30 по
// прямому запросу оператора ("непонятно, когда следующий опрос
// запланирован"), заодно возвращает:
//
//   - LastPeriod — метка последнего периода, реально сохранённого в
//     archive_hourly (то, что и так уже читаем для отставания — новых
//     запросов к БД не требуется).
//   - NextPollAt — ближайшее БУДУЩЕЕ время вида «граница периода +
//     ArchiveAtMinute», то есть именно то время, когда планировщик
//     (sched.RegisterWithArchiveAnchor в cmd/mbgw/server.go) реально
//     запустит следующий опрос архива этого прибора. Считается НАПРЯМУЮ
//     по формуле, без обращения к самому планировщику — тот же принцип,
//     что и во всём остальном дашборде: чистая функция от (текущее
//     время, period, archiveAtMinute), без завязки на внутреннее
//     состояние работающего процесса.
//
// Отставание архива — сколько последних периодов ещё не собрано, в
// часах. Логика:
//
//  1. Находим ГРАНИЦУ последнего периода, который уже точно должен был
//     закрыться И быть собранным к текущему моменту — обычная граница
//     сетки (now, округлённое вниз до period) минус ещё один period,
//     ЕСЛИ мы всё ещё внутри "льготного окна" после самой границы (см.
//     ниже) — прямой запрос оператора: "проверять нужно не в 00, а
//     00-07, так как прибор опрашивается чуть позже границы часа".
//  2. Смотрим, какой период РЕАЛЬНО последний сохранён в archive_hourly
//     (GetHourlyArchiveDesc с limit=1 — уже отсортировано по убыванию).
//  3. Разница между ожидаемой границей и реально сохранённой, делённая
//     на period, и есть отставание — переводим в часы для единообразного
//     отображения независимо от типа прибора (получасовки ВКМ и часовки
//     Akron на одной шкале).
func (s *Server) computeArchiveInfo(ctx context.Context, deviceID, param string, period time.Duration, archiveAtMinute int) archiveInfo {
	// rawAnchor — РЕАЛЬНОЕ значение минуты-якоря, как его использует сам
	// планировщик (см. cmd/mbgw/server.go: archiveAtMinute := devRec.
	// ArchiveAtMinute; if archiveAtMinute < 0 { archiveAtMinute = 5 }).
	// НЕ то же самое, что grace ниже — тот специально расширен буфером
	// для решения "просрочено или ещё нет", а NextPollAt должен показывать
	// ТОЧНОЕ время по расписанию, без этого буфера.
	rawAnchor := archiveAtMinute
	if rawAnchor < 0 {
		rawAnchor = 5
	}
	grace := rawAnchor + 2 // небольшой буфер на время самого сетевого опроса, не только на срабатывание тикера

	now := time.Now()
	currentBoundary := now.Truncate(period)

	lastClosed := currentBoundary
	if now.Sub(currentBoundary) < time.Duration(grace)*time.Minute {
		// Ещё не наступило время планового опроса ДАЖЕ для только что
		// закрывшегося периода — не считаем его просроченным, сдвигаем
		// ожидаемую границу на один период назад.
		lastClosed = currentBoundary.Add(-period)
	}

	// Ближайшее будущее время вида "граница + rawAnchor минут". Если
	// такое время для ТЕКУЩЕЙ границы уже прошло (или наступает прямо
	// сейчас) — берём следующую границу.
	nextPollAt := currentBoundary.Add(time.Duration(rawAnchor) * time.Minute)
	if !nextPollAt.After(now) {
		nextPollAt = currentBoundary.Add(period).Add(time.Duration(rawAnchor) * time.Minute)
	}

	info := archiveInfo{
		NextPollAt: nextPollAt.Format("02.01.2006 15:04:05"),
	}

	rows, err := s.repo.GetHourlyArchiveDesc(ctx, deviceID, "", param, 0, 1)
	if err != nil || len(rows) == 0 {
		return info // LagKnown остаётся false, LastPeriod остаётся ""
	}

	info.LastPeriod = rows[0].TsHour.Format("02.01.2006 15:04")

	lag := lastClosed.Sub(rows[0].TsHour)
	if lag < 0 {
		lag = 0 // защита от отрицательного значения (переопрос "в будущее" и т.п.) — не должно случаться в норме
	}
	info.LagHours = -lag.Hours()
	info.LagKnown = true
	return info
}
