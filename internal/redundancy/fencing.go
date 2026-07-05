package redundancy

import (
    "context"
    "fmt"
    "time"

    "mbgw/internal/lease"
)

// Fencer implements the split-brain protection required by
// CONTRACTS.md section 8: "захват устройства возможен только через
// device_leases (единственный владелец). зел без действующего lease не
// опрашивает." It wraps the existing lease.Lease primitive (already the
// single source of truth for device ownership, per CONTRACTS.md section
// 10) rather than introducing a second, parallel ownership mechanism.
type Fencer struct {
    leases lease.Lease
}

// NewFencer builds a Fencer over an existing lease.Lease manager. The
// same lease.Lease instance used for anti-double-poll (see internal/lease)
// is reused here - redundancy does not get its own notion of ownership.
func NewFencer(l lease.Lease) *Fencer {
    return &Fencer{leases: l}
}

// AcquireAll attempts to acquire the lease for every device in deviceIDs,
// implementing the Failover->NormalActive transition's precondition
// ("захват lease всех устройств"). If any device's lease cannot be
// acquired, all leases acquired so far in this call are released and an
// error is returned - a node must not become active while holding only
// some of the devices, since that would violate the single-owner
// invariant for the rest.
func (f *Fencer) AcquireAll(ctx context.Context, deviceIDs []string, ttl time.Duration) (release func(), err error) {
    releases := make([]func(), 0, len(deviceIDs))

    releaseAll := func() {
        for i := len(releases) - 1; i >= 0; i-- {
            releases[i]()
        }
    }

    for _, id := range deviceIDs {
        r, err := f.leases.Acquire(ctx, id, "redundancy_failover", ttl)
        if err != nil {
            releaseAll()
            return nil, fmt.Errorf("redundancy: failed to acquire lease for device %q during failover: %w", id, err)
        }
        releases = append(releases, r)
    }

    return releaseAll, nil
}