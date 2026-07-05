package redundancy

import (
    "context"
    "testing"
    "time"

    "mbgw/internal/lease"
)

func TestNext_AllContractTransitions(t *testing.T) {
    cases := []struct {
        from  State
        event string
        want  State
    }{
        {StateNormalActive, "standby_heartbeat_lost", StateDegraded},
        {StateNormalStandby, "active_heartbeat_lost_past_grace", StateFailover},
        {StateFailover, "all_device_leases_acquired", StateNormalActive},
        {StateFailover, "old_active_reappeared", StateRecovery},
        {StateRecovery, "manual_switchover", StateNormalActive},
    }
    for _, c := range cases {
        got, ok := Next(c.from, c.event)
        if !ok {
            t.Fatalf("expected (%s, %s) to be a legal transition", c.from, c.event)
        }
        if got != c.want {
            t.Fatalf("(%s, %s): want %s, got %s", c.from, c.event, c.want, got)
        }
    }
}

func TestNext_UnknownEventRejected(t *testing.T) {
    if _, ok := Next(StateNormalActive, "nonsense_event"); ok {
        t.Fatal("expected an unknown event to be rejected, not silently accepted")
    }
}

func TestNext_NoAutoFailback(t *testing.T) {
    // Recovery must not have any transition that fires without the
    // explicit "manual_switchover" event - CONTRACTS.md section 8 is
    // explicit that there is no auto-failback.
    for _, ev := range []string{"old_active_reappeared", "standby_heartbeat_lost", "active_heartbeat_lost_past_grace", "all_device_leases_acquired"} {
        if _, ok := Next(StateRecovery, ev); ok {
            t.Fatalf("Recovery must not auto-transition on %q (no auto-failback)", ev)
        }
    }
}

func TestNoopHeartbeat_NeverReportsLost(t *testing.T) {
    var hb Heartbeat = NoopHeartbeat{}
    if err := hb.Send(context.Background()); err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if hb.Lost(0) {
        t.Fatal("NoopHeartbeat must never report the peer as lost (no real peer exists yet)")
    }
}

func TestFencer_AcquireAll_Succeeds(t *testing.T) {
    l := lease.New()
    f := NewFencer(l)

    release, err := f.AcquireAll(context.Background(), []string{"dev1", "dev2", "dev3"}, 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    defer release()

    // Every device's lease must now be held - a second acquire on any of
    // them must conflict.
    for _, id := range []string{"dev1", "dev2", "dev3"} {
        if _, err := l.Acquire(context.Background(), id, "redundancy_failover", time.Second); err == nil {
            t.Fatalf("expected device %q lease to already be held", id)
        }
    }
}

func TestFencer_AcquireAll_PartialFailureReleasesAll(t *testing.T) {
    l := lease.New()

    // Pre-acquire dev2's lease under the same context Fencer uses, so
    // Fencer's own AcquireAll for dev2 will conflict.
    blocking, err := l.Acquire(context.Background(), "dev2", "redundancy_failover", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error pre-acquiring dev2: %v", err)
    }
    defer blocking()

    f := NewFencer(l)
    _, err = f.AcquireAll(context.Background(), []string{"dev1", "dev2", "dev3"}, 5*time.Second)
    if err == nil {
        t.Fatal("expected AcquireAll to fail when dev2 is already held")
    }

    // dev1 must have been released again (it was acquired before dev2
    // failed) - a node must not end up holding only some of the devices.
    release1, err := l.Acquire(context.Background(), "dev1", "redundancy_failover", time.Second)
    if err != nil {
        t.Fatalf("expected dev1's lease to have been released after the partial failure, got: %v", err)
    }
    release1()
}