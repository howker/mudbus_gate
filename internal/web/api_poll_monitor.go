package web

import (
	"net/http"
	"sort"
	"time"

	"mbgw/internal/devicestatus"
	"mbgw/internal/health"
)

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

	TimeDriftKnown     bool    `json:"time_drift_known"`
	TimeDriftReliable  bool    `json:"time_drift_reliable"`
	TimeDriftSeconds   float64 `json:"time_drift_seconds"`
	TimeDriftCheckedAt string  `json:"time_drift_checked_at,omitempty"`
	TimeDriftNote      string  `json:"time_drift_note,omitempty"`

	CorrectionMode        string `json:"correction_mode"`
	LastCorrectionKnown   bool   `json:"last_correction_known"`
	LastCorrectionAt      string `json:"last_correction_at,omitempty"`
	LastCorrectionSeconds int    `json:"last_correction_seconds"`
}

func (s *Server) handlePollMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}

	snapshot := health.Get()
	drifts := devicestatus.All()
	corrections := devicestatus.AllCorrections()

	// Показываем только реально включённые приборы. Runtime-кэши могут
	// содержать старые статусы после отключения прибора, поэтому фильтр
	// делается на сервере по конфигурационной БД, а не только в JavaScript.
	configured, err := s.repo.ListDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось прочитать список приборов: "+err.Error())
		return
	}
	enabled := make(map[string]bool, len(configured))
	for _, d := range configured {
		if d.Enabled {
			enabled[d.ID] = true
		}
	}

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
		reloads[id] = reloadSnapshot{job.StartedAt, job.FinishedAt, job.Finished, job.Error}
	}
	s.reloadJobsMu.Unlock()

	idSet := make(map[string]bool)
	for id := range enabled {
		idSet[id] = true
	}
	for id := range snapshot.Devices {
		if enabled[id] {
			idSet[id] = true
		}
	}
	for id := range reloads {
		if enabled[id] {
			idSet[id] = true
		}
	}
	for id := range drifts {
		if enabled[id] {
			idSet[id] = true
		}
	}
	for id := range corrections {
		if enabled[id] {
			idSet[id] = true
		}
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
			ID: id, PollInProgress: st.PollInProgress, PollKind: st.PollKind,
			LastPollOK: st.LastPollOK, LastPollKnown: st.LastPollKnown,
			LastPollKind: st.LastPollKind, LastPollError: st.LastPollError,
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

		if d, ok := drifts[id]; ok {
			row.TimeDriftKnown = true
			row.TimeDriftReliable = d.Reliable
			row.TimeDriftSeconds = d.DriftSeconds
			row.TimeDriftNote = d.Note
			if !d.CheckedAt.IsZero() {
				row.TimeDriftCheckedAt = d.CheckedAt.Format("02.01.2006 15:04:05")
			}
		}
		if c, ok := corrections[id]; ok {
			row.CorrectionMode = c.Mode
			row.LastCorrectionKnown = c.LastAppliedKnown
			row.LastCorrectionSeconds = c.LastAppliedSeconds
			if c.LastAppliedKnown && !c.LastAppliedAt.IsZero() {
				row.LastCorrectionAt = c.LastAppliedAt.Format("02.01.2006 15:04:05")
			}
		}

		if reload, ok := reloads[id]; ok {
			if !reload.finished {
				row.PollInProgress = true
				row.PollKind = "manual_reload"
				if !reload.startedAt.IsZero() {
					row.PollStartedAt = reload.startedAt.Format("02.01.2006 15:04:05")
				}
			} else if !reload.finishedAt.IsZero() && (st.LastPollFinishedAt.IsZero() || reload.finishedAt.After(st.LastPollFinishedAt)) {
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
