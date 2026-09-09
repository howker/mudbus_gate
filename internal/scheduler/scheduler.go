package scheduler

import (
	"container/heap"
	"fmt"
	"sync"
	"time"
)

// Package scheduler implements FINAL_TRD.md section 5.3's polling
// scheduler: separate schedules for current values and archives per
// device, manual polls that jump the queue, deferred retry after error,
// and optional night windows for archive catch-up - per a bounded
// priority queue (LLD.md: "ограничение глубины очереди (10000)").

// Kind distinguishes a current-value poll from an archive poll - they
// run on separate schedules (FINAL_TRD.md section 5.3: "раздельное
// расписание для текущих данных и архивов").
type Kind string

const (
	KindCurrent Kind = "current"
	KindArchive Kind = "archive"
	// KindBackfill is a deep archive catch-up (device.BackfillArchives),
	// as opposed to KindArchive's regular near-term read
	// (device.PollArchives). Routed through the same single-threaded
	// poller dispatch as everything else — a device's transport is not
	// safe for concurrent use (see internal/poller's package doc), so
	// this must NEVER be run in a standalone goroutine alongside a
	// current-value or regular-archive poll. That happened for real on
	// 2026-07-30: the manual "poll now" button spawned `go
	// dev.BackfillArchives(...)` next to the scheduler's own concurrent
	// KindCurrent dispatch, and two simultaneous reads on the same COM
	// port produced garbled bytes on both sides (invalid CRC, invalid
	// BCD, "rtu frame too short").
	KindBackfill Kind = "backfill"
)

// Priority determines queue ordering; higher values are served first.
type Priority int

const (
	PriorityLow    Priority = 0
	PriorityNormal Priority = 5
	// PriorityManual is used for operator-requested polls (FINAL_TRD.md
	// section 5.3: "ручной опрос по запросу оператора"), which must jump
	// ahead of regular scheduled polls already queued.
	PriorityManual Priority = 10
)

// MaxQueueDepth is the hard cap on queued tasks, per LLD.md's
// internal/scheduler spec.
const MaxQueueDepth = 10000

// Task is one unit of scheduled work: read deviceID's current values or
// archive.
type Task struct {
	DeviceID string
	Kind     Kind
	Priority Priority

	enqueuedAt time.Time
	seq        int64 // tie-breaker: FIFO within equal priority
}

// EventRecorder receives scheduler events (queue overflow), a local
// interface (mirrors channel.EventRecorder's rationale) so this package
// does not depend on internal/monitor, which does not exist yet
// (IMPLEMENTATION_BACKLOG.md T12).
type EventRecorder interface {
	RecordSchedulerEvent(stage, detail string)
}

// NoopEventRecorder discards all events.
type NoopEventRecorder struct{}

func (NoopEventRecorder) RecordSchedulerEvent(stage, detail string) {}

// taskHeap implements container/heap.Interface, ordered by Priority
// descending then seq ascending (FIFO within equal priority) - so
// heap.Pop always returns the highest-priority, oldest-enqueued task.
type taskHeap []*Task

func (h taskHeap) Len() int { return len(h) }
func (h taskHeap) Less(i, j int) bool {
	if h[i].Priority != h[j].Priority {
		return h[i].Priority > h[j].Priority
	}
	return h[i].seq < h[j].seq
}
func (h taskHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *taskHeap) Push(x any)   { *h = append(*h, x.(*Task)) }
func (h *taskHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return item
}

// Window restricts archive polling to a daily HH:MM-HH:MM range, for
// FINAL_TRD.md section 5.3's "ночные окна дозабора архивов". Wraps
// midnight correctly if EndHour/EndMinute is earlier than Start
// (e.g. 22:00-06:00).
type Window struct {
	StartHour, StartMinute int
	EndHour, EndMinute     int
}

func (w Window) contains(t time.Time) bool {
	start := time.Date(t.Year(), t.Month(), t.Day(), w.StartHour, w.StartMinute, 0, 0, t.Location())
	end := time.Date(t.Year(), t.Month(), t.Day(), w.EndHour, w.EndMinute, 0, 0, t.Location())
	if end.Before(start) || end.Equal(start) {
		return !t.Before(start) || t.Before(end)
	}
	return !t.Before(start) && t.Before(end)
}

