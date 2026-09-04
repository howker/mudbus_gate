package health

import (
	"sync"
	"time"
)

// DeviceStatus is the useful-work heartbeat for one configured meter.
// A timestamp is updated only after useful work was actually confirmed,
// not merely because a scheduled task started.
type DeviceStatus struct {
	LastCurrentSuccess time.Time
	LastArchiveSuccess time.Time
	LastESWriteSuccess time.Time
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
