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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[deviceID] = &deviceSchedule{
		deviceID:        deviceID,
		currentInterval: currentInterval,
		archiveInterval: archiveInterval,
		nextCurrentDue:  time.Time{},
		nextArchiveDue:  time.Time{},
		archiveWindow:   archiveWindow,
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
		if ds.archiveInterval > 0 && !now.Before(ds.nextArchiveDue) {
			if ds.archiveWindow == nil || ds.archiveWindow.contains(now) {
				s.enqueueLocked(&Task{DeviceID: ds.deviceID, Kind: KindArchive, Priority: PriorityNormal})
				ds.nextArchiveDue = now.Add(ds.archiveInterval)
			} else {
				// Outside the configured window - defer until it next
				// opens, rather than enqueueing now or re-checking every
				// single Tick.
				ds.nextArchiveDue = ds.archiveWindow.nextStart(now)
			}
		}
	}
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
