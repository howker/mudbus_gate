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
