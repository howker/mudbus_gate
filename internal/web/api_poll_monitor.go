package web

import (
	"net/http"
	"sort"
	"time"

	"mbgw/internal/health"
)

// pollMonitorRow — лёгкий runtime-снимок для вкладки «Монитор опроса».
// Здесь намеренно нет обращений к SQLite: вкладка обновляется часто, а
// имена/список приборов браузер уже получает через /api/devices. Это не
// создаёт лишнюю конкуренцию с записью архива в локальную БД.
type pollMonitorRow struct {
	ID string `json:"id"`

	PollInProgress bool   `json:"poll_in_progress"`
	PollStartedAt  string `json:"poll_started_at,omitempty"`
	PollKind       string `json:"poll_kind,omitempty"`
	NextPollAt     string `json:"next_poll_at,omitempty"`

	LastPollFinishedAt string `json:"last_poll_finished_at,omitempty"`
	LastPollOK         bool   `json:"last_poll_ok"`
	LastPollKnown      bool   `json:"last_poll_known"`
	LastPollKind       string `json:"last_poll_kind,omitempty"`
	LastPollError      string `json:"last_poll_error,omitempty"`
}

func (s *Server) handlePollMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}

	snapshot := health.Get()

	// Принудительный переопрос идёт напрямую из web-job, минуя poller,
	// поэтому накладываем его состояние отдельно. Для завершённых задач
	// также учитываем результат, если этот переопрос новее последнего
	// планового current/archive — тогда колонка «Статус последнего опроса»
	// показывает действительно последнюю операторскую операцию.
	type reloadSnapshot struct {
		startedAt  time.Time
		finishedAt time.Time
		finished   bool
		errorText  string
	}
	reloads := make(map[string]reloadSnapshot)
	s.reloadJobsMu.Lock()
	for id, job := range s.reloadJobs {
		if job == nil {
			continue
		}
		reloads[id] = reloadSnapshot{
			startedAt:  job.StartedAt,
			finishedAt: job.FinishedAt,
			finished:   job.Finished,
			errorText:  job.Error,
		}
	}
	s.reloadJobsMu.Unlock()

	idSet := make(map[string]bool)
	for id := range snapshot.Devices {
		idSet[id] = true
	}
	for id := range reloads {
		idSet[id] = true
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]pollMonitorRow, 0, len(ids))
	for _, id := range ids {
		st := snapshot.Devices[id]
		row := pollMonitorRow{
			ID:             id,
			PollInProgress: st.PollInProgress,
			PollKind:       st.PollKind,
			LastPollOK:     st.LastPollOK,
			LastPollKnown:  st.LastPollKnown,
			LastPollKind:   st.LastPollKind,
			LastPollError:  st.LastPollError,
		}
		if !st.PollStartedAt.IsZero() {
			row.PollStartedAt = st.PollStartedAt.Format("02.01.2006 15:04:05")
		}
		if !st.NextPollAt.IsZero() {
			row.NextPollAt = st.NextPollAt.Format("02.01.2006 15:04:05")
		}
		if !st.LastPollFinishedAt.IsZero() {
			row.LastPollFinishedAt = st.LastPollFinishedAt.Format("02.01.2006 15:04:05")
		}

		if reload, ok := reloads[id]; ok {
			if !reload.finished {
				row.PollInProgress = true
				row.PollKind = "manual_reload"
				if !reload.startedAt.IsZero() {
					row.PollStartedAt = reload.startedAt.Format("02.01.2006 15:04:05")
				}
			} else if !reload.finishedAt.IsZero() &&
				(st.LastPollFinishedAt.IsZero() || reload.finishedAt.After(st.LastPollFinishedAt)) {
				row.LastPollKnown = true
				row.LastPollKind = "manual_reload"
				row.LastPollOK = reload.errorText == ""
				row.LastPollError = reload.errorText
				row.LastPollFinishedAt = reload.finishedAt.Format("02.01.2006 15:04:05")
			}
		}
		out = append(out, row)
	}

	writeJSON(w, http.StatusOK, out)
}
