package health

import (
	"testing"
	"time"
)

func TestPollMonitorLifecycle(t *testing.T) {
	deviceID := "test_poll_monitor_lifecycle"
	started := time.Date(2026, 9, 7, 15, 18, 13, 0, time.Local)
	next := started.Add(30 * time.Minute)
	finished := started.Add(2 * time.Second)

	SetNextPoll(deviceID, next)
	MarkPollStarted(deviceID, "current", started)

	st := Get().Devices[deviceID]
	if !st.PollInProgress {
		t.Fatal("после MarkPollStarted прибор должен быть отмечен как опрашиваемый")
	}
	if !st.PollStartedAt.Equal(started) || st.PollKind != "current" {
		t.Fatalf("неверное состояние старта: %+v", st)
	}
	if !st.NextPollAt.Equal(next) {
		t.Fatalf("неверное время следующего опроса: got %v want %v", st.NextPollAt, next)
	}

	MarkPollFinished(deviceID, "current", finished, false, "нет ответа прибора", true)
	st = Get().Devices[deviceID]
	if st.PollInProgress {
		t.Fatal("после MarkPollFinished прибор не должен оставаться активным")
	}
	if !st.LastPollKnown || st.LastPollOK {
		t.Fatalf("ожидался известный неуспешный результат: %+v", st)
	}
	if !st.LastPollFinishedAt.Equal(finished) || st.LastPollKind != "current" || st.LastPollError != "нет ответа прибора" {
		t.Fatalf("неверный результат завершения: %+v", st)
	}
	// Время начала последней операции сохраняется для таблицы монитора.
	if !st.PollStartedAt.Equal(started) {
		t.Fatalf("время начала было потеряно: got %v want %v", st.PollStartedAt, started)
	}
}

func TestBackfillDoesNotOverwriteLastRegularPollResult(t *testing.T) {
	deviceID := "test_backfill_result"
	regularFinished := time.Date(2026, 9, 7, 15, 0, 0, 0, time.Local)
	MarkPollFinished(deviceID, "current", regularFinished, true, "", true)

	MarkPollStarted(deviceID, "backfill", regularFinished.Add(time.Minute))
	MarkPollFinished(deviceID, "backfill", regularFinished.Add(2*time.Minute), true, "", false)

	st := Get().Devices[deviceID]
	if !st.LastPollKnown || !st.LastPollOK || st.LastPollKind != "current" || !st.LastPollFinishedAt.Equal(regularFinished) {
		t.Fatalf("startup-backfill не должен подменять результат обычного опроса: %+v", st)
	}
}

func TestPollProgressAdvancesWatchdogHeartbeat(t *testing.T) {
	deviceID := "test_poll_progress"
	started := time.Now().Add(-time.Minute)
	progress := time.Now()
	MarkPollStarted(deviceID, "backfill", started)
	MarkPollProgress(deviceID, progress)
	st := Get().Devices[deviceID]
	if !st.PollLastProgressAt.Equal(progress) {
		t.Fatalf("PollLastProgressAt = %v, want %v", st.PollLastProgressAt, progress)
	}
}

func TestQueuedPollState(t *testing.T) {
	deviceID := "test_queued_poll_state"
	t0 := time.Date(2026, 9, 8, 10, 0, 0, 0, time.Local)
	MarkPollQueued(deviceID, t0)
	MarkPollQueued(deviceID, t0.Add(time.Second))
	st := Get().Devices[deviceID]
	if st.PollQueueDepth != 2 || !st.PollQueuedAt.Equal(t0) {
		t.Fatalf("unexpected queued state: %+v", st)
	}
	MarkPollDequeued(deviceID, t0.Add(2*time.Second))
	st = Get().Devices[deviceID]
	if st.PollQueueDepth != 1 || !st.PollQueuedAt.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("unexpected state after first dequeue: %+v", st)
	}
	MarkPollDequeued(deviceID, t0.Add(3*time.Second))
	st = Get().Devices[deviceID]
	if st.PollQueueDepth != 0 || !st.PollQueuedAt.IsZero() {
		t.Fatalf("queue must be empty: %+v", st)
	}
}

func TestSchedulerDueStallKeepsFirstTimestamp(t *testing.T) {
	ClearSchedulerDueStall()
	t0 := time.Date(2026, 9, 8, 10, 0, 0, 0, time.Local)
	MarkSchedulerDueStall(t0)
	MarkSchedulerDueStall(t0.Add(time.Minute))
	if got := Get().SchedulerDueStallSince; !got.Equal(t0) {
		t.Fatalf("stall start moved: got %v want %v", got, t0)
	}
	ClearSchedulerDueStall()
	if got := Get().SchedulerDueStallSince; !got.IsZero() {
		t.Fatalf("stall marker not cleared: %v", got)
	}
}
