package watchdog

import (
	"testing"
	"time"

	"mbgw/internal/health"
)

func TestOneStuckDeviceDoesNotRestartGateway(t *testing.T) {
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.Local)
	s := health.Snapshot{PollerLastCycle: now.Add(-time.Second), Devices: map[string]health.DeviceStatus{
		"a": {PollInProgress: true, PollStartedAt: now.Add(-20 * time.Minute), PollLastProgressAt: now.Add(-20 * time.Minute)},
		"b": {PollInProgress: false},
	}}
	r := Evaluate(now, now.Add(-time.Hour), 10*time.Minute, []string{"a", "b"}, s)
	if r.GlobalStall || len(r.StuckDevices) != 1 || r.StuckDevices[0] != "a" {
		t.Fatalf("unexpected result: %#v", r)
	}
}

func TestAllDevicesStuckIsGlobal(t *testing.T) {
	now := time.Now()
	old := now.Add(-11 * time.Minute)
	s := health.Snapshot{PollerLastCycle: now, Devices: map[string]health.DeviceStatus{
		"a": {PollInProgress: true, PollStartedAt: old, PollLastProgressAt: old},
		"b": {PollInProgress: true, PollStartedAt: old, PollLastProgressAt: old},
	}}
	r := Evaluate(now, now.Add(-time.Hour), 10*time.Minute, []string{"a", "b"}, s)
	if !r.GlobalStall || r.PollerStall {
		t.Fatalf("unexpected result: %#v", r)
	}
}

func TestStalePollerIsGlobalButStartupHasGrace(t *testing.T) {
	now := time.Now()
	s := health.Snapshot{Devices: map[string]health.DeviceStatus{}}
	if r := Evaluate(now, now.Add(-5*time.Minute), 10*time.Minute, nil, s); r.GlobalStall {
		t.Fatalf("startup grace must suppress alert: %#v", r)
	}
	if r := Evaluate(now, now.Add(-20*time.Minute), 10*time.Minute, nil, s); !r.GlobalStall || !r.PollerStall {
		t.Fatalf("stale poller must be global: %#v", r)
	}
}

func TestQueuedAllDevicesIsGlobal(t *testing.T) {
	now := time.Now()
	old := now.Add(-11 * time.Minute)
	s := health.Snapshot{PollerLastCycle: now, Devices: map[string]health.DeviceStatus{
		"a": {PollQueueDepth: 1, PollQueuedAt: old},
		"b": {PollQueueDepth: 2, PollQueuedAt: old},
	}}
	r := Evaluate(now, now.Add(-time.Hour), 10*time.Minute, []string{"a", "b"}, s)
	if !r.GlobalStall || len(r.StuckDevices) != 2 {
		t.Fatalf("queued workers must be detected as global stall: %#v", r)
	}
}

func TestOldPlannedTimeAloneDoesNotTriggerGlobalRestart(t *testing.T) {
	now := time.Now()
	// Центральный цикл жив. Сам факт, что расписание когда-то было due,
	// не является отдельным watchdog-сигналом: scheduler.Tick переносит
	// due вперёд, а реальная очередь контролируется по каждому прибору.
	s := health.Snapshot{
		PollerLastCycle: now,
		Devices: map[string]health.DeviceStatus{
			"a": {PollInProgress: false, PollQueueDepth: 0},
		},
	}
	r := Evaluate(now, now.Add(-time.Hour), 10*time.Minute, []string{"a"}, s)
	if r.GlobalStall || len(r.StuckDevices) != 0 {
		t.Fatalf("живой scheduler без зависшей FIFO не должен перезапускать шлюз: %#v", r)
	}
}

func TestHealthyLongOperationMayHaveOldQueuedWork(t *testing.T) {
	now := time.Now()
	s := health.Snapshot{PollerLastCycle: now, Devices: map[string]health.DeviceStatus{
		"a": {
			PollInProgress:     true,
			PollStartedAt:      now.Add(-30 * time.Minute),
			PollLastProgressAt: now.Add(-time.Second),
			PollQueueDepth:     3,
			PollQueuedAt:       now.Add(-20 * time.Minute),
		},
	}}
	r := Evaluate(now, now.Add(-time.Hour), 10*time.Minute, []string{"a"}, s)
	if r.GlobalStall || len(r.StuckDevices) != 0 {
		t.Fatalf("живой длительный опрос не должен считаться зависшим только из-за старой очереди: %#v", r)
	}
}
