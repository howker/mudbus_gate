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
