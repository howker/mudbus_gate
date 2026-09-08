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

	PollInProgress     bool
	PollStartedAt      time.Time
	PollLastProgressAt time.Time
	PollKind           string

	// PollQueueDepth/PollQueuedAt показывают задания, уже переданные
	// диспетчером в FIFO прибора, но ещё не начавшие фактический опрос.
	// Это позволяет watchdog отличить нормальное ожидание расписания от
	// зависшей очереди worker-а.
	PollQueueDepth int
	PollQueuedAt   time.Time

	LastPollFinishedAt time.Time
	LastPollOK         bool
	LastPollKnown      bool
	LastPollKind       string
	LastPollError      string

	NextPollAt time.Time
}

// Snapshot is a point-in-time copy safe for the Web/API layer.
type Snapshot struct {
	PollerLastCycle        time.Time
	PollerLastDispatch     time.Time
	SchedulerDueStallSince time.Time
	Devices                map[string]DeviceStatus

	SQLiteCheckedAt time.Time
	SQLiteOK        bool
	SQLiteError     string
}

var state struct {
	sync.RWMutex

	pollerLastCycle        time.Time
	pollerLastDispatch     time.Time
	schedulerDueStallSince time.Time
	devices                map[string]DeviceStatus

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

// MarkPollerDispatch отмечает, что диспетчер действительно забрал задание
// из scheduler и передал его в FIFO прибора. Это более сильный признак
// жизни, чем один только тик центрального цикла.
func MarkPollerDispatch(at time.Time) {
	state.Lock()
	state.pollerLastDispatch = at
	state.Unlock()
}

// MarkSchedulerDueStall фиксирует момент, когда планировщик уже видел
// просроченную плановую работу, но после Tick не отдал ни одного задания.
// Повторные вызовы не сдвигают начало — watchdog должен видеть длительность.
func MarkSchedulerDueStall(at time.Time) {
	state.Lock()
	if state.schedulerDueStallSince.IsZero() {
		state.schedulerDueStallSince = at
	}
	state.Unlock()
}

func ClearSchedulerDueStall() {
	state.Lock()
	state.schedulerDueStallSince = time.Time{}
	state.Unlock()
}

// MarkPollQueued/MarkPollDequeued ведут минимальное состояние FIFO прибора.
// Время первой ожидающей задачи не сдвигается при добавлении следующих.
func MarkPollQueued(deviceID string, at time.Time) {
	state.Lock()
	st := state.devices[deviceID]
	if st.PollQueueDepth == 0 {
		st.PollQueuedAt = at
	}
	st.PollQueueDepth++
	state.devices[deviceID] = st
	state.Unlock()
}

func MarkPollDequeued(deviceID string, at time.Time) {
	state.Lock()
	st := state.devices[deviceID]
	if st.PollQueueDepth > 0 {
		st.PollQueueDepth--
	}
	if st.PollQueueDepth == 0 {
		st.PollQueuedAt = time.Time{}
	} else {
		// Точный возраст следующей задачи нам не известен; at — безопасная
		// нижняя граница, исключающая ложное объявление зависания.
		st.PollQueuedAt = at
	}
	state.devices[deviceID] = st
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
	st.PollLastProgressAt = at
	st.PollKind = kind
	state.devices[deviceID] = st
	state.Unlock()
}

// MarkPollProgress отмечает продвижение уже начатой длительной операции.
// Дозабор архива вызывает её после каждой реально обработанной записи/страницы,
// чтобы watchdog не принял долгую, но живую работу за зависание.
func MarkPollProgress(deviceID string, at time.Time) {
	state.Lock()
	st := state.devices[deviceID]
	if st.PollInProgress {
		st.PollLastProgressAt = at
		state.devices[deviceID] = st
	}
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
	st.PollLastProgressAt = at
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
		PollerLastCycle:        state.pollerLastCycle,
		PollerLastDispatch:     state.pollerLastDispatch,
		SchedulerDueStallSince: state.schedulerDueStallSince,
		Devices:                devices,
		SQLiteCheckedAt:        state.sqliteCheckedAt,
		SQLiteOK:               state.sqliteOK,
		SQLiteError:            state.sqliteError,
	}
}
