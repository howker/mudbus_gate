package monitor

import (
	"testing"
	"time"

	"mbgw/internal/channel"
	"mbgw/internal/scheduler"
)

func TestBus_PublishSubscribe_Delivery(t *testing.T) {
	b := NewBus(nil)
	ch, unsubscribe := b.Subscribe()
	defer unsubscribe()

	b.Publish(Event{DeviceID: "dev1", Stage: "down", Detail: "test"})

	select {
	case ev := <-ch:
		if ev.DeviceID != "dev1" || ev.Stage != "down" {
			t.Fatalf("unexpected event: %+v", ev)
		}
		if ev.Timestamp.IsZero() {
			t.Fatal("expected Publish to fill in a zero Timestamp")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the event to be delivered")
	}
}

func TestBus_Unsubscribe_StopsDelivery(t *testing.T) {
	b := NewBus(nil)
	ch, unsubscribe := b.Subscribe()
	unsubscribe()

	b.Publish(Event{DeviceID: "dev1", Stage: "down"})

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected no event after unsubscribe")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("expected the channel to be closed promptly after unsubscribe")
	}
}

func TestBus_MultipleSubscribers_AllReceive(t *testing.T) {
	b := NewBus(nil)
	ch1, unsub1 := b.Subscribe()
	ch2, unsub2 := b.Subscribe()
	defer unsub1()
	defer unsub2()

	b.Publish(Event{DeviceID: "dev1", Stage: "ok"})

	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case ev := <-ch:
			if ev.DeviceID != "dev1" {
				t.Fatalf("subscriber %d: unexpected event %+v", i, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out waiting for delivery", i)
		}
	}
}

func TestBus_FullSubscriberBuffer_DropsRatherThanBlocks(t *testing.T) {
	b := NewBus(nil)
	_, unsubscribe := b.Subscribe() // never drained
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		for i := 0; i < subscriberBufferSize+10; i++ {
			b.Publish(Event{DeviceID: "dev1", Stage: "spam"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer instead of dropping")
	}
}

// TestBus_SatisfiesChannelAndSchedulerEventRecorder is a compile-time
// proof (not just a comment claim) that *Bus can be used directly
// wherever channel.EventRecorder or scheduler.EventRecorder is expected,
// per bus.go's RecordChannelEvent/RecordSchedulerEvent doc comments.
func TestBus_SatisfiesChannelAndSchedulerEventRecorder(t *testing.T) {
	b := NewBus(nil)
	var _ channel.EventRecorder = b
	var _ scheduler.EventRecorder = b

	ch, unsubscribe := b.Subscribe()
	defer unsubscribe()

	b.RecordChannelEvent("dev1", "primary", "degraded", "3 failures")
	select {
	case ev := <-ch:
		if ev.DeviceID != "dev1" || ev.ChannelID != "primary" || ev.Stage != "degraded" {
			t.Fatalf("unexpected event from RecordChannelEvent: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for RecordChannelEvent's event")
	}

	b.RecordSchedulerEvent("queue_overflow_evicted", "current:dev2")
	select {
	case ev := <-ch:
		if ev.Stage != "queue_overflow_evicted" {
			t.Fatalf("unexpected event from RecordSchedulerEvent: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for RecordSchedulerEvent's event")
	}
}
