package lease

import (
	"context"
	"fmt"
	"sync"
	"time"

	"mbgw/internal/errs"
)

// Lease serializes logical operations for one device.
//
// The lease key is the deviceID only. leaseContext is diagnostic metadata
// describing the operation that is asking for the lease (for example
// "current", "hourly", "manual_reload", "redundancy_failover"); it is NOT
// part of the lock key.
//
// This is intentionally separate from the physical I/O lock in pollcore:
//   - lease: one logical operation per device;
//   - pollcore I/O lock: one wire transaction at a time per physical channel.
//
// Acquire is non-blocking: if the device is already leased, it returns
// errs.ErrLease immediately. Callers that want to wait/retry do that with
// their own backoff.
type Lease interface {
	Acquire(ctx context.Context, deviceID, leaseContext string, ttl time.Duration) (release func(), err error)
}

type entry struct {
	context string
}

// LocalLease is the local, in-process lease implementation.
//
// ttl is accepted to preserve the Lease interface and future distributed
// implementations, but LocalLease deliberately does NOT auto-expire an
// active lease. An in-process lease disappears automatically when the
// process exits, while expiring it by wall-clock time could allow a second
// operation to enter while a legitimate long-running operation is still
// using the same device.
type LocalLease struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// New creates a new local lease manager.
func New() *LocalLease {
	return &LocalLease{entries: make(map[string]*entry)}
}

// Acquire attempts to acquire the lease for the whole device.
//
// leaseContext is used only in diagnostics. Different contexts on the same
// device conflict with each other.
func (l *LocalLease) Acquire(ctx context.Context, deviceID, leaseContext string, ttl time.Duration) (func(), error) {
	if deviceID == "" {
		return nil, fmt.Errorf("lease: deviceID must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// LocalLease does not use TTL; see type comment above.
	_ = ttl

	l.mu.Lock()
	if held, ok := l.entries[deviceID]; ok {
		l.mu.Unlock()
		return nil, fmt.Errorf(
			"lease held for device %q by context %q (requested context %q): %w",
			deviceID, held.context, leaseContext, errs.ErrLease,
		)
	}

	e := &entry{context: leaseContext}
	l.entries[deviceID] = e
	l.mu.Unlock()

	released := false
	var releaseMu sync.Mutex
	release := func() {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		if released {
			return
		}
		released = true

		l.mu.Lock()
		defer l.mu.Unlock()
		if cur, ok := l.entries[deviceID]; ok && cur == e {
			delete(l.entries, deviceID)
		}
	}

	return release, nil
}
