package poller

import (
	"context"
	"testing"
	"time"

	"mbgw/internal/device"
	"mbgw/internal/scheduler"
)

func TestPollerNewOwnsDeviceRegistry(t *testing.T) {
	initial := make(map[string]*device.Device)
	p := New(scheduler.New(nil), initial, time.Second)

	dev, _ := newTestDevice(t, "late")
	initial["late"] = dev

	if p.HasDevice("late") {
		t.Fatal("poller unexpectedly observed caller map mutation; runtime devices must be added through AddDevice")
	}
	if !p.AddDevice("late", dev) {
		t.Fatal("AddDevice rejected a new runtime device")
	}
	if !p.HasDevice("late") {
		t.Fatal("AddDevice did not make the runtime device visible")
	}
}

func TestPollerAddDeviceWhileRunningDispatches(t *testing.T) {
	sched := scheduler.New(nil)
	p := New(sched, map[string]*device.Device{}, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()

	dev, cli := newTestDevice(t, "late")
	if !p.AddDevice("late", dev) {
		cancel()
		<-done
		t.Fatal("AddDevice rejected a new device while poller was running")
	}
	if p.AddDevice("late", dev) {
		cancel()
		<-done
		t.Fatal("AddDevice replaced an already registered device")
	}

	sched.RequestManualPoll("late", scheduler.KindCurrent)
	deadline := time.Now().Add(time.Second)
	for cli.Reads() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poller did not stop after cancellation")
	}

	if cli.Reads() != 1 {
		t.Fatalf("dynamically added device was polled %d times, want exactly 1", cli.Reads())
	}
}
