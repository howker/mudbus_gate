package monitor

import (
	"sync"
	"time"
)

// Package monitor implements a comm_events-shaped event bus (LLD.md's
// internal/monitor spec): Bus.Publish/Subscribe for real-time delivery,
// feeding the future SSE endpoint (T13).
//
// Alarm rules/acknowledgment (also named in LLD.md's DoD for this
// package) are deliberately NOT implemented here - deferred by explicit
// decision, pending a real answer to "do we even need SCADA-style
// alarms for a system whose devices are mostly polled hourly/daily over
// modem, versus a simpler 'N consecutive failures' threshold on top of
// the existing retry/backoff". See docs/ADR.md ADR-003.

// Persister optionally stores events durably (the comm_events domain
// entity, FINAL_TRD.md section 7). This is a local interface, not
// internal/storage.Repo directly: internal/storage's models.go/migrations
// (marked Core/protected per T9's "Запрещено: менять storage.go/
// models.go/migrations/*.sql") do not yet define a comm_events table -
// adding one is a follow-up, not something this package should force by
// depending on storage directly today. NoopPersister is the default
// until that table exists.
type Persister interface {
	PersistEvent(Event)
}

// NoopPersister discards all events (no durable storage yet).
type NoopPersister struct{}

func (NoopPersister) PersistEvent(Event) {}

// subscriberBufferSize bounds each subscriber's event channel. A slow
// subscriber that falls behind has new events dropped (not blocking
// Publish, and not blocking other subscribers) rather than growing
// unbounded - reasonable for a live monitoring feed (an SSE client that
// reconnects gets a fresh subscription anyway, per CONTRACTS' /monitor/
// stream design intent).
const subscriberBufferSize = 100

// Bus is an in-process publish/subscribe hub for monitor Events.
type Bus struct {
	mu          sync.Mutex
	subscribers map[int]chan Event
	nextID      int
	persist     Persister
}

// NewBus creates a Bus. persist may be nil (defaults to NoopPersister).
func NewBus(persist Persister) *Bus {
	if persist == nil {
		persist = NoopPersister{}
	}
	return &Bus{
		subscribers: make(map[int]chan Event),
		persist:     persist,
	}
}

// Subscribe returns a channel of future Events and an unsubscribe
// function. Events published before Subscribe is called are not
// delivered (no replay/history in this bus).
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan Event, subscriberBufferSize)
	b.subscribers[id] = ch
	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subscribers[id]; ok {
			delete(b.subscribers, id)
			close(existing)
		}
	}
	return ch, unsubscribe
}

// Publish delivers ev to every current subscriber (non-blocking - a full
// subscriber buffer just drops the event rather than stalling Publish or
// other subscribers) and persists it.
func (b *Bus) Publish(ev Event) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}

	b.mu.Lock()
	subs := make([]chan Event, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			// Subscriber's buffer is full - drop rather than block.
		}
	}

	b.persist.PersistEvent(ev)
}

// RecordChannelEvent gives *Bus the same method shape as
// channel.EventRecorder (structural typing - internal/channel does not
// import this package, per its own layering rule, but *Bus can be passed
// directly wherever a channel.EventRecorder is expected, e.g. from
// cmd/mbgw/run.go).
func (b *Bus) RecordChannelEvent(deviceID, channelID, stage, detail string) {
	b.Publish(Event{DeviceID: deviceID, ChannelID: channelID, Stage: stage, Detail: detail})
}

// RecordSchedulerEvent gives *Bus the same method shape as
// scheduler.EventRecorder (same structural-typing rationale as
// RecordChannelEvent).
func (b *Bus) RecordSchedulerEvent(stage, detail string) {
	b.Publish(Event{Stage: stage, Detail: detail})
}
