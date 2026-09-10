package scheduler

import "testing"

func TestScheduler_ManualArchiveKeepsDistinctKindAndManualPriority(t *testing.T) {
	s := New(nil)
	s.RequestManualPoll("ivk", KindManualArchive)

	task, ok := s.Next()
	if !ok {
		t.Fatal("expected manual archive task")
	}
	if task.DeviceID != "ivk" || task.Kind != KindManualArchive || task.Priority != PriorityManual {
		t.Fatalf("unexpected task: %+v", task)
	}
}
