package lease

import (
    "context"
    "errors"
    "testing"
    "time"

    "mbgw/internal/errs"
)

func TestAcquire_Succeeds(t *testing.T) {
    l := New()
    release, err := l.Acquire(context.Background(), "dev1", "", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    defer release()
}

func TestAcquire_ConflictsWhileHeld(t *testing.T) {
    l := New()
    release, err := l.Acquire(context.Background(), "dev1", "", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    defer release()

    _, err = l.Acquire(context.Background(), "dev1", "", 5*time.Second)
    if err == nil {
        t.Fatal("expected error acquiring already-held lease, got nil")
    }
    if !errors.Is(err, errs.ErrLease) {
        t.Fatalf("expected errs.ErrLease, got %v", err)
    }
}

func TestAcquire_SucceedsAfterRelease(t *testing.T) {
    l := New()
    release, err := l.Acquire(context.Background(), "dev1", "", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    release()

    release2, err := l.Acquire(context.Background(), "dev1", "", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error re-acquiring after release: %v", err)
    }
    defer release2()
}

func TestAcquire_SucceedsAfterExpiry(t *testing.T) {
    l := New()
    _, err := l.Acquire(context.Background(), "dev1", "", 20*time.Millisecond)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    // Do not release; wait for natural expiry.
    time.Sleep(30 * time.Millisecond)

    release2, err := l.Acquire(context.Background(), "dev1", "", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error acquiring after expiry: %v", err)
    }
    defer release2()
}

func TestAcquire_DifferentDevicesDoNotConflict(t *testing.T) {
    l := New()
    release1, err := l.Acquire(context.Background(), "dev1", "", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    defer release1()

    release2, err := l.Acquire(context.Background(), "dev2", "", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error acquiring lease for a different device: %v", err)
    }
    defer release2()
}

func TestAcquire_DifferentContextsOnSameDeviceConflictIndependently(t *testing.T) {
    l := New()
    releaseA, err := l.Acquire(context.Background(), "dev1", "ctxA", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    defer releaseA()

    releaseB, err := l.Acquire(context.Background(), "dev1", "ctxB", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error acquiring a different context on same device: %v", err)
    }
    defer releaseB()

    _, err = l.Acquire(context.Background(), "dev1", "ctxA", 5*time.Second)
    if err == nil {
        t.Fatal("expected conflict re-acquiring the same (device, context) pair")
    }
}

func TestAcquire_ReleaseIsIdempotent(t *testing.T) {
    l := New()
    release, err := l.Acquire(context.Background(), "dev1", "", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    release()
    release() // second call must not panic or corrupt state

    release2, err := l.Acquire(context.Background(), "dev1", "", 5*time.Second)
    if err != nil {
        t.Fatalf("unexpected error after idempotent double release: %v", err)
    }
    defer release2()
}

func TestAcquire_EmptyDeviceIDRejected(t *testing.T) {
    l := New()
    if _, err := l.Acquire(context.Background(), "", "", 5*time.Second); err == nil {
        t.Fatal("expected error for empty deviceID, got nil")
    }
}