// nextStart returns the next time.Time at or after t when the window
// begins (today if it hasn't started yet, otherwise tomorrow).
func (w Window) nextStart(t time.Time) time.Time {
	start := time.Date(t.Year(), t.Month(), t.Day(), w.StartHour, w.StartMinute, 0, 0, t.Location())
	if start.Before(t) {
		start = start.Add(24 * time.Hour)
	}
	return start
}

type deviceSchedule struct {
	deviceID        string
	currentInterval time.Duration
	archiveInterval time.Duration
	nextCurrentDue  time.Time
	nextArchiveDue  time.Time
	archiveWindow   *Window

	archiveCalendar bool
	archiveEvery    int
	archiveDaysMask uint8 // bit0=Monday ... bit6=Sunday

	// archiveAtMinute, when >= 0, anchors the hourly archive poll to a
	// fixed minute past each hour (e.g. 5 → always HH:05) instead of
	// "start time + N*interval", which drifts to an arbitrary minute
	// depending on when the process happened to start. That drift caused
	// a real production bug (2026-07-30): the process started at ~10:15,
	// so archive polls landed at HH:40, but ЭС reads the "newest" archive
	// row at ~HH:08 — before HH:40 the freshest stored hour was still the
	// PREVIOUS hour, so ЭС got the same row twice and recorded a zero
	// delta for that hour. Anchoring to HH:05 means the current hour is
	// collected well before ЭС's ~HH:08 read. -1 = disabled (legacy
	// interval-only behaviour, kept for tests and non-Akron devices).
	archiveAtMinute int
}

// nextArchiveAnchored returns the next wall-clock time strictly after
// `now` that falls on archiveAtMinute past some hour-boundary-aligned
// step of size `interval`. E.g. atMinute=5, interval=1h, now=13:07 →
// 14:05; now=13:02 → 13:05.
//
// ВАЖНО (исправлено 2026-08-26): раньше здесь был жёстко зашит шаг РОВНО
// В ЧАС (candidate.Add(time.Hour)), независимо от того, что реально
// передано в interval — эта функция изначально писалась только под
// Akron (часовой архив), и при добавлении получасового опроса для ВКМ
// (archiveInterval=30 мин) полностью игнорировала эту настройку: любой
// прибор с archiveAtMinute>=0 (а это включено по умолчанию, =5) всё
// равно опрашивался РОВНО раз в час, что бы ни стояло в archiveInterval.
// Это и было настоящей причиной того, что ВКМ систематически терял
// получасовку ":00" даже ПОСЛЕ смены archiveInterval на 30 минут —
// прошлый фикс менял значение, которое этот планировщик в заякоренном
// режиме просто не читал (подтверждено живьём, 2026-08-26).
//
// Теперь шаг — сам interval (через цикл, чтобы корректно "перепрыгнуть"
// несколько пропущенных интервалов разом, если процесс был неактивен
// дольше одного шага) — при interval=1h ведёт себя ТОЧНО как раньше
// (один проход цикла эквивалентен старому одиночному +Add(time.Hour)),
// так что для Akron поведение не меняется.

// nextArchiveCalendar returns the next regular archive poll strictly after now.
// The cadence is anchored to local midnight, not process start, so restarts do
// not shift an every-N-period schedule. offsetMinutes is applied after each
// archive-period boundary. daysMask uses bit0=Monday ... bit6=Sunday; 0 means
// every day for backward compatibility.
func nextArchiveCalendar(now time.Time, period time.Duration, every int, daysMask uint8, window *Window, offsetMinutes int) time.Time {
	if period <= 0 {
		period = time.Hour
	}
	if every <= 0 {
		every = 1
	}
	if daysMask == 0 {
		daysMask = 0x7f
	}
	step := period * time.Duration(every)
	if step <= 0 {
		step = period
	}

	for dayOffset := 0; dayOffset < 14; dayOffset++ {
		day := time.Date(now.Year(), now.Month(), now.Day()+dayOffset, 0, 0, 0, 0, now.Location())
		weekdayBit := uint8(1 << ((int(day.Weekday()) + 6) % 7)) // Go Sunday=0 -> Monday bit0
		if daysMask&weekdayBit == 0 {
			continue
		}
		for boundary := day; boundary.Before(day.Add(24 * time.Hour)); boundary = boundary.Add(step) {
			candidate := boundary.Add(time.Duration(offsetMinutes) * time.Minute)
			if !candidate.After(now) {
				continue
			}
			if window != nil && !window.contains(candidate) {
				continue
			}
			return candidate
		}
	}
	return time.Time{}
}

