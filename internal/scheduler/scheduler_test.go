package scheduler

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeEventRecorder struct {
	events []string
}

func (f *fakeEventRecorder) RecordSchedulerEvent(stage, detail string) {
	f.events = append(f.events, fmt.Sprintf("%s|%s", stage, detail))
}

func TestScheduler_Tick_EnqueuesDueTasks(t *testing.T) {
	s := New(nil)
	s.Register("dev1", 5*time.Second, 1*time.Hour, nil)

	// Registration sets both schedules "due now" - the first Tick should
	// enqueue both current and archive tasks.
	s.Tick(time.Now())

	if s.Len() != 2 {
		t.Fatalf("expected 2 tasks enqueued (current+archive), got %d", s.Len())
	}
}

func TestScheduler_Tick_NotYetDue(t *testing.T) {
	s := New(nil)
	now := time.Now()
	s.Register("dev1", 5*time.Second, 1*time.Hour, nil)
	s.Tick(now) // consumes the initial "due now" state

	// Drain whatever was enqueued by the first tick.
	for {
		if _, ok := s.Next(); !ok {
			break
		}
	}

	// A tick 1 second later should enqueue nothing (current interval is 5s).
	s.Tick(now.Add(1 * time.Second))
	if s.Len() != 0 {
		t.Fatalf("expected no tasks enqueued before the interval elapses, got %d", s.Len())
	}

	// A tick 5+ seconds later should enqueue the current-value task.
	s.Tick(now.Add(6 * time.Second))
	if s.Len() != 1 {
		t.Fatalf("expected 1 task (current) enqueued once the interval elapses, got %d", s.Len())
	}
	task, ok := s.Next()
	if !ok || task.Kind != KindCurrent || task.DeviceID != "dev1" {
		t.Fatalf("expected a current task for dev1, got %+v (ok=%v)", task, ok)
	}
}

func TestScheduler_ManualPoll_JumpsQueue(t *testing.T) {
	s := New(nil)
	s.Register("dev1", 0, 0, nil) // no automatic schedule

	// Enqueue a normal-priority task first, then a manual one - manual
	// must come out first despite being enqueued second.
	s.RequestManualPoll("dev2", KindCurrent) // will be popped first regardless
	s.mu.Lock()
	s.enqueueLocked(&Task{DeviceID: "dev1", Kind: KindCurrent, Priority: PriorityNormal})
	s.mu.Unlock()

	first, ok := s.Next()
	if !ok {
		t.Fatal("expected a task")
	}
	if first.Priority != PriorityManual || first.DeviceID != "dev2" {
		t.Fatalf("expected the manual task first, got %+v", first)
	}

	second, ok := s.Next()
	if !ok || second.DeviceID != "dev1" {
		t.Fatalf("expected the normal task second, got %+v (ok=%v)", second, ok)
	}
}

func TestScheduler_QueueOverflow_EvictsLowerPriority(t *testing.T) {
	events := &fakeEventRecorder{}
	s := New(events)

	// Fill the queue to capacity with low-priority tasks.
	for i := 0; i < MaxQueueDepth; i++ {
		s.mu.Lock()
		s.enqueueLocked(&Task{DeviceID: fmt.Sprintf("dev%d", i), Kind: KindCurrent, Priority: PriorityLow})
		s.mu.Unlock()
	}
	if s.Len() != MaxQueueDepth {
		t.Fatalf("expected queue full at %d, got %d", MaxQueueDepth, s.Len())
	}

	// A higher-priority manual task must evict a low-priority one, not be
	// dropped itself, and the queue must stay at MaxQueueDepth (not grow).
	s.RequestManualPoll("urgent", KindCurrent)
	if s.Len() != MaxQueueDepth {
		t.Fatalf("expected queue to stay at cap %d after an eviction, got %d", MaxQueueDepth, s.Len())
	}

	found := false
	for {
		task, ok := s.Next()
		if !ok {
			break
		}
		if task.DeviceID == "urgent" {
			found = true
			break // it must be first out, since it's highest priority
		}
	}
	if !found {
		t.Fatal("expected the manual task to have evicted a low-priority task and be present in the queue")
	}

	sawEviction := false
	for _, e := range events.events {
		if strings.HasPrefix(e, "queue_overflow_evicted|") {
			sawEviction = true
		}
	}
	if !sawEviction {
		t.Fatalf("expected a queue_overflow_evicted event, got: %v", events.events)
	}
}

