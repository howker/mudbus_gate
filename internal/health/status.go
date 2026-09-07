package health

import (
	"sync"
	"time"
)

// DeviceStatus хранит подтверждённую полезную работу и текущее состояние
// опроса одного прибора. Поля копируются наружу только через Get().
type DeviceStatus struct {
	LastCurrentSuccess time.Time
	LastArchiveSuccess time.Time
	LastESWriteSuccess time.Time

	PollInProgress bool
	PollStartedAt  time.Time
	PollKind       string

	LastPollFinishedAt time.Time
	LastPollOK         bool
	LastPollKnown      bool
	LastPollKind       string
	LastPollError      string

	NextPollAt time.Time
}

// Snapshot is a point-in-time copy safe for the Web/API layer.
type Snapshot struct {
	PollerLastCycle time.Time
	Devices         map[string]DeviceStatus

	SQLiteCheckedAt time.Time
	SQLiteOK        bool
	SQLiteError     string
}

var state struct {
	sync.RWMutex

	pollerLastCycle time.Time
	devices         map[string]DeviceStatus

	sqliteCheckedAt time.Time
	sqliteOK        bool
	sqliteError     string
}

func init() {
	state.devices = make(map[string]DeviceStatus)
}

// MarkPollerCycle records that the central poller completed one scheduler
// cycle. This is deliberately separate from per-device success: a live
// scheduler is not proof that any meter answered.
func MarkPollerCycle(at time.Time) {
	state.Lock()
	state.pollerLastCycle = at
	state.Unlock()
}

// MarkCurrentSuccess records confirmed useful current-value work for one
// device.
func MarkCurrentSuccess(deviceID string, at time.Time) {
	state.Lock()
	st := state.devices[deviceID]
	st.LastCurrentSuccess = at
	state.devices[deviceID] = st
	state.Unlock()
}

// MarkArchiveSuccess records confirmed useful archive work for one device.
func MarkArchiveSuccess(deviceID string, at time.Time) {
	state.Lock()
	st := state.devices[deviceID]
	st.LastArchiveSuccess = at
	state.devices[deviceID] = st
	state.Unlock()
}

// MarkESWriteSuccess records a successful INSERT/UPDATE of this device's
// value in Energosphere. Merely finding that a point already exists does
// not count as a new successful write.
func MarkESWriteSuccess(deviceID string, at time.Time) {
	state.Lock()
	st := state.devices[deviceID]
	st.LastESWriteSuccess = at
	state.devices[deviceID] = st
	state.Unlock()
}

// MarkPollStarted отмечает реальное начало задания poller для прибора.
// Поле нужно вкладке «Монитор опроса»: пока оно установлено, индикатор
// прибора зелёный.
func MarkPollStarted(deviceID, kind string, at time.Time) {
	state.Lock()
	st := state.devices[deviceID]
	st.PollInProgress = true
	st.PollStartedAt = at
	st.PollKind = kind
	state.devices[deviceID] = st
	state.Unlock()
}

// MarkPollFinished завершает текущее задание. recordResult=false нужен для
// служебного startup-backfill: он может успешно закончиться без новых строк,
// поэтому не должен подменять результат последнего обычного current/archive
// опроса ложным «неуспешно».
func MarkPollFinished(deviceID, kind string, at time.Time, ok bool, errText string, recordResult bool) {
	state.Lock()
	st := state.devices[deviceID]
	st.PollInProgress = false
	if recordResult {
		st.LastPollFinishedAt = at
		st.LastPollOK = ok
		st.LastPollKnown = true
		st.LastPollKind = kind
		st.LastPollError = errText
	}
	state.devices[deviceID] = st
	state.Unlock()
}

// SetNextPoll stores the exact next due time calculated by scheduler for
// this device. Zero means that no scheduled poll is currently known.
func SetNextPoll(deviceID string, at time.Time) {
	state.Lock()
	st := state.devices[deviceID]
	st.NextPollAt = at
	state.devices[deviceID] = st
	state.Unlock()
}

// SetSQLite records the latest explicit SQLite health check.
func SetSQLite(at time.Time, ok bool, err error) {
	state.Lock()
	state.sqliteCheckedAt = at
	state.sqliteOK = ok
	if err != nil {
		state.sqliteError = err.Error()
	} else {
		state.sqliteError = ""
	}
	state.Unlock()
}

// Get returns a copy so callers cannot mutate shared state.
func Get() Snapshot {
	state.RLock()
	defer state.RUnlock()

	devices := make(map[string]DeviceStatus, len(state.devices))
	for id, st := range state.devices {
		devices[id] = st
	}

	return Snapshot{
		PollerLastCycle: state.pollerLastCycle,
		Devices:         devices,
		SQLiteCheckedAt: state.sqliteCheckedAt,
		SQLiteOK:        state.sqliteOK,
		SQLiteError:     state.sqliteError,
	}
}