func nextArchiveAnchored(now time.Time, atMinute int, interval time.Duration) time.Time {
	if interval <= 0 {
		interval = time.Hour
	}
	candidate := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), atMinute, 0, 0, now.Location())
	for !candidate.After(now) {
		candidate = candidate.Add(interval)
	}
	return candidate
}

// Scheduler produces polling Tasks from per-device schedules, plus
// operator-injected manual polls, via a single bounded priority queue.
type Scheduler struct {
	mu      sync.Mutex
	devices map[string]*deviceSchedule
	queue   taskHeap
	seq     int64
	events  EventRecorder
}

// New creates a Scheduler. events may be nil (defaults to
// NoopEventRecorder).
func New(events EventRecorder) *Scheduler {
	if events == nil {
		events = NoopEventRecorder{}
	}
	s := &Scheduler{devices: make(map[string]*deviceSchedule), events: events}
	heap.Init(&s.queue)
	return s
}

// Register configures deviceID's polling intervals. A zero interval
// disables that kind of polling for the device. archiveWindow may be nil
// (no restriction - archive polls run on interval alone, like current
// values). Both schedules start due at the zero time.Time (always in the
// past relative to any real timestamp), so the first Tick call - however
// far in wall-clock terms from when Register was called - correctly
// treats them as due. This deliberately does NOT use time.Now() here,
// so Register/Tick stay fully decoupled from wall-clock time, keeping
// the whole schedule deterministic and testable via Tick(now) alone.
func (s *Scheduler) Register(deviceID string, currentInterval, archiveInterval time.Duration, archiveWindow *Window) {
	s.RegisterWithArchiveAnchor(deviceID, currentInterval, archiveInterval, archiveWindow, -1)
}

// RegisterWithArchiveAnchor is Register plus archiveAtMinute: when >= 0,
// the hourly archive poll fires at that fixed minute past each hour
// (HH:archiveAtMinute) instead of drifting with the process start time.
// See deviceSchedule.archiveAtMinute for why this matters. archiveAtMinute
// < 0 preserves the legacy interval-only behaviour.
func (s *Scheduler) RegisterWithArchiveAnchor(deviceID string, currentInterval, archiveInterval time.Duration, archiveWindow *Window, archiveAtMinute int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[deviceID] = &deviceSchedule{
		deviceID:        deviceID,
		currentInterval: currentInterval,
		archiveInterval: archiveInterval,
		nextCurrentDue:  time.Time{},
		nextArchiveDue:  time.Time{},
		archiveWindow:   archiveWindow,
		archiveAtMinute: archiveAtMinute,
	}
}

// RegisterArchiveCalendar configures the production archive schedule. Unlike
// the legacy Register* methods, the first regular archive task is NOT due
// immediately: the first Tick calculates the next calendar boundary. Startup
// catch-up is a separate KindBackfill task queued explicitly by the server.
func (s *Scheduler) RegisterArchiveCalendar(deviceID string, archivePeriod time.Duration, every int, daysMask uint8, archiveWindow *Window, offsetMinutes int) {
	if every <= 0 {
		every = 1
	}
	if daysMask == 0 {
		daysMask = 0x7f
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[deviceID] = &deviceSchedule{
		deviceID: deviceID, archiveInterval: archivePeriod, archiveWindow: archiveWindow,
		archiveAtMinute: offsetMinutes, archiveCalendar: true, archiveEvery: every, archiveDaysMask: daysMask,
	}
}

// Tick checks every registered device's due times against now and
// enqueues any tasks that have become due. Callers (the Poller) are
// expected to call this periodically (e.g. every second) - Tick itself
// runs no internal timer, keeping this package free of goroutine
// lifecycle concerns (consistent with LLD.md's "Запрещено: protocol/
// transport" - Scheduler doesn't own I/O timing either, just logical due
// times).
func (s *Scheduler) Tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, ds := range s.devices {
		if ds.currentInterval > 0 && !now.Before(ds.nextCurrentDue) {
			s.enqueueLocked(&Task{DeviceID: ds.deviceID, Kind: KindCurrent, Priority: PriorityNormal})
			ds.nextCurrentDue = now.Add(ds.currentInterval)
		}
		if ds.archiveInterval > 0 {
			if ds.archiveCalendar {
				if ds.nextArchiveDue.IsZero() {
					ds.nextArchiveDue = nextArchiveCalendar(now, ds.archiveInterval, ds.archiveEvery, ds.archiveDaysMask, ds.archiveWindow, ds.archiveAtMinute)
				} else if !now.Before(ds.nextArchiveDue) {
					s.enqueueLocked(&Task{DeviceID: ds.deviceID, Kind: KindArchive, Priority: PriorityNormal})
					ds.nextArchiveDue = nextArchiveCalendar(now, ds.archiveInterval, ds.archiveEvery, ds.archiveDaysMask, ds.archiveWindow, ds.archiveAtMinute)
				}
			} else if !now.Before(ds.nextArchiveDue) {
				if ds.archiveWindow == nil || ds.archiveWindow.contains(now) {
					s.enqueueLocked(&Task{DeviceID: ds.deviceID, Kind: KindArchive, Priority: PriorityNormal})
					if ds.archiveAtMinute >= 0 {
						ds.nextArchiveDue = nextArchiveAnchored(now, ds.archiveAtMinute, ds.archiveInterval)
					} else {
						ds.nextArchiveDue = now.Add(ds.archiveInterval)
					}
				} else {
					ds.nextArchiveDue = ds.archiveWindow.nextStart(now)
				}
			}
		}
	}
}

