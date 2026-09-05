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

func TestAcquire_DoesNotExpireWhileHeld(t *testing.T) {
	l := New()
	release, err := l.Acquire(context.Background(), "dev1", "long_operation", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()

	time.Sleep(40 * time.Millisecond)

	_, err = l.Acquire(context.Background(), "dev1", "second_operation", 5*time.Second)
	if err == nil {
		t.Fatal("expected device lease to remain held until release, even after ttl elapsed")
	}
	if !errors.Is(err, errs.ErrLease) {
		t.Fatalf("expected errs.ErrLease, got %v", err)
	}
}

func TestAcquire_DifferentDevicesDoNotConflict(t *testing.T) {
	l := New()
	release1, err := l.Acquire(context.Background(), "dev1", "current", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release1()

	release2, err := l.Acquire(context.Background(), "dev2", "archive", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error acquiring lease for a different device: %v", err)
	}
	defer release2()
}

func TestAcquire_DifferentContextsOnSameDeviceConflict(t *testing.T) {
	l := New()
	releaseA, err := l.Acquire(context.Background(), "dev1", "current", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer releaseA()

	_, err = l.Acquire(context.Background(), "dev1", "archive", 5*time.Second)
	if err == nil {
		t.Fatal("expected different logical operations on the same device to conflict")
	}
	if !errors.Is(err, errs.ErrLease) {
		t.Fatalf("expected errs.ErrLease, got %v", err)
	}
}

func TestAcquire_ReleaseIsIdempotent(t *testing.T) {
	l := New()
	release, err := l.Acquire(context.Background(), "dev1", "", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	release()
	release()

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

func TestAcquire_CancelledContextRejected(t *testing.T) {
	l := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := l.Acquire(ctx, "dev1", "current", 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
