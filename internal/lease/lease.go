package lease

import (
    "context"
    "fmt"
    "sync"
    "time"

    "mbgw/internal/errs"
)

// Lease provides the single MVP primitive that solves both anti-double-poll
// (only one owner per device at a time) and serialization of stateful
// sessions within a context (e.g. VKM archive requests, which must not run
// concurrently on the same interface), per CONTRACTS.md section 10.
//
// This is the local (in-memory) MVP implementation; a distributed
// implementation (backed by the device_leases table, for redundancy) is a
// post-MVP follow-up.
type Lease interface {
    // Acquire blocks until the lease for (deviceID, context) is free or ctx
    // is cancelled, then holds it for at most ttl. release must be called
    // to give it up early; if not called, the lease expires after ttl.
    Acquire(ctx context.Context, deviceID, leaseContext string, ttl time.Duration) (release func(), err error)
}

type entry struct {
    mu      sync.Mutex
    expires time.Time
}

// LocalLease is the local, in-process Lease implementation for MVP.
type LocalLease struct {
    mu      sync.Mutex
    entries map[string]*entry
}

// New creates a new local lease manager.
func New() *LocalLease {
    return &LocalLease{entries: make(map[string]*entry)}
}

func key(deviceID, leaseContext string) string {
    return deviceID + "|" + leaseContext
}

// Acquire attempts to acquire the lease for (deviceID, leaseContext). If
// already held (and not expired), it returns errs.ErrLease immediately -
// callers (e.g. archive strategies hitting a BUSY device) are expected to
// retry with their own backoff rather than block here, matching
// CONTRACTS.md section 6.1's own retry/backoff loop for VKM archive BUSY
// handling.
func (l *LocalLease) Acquire(ctx context.Context, deviceID, leaseContext string, ttl time.Duration) (func(), error) {
    if deviceID == "" {
        return nil, fmt.Errorf("lease: deviceID must not be empty")
    }

    l.mu.Lock()
    k := key(deviceID, leaseContext)
    e, ok := l.entries[k]
    now := time.Now()

    if ok && e.expires.After(now) {
        l.mu.Unlock()
        return nil, fmt.Errorf("lease held for device %q context %q: %w", deviceID, leaseContext, errs.ErrLease)
    }

    if !ok {
        e = &entry{}
        l.entries[k] = e
    }
    e.expires = now.Add(ttl)
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
        if cur, ok := l.entries[k]; ok && cur == e {
            delete(l.entries, k)
        }
    }

    return release, nil
}