func TestScheduler_QueueOverflow_DropsNewLowPriorityTask(t *testing.T) {
	events := &fakeEventRecorder{}
	s := New(events)

	// Fill the queue with NORMAL priority tasks (not the lowest possible).
	for i := 0; i < MaxQueueDepth; i++ {
		s.mu.Lock()
		s.enqueueLocked(&Task{DeviceID: fmt.Sprintf("dev%d", i), Kind: KindCurrent, Priority: PriorityNormal})
		s.mu.Unlock()
	}

	// A LOW priority task arriving now must be dropped (not evict a
	// normal-priority task), since it does not outrank anything queued.
	s.mu.Lock()
	s.enqueueLocked(&Task{DeviceID: "low", Kind: KindCurrent, Priority: PriorityLow})
	s.mu.Unlock()

	if s.Len() != MaxQueueDepth {
		t.Fatalf("expected queue to remain at cap %d, got %d", MaxQueueDepth, s.Len())
	}

	foundLow := false
	for {
		task, ok := s.Next()
		if !ok {
			break
		}
		if task.DeviceID == "low" {
			foundLow = true
		}
	}
	if foundLow {
		t.Fatal("expected the new low-priority task to have been dropped, not enqueued")
	}

	sawDrop := false
	for _, e := range events.events {
		if strings.HasPrefix(e, "queue_overflow_dropped_new") {
			sawDrop = true
		}
	}
	if !sawDrop {
		t.Fatalf("expected a queue_overflow_dropped_new event, got: %v", events.events)
	}
}

func TestScheduler_RequestRetry_DefersNextPoll(t *testing.T) {
	s := New(nil)
	now := time.Now()
	s.Register("dev1", 1*time.Second, 0, nil)
	s.Tick(now) // consume the initial due-now state
	for {
		if _, ok := s.Next(); !ok {
			break
		}
	}

	// Defer the next current-value poll by 10 seconds.
	s.RequestRetry("dev1", KindCurrent, 10*time.Second)

	// A tick 2 seconds later (past the original 1s interval, but before
	// the 10s deferral) must NOT enqueue anything.
	s.Tick(now.Add(2 * time.Second))
	if s.Len() != 0 {
		t.Fatalf("expected no task before the deferred retry delay elapses, got %d", s.Len())
	}

	s.Tick(now.Add(11 * time.Second))
	if s.Len() != 1 {
		t.Fatalf("expected the task to be enqueued after the deferred delay, got %d", s.Len())
	}
}

func TestScheduler_ArchiveWindow_OutsideWindow_NoPoll(t *testing.T) {
	s := New(nil)
	win := &Window{StartHour: 2, StartMinute: 0, EndHour: 4, EndMinute: 0}
	s.Register("dev1", 0, 1*time.Hour, win)

	noon := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.Tick(noon)
	if s.Len() != 0 {
		t.Fatalf("expected no archive task outside the window, got %d", s.Len())
	}
}

func TestScheduler_ArchiveWindow_InsideWindow_Polls(t *testing.T) {
	s := New(nil)
	win := &Window{StartHour: 2, StartMinute: 0, EndHour: 4, EndMinute: 0}
	s.Register("dev1", 0, 1*time.Hour, win)

	threeAM := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	s.Tick(threeAM)
	if s.Len() != 1 {
		t.Fatalf("expected an archive task inside the window, got %d", s.Len())
	}
	task, ok := s.Next()
	if !ok || task.Kind != KindArchive {
		t.Fatalf("expected an archive task, got %+v (ok=%v)", task, ok)
	}
}

func TestScheduler_Next_EmptyQueue(t *testing.T) {
	s := New(nil)
	if _, ok := s.Next(); ok {
		t.Fatal("expected ok=false for an empty queue")
	}
}