// NextPollAt returns the exact earliest next scheduled current/archive poll
// for deviceID according to the scheduler's live due-times. Manual tasks are
// intentionally not included: they are already queued work, not the next
// planned poll. Zero/false means the device is unknown or has no enabled
// schedule.
func (s *Scheduler) NextPollAt(deviceID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ds, ok := s.devices[deviceID]
	if !ok {
		return time.Time{}, false
	}

	var next time.Time
	if ds.currentInterval > 0 && !ds.nextCurrentDue.IsZero() {
		next = ds.nextCurrentDue
	}
	if ds.archiveInterval > 0 && !ds.nextArchiveDue.IsZero() {
		if next.IsZero() || ds.nextArchiveDue.Before(next) {
			next = ds.nextArchiveDue
		}
	}
	if next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}

// RequestManualPoll injects a high-priority task for an operator-requested
// poll (FINAL_TRD.md section 5.3), jumping ahead of normal-priority tasks
// already queued.
func (s *Scheduler) RequestManualPoll(deviceID string, kind Kind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueueLocked(&Task{DeviceID: deviceID, Kind: kind, Priority: PriorityManual})
}

// RequestRetry defers deviceID's next regular kind poll by delay
// (FINAL_TRD.md section 5.3: "отложенный повтор после ошибки"), instead
// of enqueueing another attempt immediately - avoids hammering a device
// that just failed. No-op if deviceID was never Register-ed.
func (s *Scheduler) RequestRetry(deviceID string, kind Kind, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ds, ok := s.devices[deviceID]
	if !ok {
		return
	}
	due := time.Now().Add(delay)
	switch kind {
	case KindCurrent:
		ds.nextCurrentDue = due
	case KindArchive:
		ds.nextArchiveDue = due
	}
}

// enqueueLocked pushes t onto the queue, enforcing MaxQueueDepth: if full,
// the lowest-priority task currently queued is evicted to make room for
// t, UNLESS t's own priority is not higher than that lowest task's (in
// which case t itself is dropped instead) - per FINAL_TRD.md section 6:
// "при переполнении — отбрасывание по нижнему приоритету с записью
// события." Either outcome is logged. Must be called with s.mu held.
func (s *Scheduler) enqueueLocked(t *Task) {
	t.enqueuedAt = time.Now()
	s.seq++
	t.seq = s.seq

	if len(s.queue) >= MaxQueueDepth {
		lowestIdx := 0
		for i, existing := range s.queue {
			if existing.Priority < s.queue[lowestIdx].Priority {
				lowestIdx = i
			}
		}
		lowest := s.queue[lowestIdx]

		if t.Priority <= lowest.Priority {
			s.events.RecordSchedulerEvent("queue_overflow_dropped_new", taskLabel(t))
			return
		}
		heap.Remove(&s.queue, lowestIdx)
		s.events.RecordSchedulerEvent("queue_overflow_evicted", taskLabel(lowest))
	}

	heap.Push(&s.queue, t)
}

// Next pops and returns the highest-priority, oldest-enqueued task
// currently queued. ok is false if the queue is empty.
func (s *Scheduler) Next() (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return Task{}, false
	}
	t := heap.Pop(&s.queue).(*Task)
	return *t, true
}

// Len returns the current queue depth.
func (s *Scheduler) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

func taskLabel(t *Task) string {
	return fmt.Sprintf("%s:%s", t.Kind, t.DeviceID)
}
