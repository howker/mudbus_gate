package poller

import (
	"context"
	"log"
	"time"

	"mbgw/internal/device"
	"mbgw/internal/scheduler"
)

// Package poller centrally dispatches scheduler.Task items to the right
// device.Device, replacing the earlier per-device fixed-interval ticker
// goroutines (device.Start) with a single shared queue - this is what
// actually makes manual polls, priority, and deferred retry (all built
// into internal/scheduler) reach real devices.
//
// Deliberately single-threaded (one task dispatched at a time, in queue
// order) for this first version: the simplest design that provably
// cannot double-poll anything, since a device's own transport/session
// are not safe for concurrent use anyway (CONTRACTS.md section 1:
// "Transport... NOT thread-safe"). Because dispatch is fully sequential,
// no additional lease-based guarding is needed here for current-value
// polls specifically - archive polls still go through device.Lease
// internally (unchanged from before), which matters there because a
// single archive read is a multi-step transaction that could, in a
// future multi-worker version of this package, race with itself; today
// it's simply redundant-but-harmless. A worker-pool version (N
// goroutines, still guarded by lease.Acquire per device+context) is a
// reasonable future extension, not built here.
//
// NOTE: channel.Manager-based failover (switching a device's active
// transport when its primary channel goes down) is NOT wired into Poller
// yet - Device.Client is still fixed at construction time, as before.
// Real failover requires Device to support a swappable Client, which is
// a device.go design change beyond this increment's scope - flagged
// here explicitly, not silently skipped.

// Poller drives a scheduler.Scheduler and dispatches its Tasks to the
// corresponding device.Device.
type Poller struct {
	scheduler *scheduler.Scheduler
	devices   map[string]*device.Device
	tickEvery time.Duration
}

// New creates a Poller. devices maps device ID -> already-constructed
// Device (transport/session/profile already wired, as cmd/mbgw/run.go
// builds them). tickEvery controls how often the scheduler's due-times
// are checked; a zero/negative value defaults to 1 second.
func New(sched *scheduler.Scheduler, devices map[string]*device.Device, tickEvery time.Duration) *Poller {
	if tickEvery <= 0 {
		tickEvery = 1 * time.Second
	}
	return &Poller{scheduler: sched, devices: devices, tickEvery: tickEvery}
}

// Run drives the scheduler (Tick, then drain every due task) until ctx is
// cancelled. Blocks the calling goroutine - callers typically invoke this
// via `go poller.Run(ctx)`.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.tickEvery)
	defer ticker.Stop()

	log.Printf("[poller] запуск (%d приборов, тик %v)\n", len(p.devices), p.tickEvery)

	for {
		select {
		case <-ctx.Done():
			log.Println("[poller] остановка")
			return
		case <-ticker.C:
			p.scheduler.Tick(time.Now())
			p.drain(ctx)
		}
	}
}

// drain dispatches every task currently queued, one at a time, until the
// queue is empty (bounded implicitly by scheduler.MaxQueueDepth).
func (p *Poller) drain(ctx context.Context) {
	for {
		task, ok := p.scheduler.Next()
		if !ok {
			return
		}
		p.dispatch(ctx, task)
	}
}

// dispatch routes one Task to its device's Poll or PollArchives.
func (p *Poller) dispatch(ctx context.Context, task scheduler.Task) {
	dev, ok := p.devices[task.DeviceID]
	if !ok {
		log.Printf("[poller] задача для неизвестного прибора %q, пропускаю\n", task.DeviceID)
		return
	}

	switch task.Kind {
	case scheduler.KindCurrent:
		dev.Poll(ctx)
	case scheduler.KindArchive:
		dev.PollArchives(ctx)
	default:
		log.Printf("[poller] неизвестный тип задачи %q для %q\n", task.Kind, task.DeviceID)
	}
}