// TestNextArchiveAnchored verifies the hour-anchored next-due calculation
// used to stop the archive poll from drifting with process start time
// (the 2026-07-30 zero-delta-hour bug). All cases here use interval=1h
// (Akron's real interval) — see TestNextArchiveAnchored_HalfHourInterval
// below for the ВКМ 30-минутный случай, добавленный после того, как
// выяснилось, что старая версия функции игнорировала interval и всегда
// прибавляла ровно час (2026-08-26).
func TestNextArchiveAnchored(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		now      time.Time
		atMinute int
		want     time.Time
	}{
		// Before the anchor minute this hour → same hour's anchor.
		{time.Date(2026, 7, 30, 13, 2, 0, 0, loc), 5, time.Date(2026, 7, 30, 13, 5, 0, 0, loc)},
		// Exactly on the anchor → next hour (strictly after now).
		{time.Date(2026, 7, 30, 13, 5, 0, 0, loc), 5, time.Date(2026, 7, 30, 14, 5, 0, 0, loc)},
		// After the anchor minute → next hour's anchor.
		{time.Date(2026, 7, 30, 13, 40, 0, 0, loc), 5, time.Date(2026, 7, 30, 14, 5, 0, 0, loc)},
		// Anchor 0 (top of hour) from mid-hour → next hour :00.
		{time.Date(2026, 7, 30, 13, 30, 0, 0, loc), 0, time.Date(2026, 7, 30, 14, 0, 0, 0, loc)},
		// Wraps across midnight.
		{time.Date(2026, 7, 30, 23, 50, 0, 0, loc), 5, time.Date(2026, 7, 31, 0, 5, 0, 0, loc)},
	}
	for i, c := range cases {
		got := nextArchiveAnchored(c.now, c.atMinute, time.Hour)
		if !got.Equal(c.want) {
			t.Errorf("case %d: nextArchiveAnchored(%v, %d, 1h) = %v, want %v", i, c.now, c.atMinute, got, c.want)
		}
	}
}

// TestNextArchiveAnchored_HalfHourInterval — прямая проверка фикса от
// 2026-08-26: с interval=30 минут якорь должен продвигаться каждые 30
// минут (HH:05, HH:35, (HH+1):05, ...), а не перепрыгивать сразу на час,
// как это было в старой реализации (жёстко зашитый Add(time.Hour) вместо
// Add(interval)) — именно это и было настоящей причиной систематической
// потери получасовки ":00" у ВКМ даже после того, как device-уровневый
// archiveInterval был изменён на 30 минут.
func TestNextArchiveAnchored_HalfHourInterval(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		now      time.Time
		atMinute int
		want     time.Time
	}{
		// До якоря :05 в текущем получасе → якорь в этом же получасе.
		{time.Date(2026, 8, 26, 13, 2, 0, 0, loc), 5, time.Date(2026, 8, 26, 13, 5, 0, 0, loc)},
		// Сразу после :05 → следующий якорь через 30 минут, :35, НЕ через
		// час (:14:05) — именно это раньше ломалось.
		{time.Date(2026, 8, 26, 13, 10, 0, 0, loc), 5, time.Date(2026, 8, 26, 13, 35, 0, 0, loc)},
		// После :35 → следующий якорь в следующем часе, :05.
		{time.Date(2026, 8, 26, 13, 40, 0, 0, loc), 5, time.Date(2026, 8, 26, 14, 5, 0, 0, loc)},
		// Ровно на якоре :35 → следующий (строго после now) через 30 мин.
		{time.Date(2026, 8, 26, 13, 35, 0, 0, loc), 5, time.Date(2026, 8, 26, 14, 5, 0, 0, loc)},
	}
	for i, c := range cases {
		got := nextArchiveAnchored(c.now, c.atMinute, 30*time.Minute)
		if !got.Equal(c.want) {
			t.Errorf("case %d: nextArchiveAnchored(%v, %d, 30m) = %v, want %v", i, c.now, c.atMinute, got, c.want)
		}
	}
}

// TestScheduler_ArchiveAnchor_DoesNotDrift confirms that with an anchor
// set, consecutive archive due-times land on the fixed minute regardless
// of the (arbitrary) minute at which polling actually happens.
func TestScheduler_ArchiveAnchor_DoesNotDrift(t *testing.T) {
	s := New(nil)
	// current disabled, archive hourly, anchored to HH:05.
	s.RegisterWithArchiveAnchor("dev1", 0, time.Hour, nil, 5)

	// First tick at an arbitrary minute (13:47) — first poll is due
	// immediately (nextArchiveDue starts at zero time).
	s.Tick(time.Date(2026, 7, 30, 13, 47, 0, 0, time.UTC))
	task, ok := s.Next()
	if !ok || task.Kind != KindArchive {
		t.Fatalf("expected an archive task on first tick, got ok=%v task=%+v", ok, task)
	}

	// Next due must be 14:05, NOT 14:47 (would be interval-drift).
	ds := s.devices["dev1"]
	want := time.Date(2026, 7, 30, 14, 5, 0, 0, time.UTC)
	if !ds.nextArchiveDue.Equal(want) {
		t.Fatalf("nextArchiveDue = %v, want %v (anchored, not drifted to :47)", ds.nextArchiveDue, want)
	}
}

// TestScheduler_ArchiveAnchor_HalfHourInterval_DoesNotSkipPeriods —
// регрессионный тест на реальный производственный баг (2026-08-25/26):
// прибор ВКМ360 с archiveInterval=30 минут и включённым якорем терял
// каждую вторую получасовку, потому что заякоренный режим планировщика
// жёстко использовал шаг в час независимо от archiveInterval. Проверяет,
// что ПОСЛЕДОВАТЕЛЬНЫЕ срабатывания идут каждые 30 минут, а не раз в час.
func TestScheduler_ArchiveAnchor_HalfHourInterval_DoesNotSkipPeriods(t *testing.T) {
	s := New(nil)
	s.RegisterWithArchiveAnchor("dev1", 0, 30*time.Minute, nil, 5)

	start := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
	s.Tick(start)
	if _, ok := s.Next(); !ok {
		t.Fatal("expected an archive task on first tick")
	}

	ds := s.devices["dev1"]
	firstDue := ds.nextArchiveDue

	// Продвигаемся до firstDue и чуть дальше — должно сработать.
	s.Tick(firstDue.Add(time.Second))
	if _, ok := s.Next(); !ok {
		t.Fatal("expected an archive task at the first anchored due time")
	}
	secondDue := ds.nextArchiveDue

	gap := secondDue.Sub(firstDue)
	if gap != 30*time.Minute {
		t.Fatalf("expected consecutive archive due-times 30 minutes apart, got %v apart (firstDue=%v, secondDue=%v) — half-hour periods are being skipped again", gap, firstDue, secondDue)
	}
}

func TestNextPollAtReturnsEarliestLiveDueTime(t *testing.T) {
	s := New(nil)
	s.RegisterWithArchiveAnchor("dev1", 10*time.Minute, time.Hour, nil, 5)

	now := time.Date(2026, 9, 7, 14, 2, 0, 0, time.Local)
	s.Tick(now)

	next, ok := s.NextPollAt("dev1")
	if !ok {
		t.Fatal("ожидалось известное время следующего опроса")
	}
	want := time.Date(2026, 9, 7, 14, 5, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("следующий опрос = %v, ожидался ближайший current/archive %v", next, want)
	}
}

func TestManualCurrentQueuedBeforeBackfillKeepsFIFOOrder(t *testing.T) {
	s := New(nil)
	s.RequestManualPoll("dev1", KindCurrent)
	s.RequestManualPoll("dev1", KindBackfill)

	first, ok := s.Next()
	if !ok || first.DeviceID != "dev1" || first.Kind != KindCurrent {
		t.Fatalf("первым должен идти первичный current, получено %+v (ok=%v)", first, ok)
	}
	second, ok := s.Next()
	if !ok || second.DeviceID != "dev1" || second.Kind != KindBackfill {
		t.Fatalf("startup-backfill должен идти вторым, получено %+v (ok=%v)", second, ok)
	}
}
