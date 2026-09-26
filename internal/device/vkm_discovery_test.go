package device

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/lease"
)

func discoveryStatusMap(results []VKMPipeDiscoveryResult) map[int]string {
	out := make(map[int]string, len(results))
	for _, r := range results {
		out[r.PipeNo] = r.Status
	}
	return out
}

func TestDiscoverVKMPipesSinglePipe(t *testing.T) {
	now := time.Date(2026, 9, 25, 21, 20, 0, 0, time.Local)
	results, err := discoverVKMPipesAt(context.Background(), now, func(_ context.Context, pipe int, _ time.Time) (string, error) {
		if pipe == 1 {
			return "pipe1", nil
		}
		return "", archive.ErrVKMInvalidPipe
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(results) != 10 {
		t.Fatalf("results=%d, want 10", len(results))
	}
	status := discoveryStatusMap(results)
	if status[1] != VKMPipeDiscoveryAvailable {
		t.Fatalf("pipe1=%q, want available", status[1])
	}
	for pipe := 2; pipe <= 10; pipe++ {
		if status[pipe] != VKMPipeDiscoveryAbsent {
			t.Fatalf("pipe%d=%q, want absent", pipe, status[pipe])
		}
	}
}

func TestDiscoverVKMPipesTwoPipesRetriesNoRecords(t *testing.T) {
	now := time.Date(2026, 9, 25, 21, 20, 0, 0, time.Local)
	latest := now.Truncate(vkmArchivePeriod).Add(-vkmArchivePeriod)
	calls := map[int]int{}
	results, err := discoverVKMPipesAt(context.Background(), now, func(_ context.Context, pipe int, periodStart time.Time) (string, error) {
		calls[pipe]++
		switch pipe {
		case 1:
			return "pipe1", nil
		case 2:
			if periodStart.Equal(latest) {
				return "", archive.ErrVKMNoRecords
			}
			return "pipe2", nil
		default:
			return "", archive.ErrVKMInvalidPipe
		}
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	status := discoveryStatusMap(results)
	if status[1] != VKMPipeDiscoveryAvailable || status[2] != VKMPipeDiscoveryAvailable {
		t.Fatalf("statuses=%v, want pipes 1 and 2 available", status)
	}
	if calls[2] != 2 {
		t.Fatalf("pipe2 calls=%d, want 2 after no-records retry", calls[2])
	}
}

func TestDiscoverVKMPipesNoRecordsRemainUncertain(t *testing.T) {
	now := time.Date(2026, 9, 25, 21, 20, 0, 0, time.Local)
	results, err := discoverVKMPipesAt(context.Background(), now, func(_ context.Context, pipe int, _ time.Time) (string, error) {
		if pipe == 1 {
			return "", archive.ErrVKMNoRecords
		}
		return "", archive.ErrVKMInvalidPipe
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got := discoveryStatusMap(results)[1]; got != VKMPipeDiscoveryUncertain {
		t.Fatalf("pipe1=%q, want uncertain (no records must not mean absent)", got)
	}
}

func TestDiscoverVKMPipesCapacityTen(t *testing.T) {
	now := time.Date(2026, 9, 25, 21, 20, 0, 0, time.Local)
	results, err := discoverVKMPipesAt(context.Background(), now, func(_ context.Context, _ int, _ time.Time) (string, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(results) != 10 {
		t.Fatalf("results=%d, want 10", len(results))
	}
	for _, result := range results {
		if result.Status != VKMPipeDiscoveryAvailable {
			t.Fatalf("pipe%d=%q, want available", result.PipeNo, result.Status)
		}
	}
}

func TestDiscoveryReleasesLeaseBetweenPipesSoScheduledPollCanInterleave(t *testing.T) {
	ctx := context.Background()
	l := lease.New()
	const deviceID = "vkm-lease"
	scheduledAcquired := false

	_, err := discoverVKMPipesWithLeaseAt(ctx, time.Date(2026, 9, 26, 10, 5, 0, 0, time.Local),
		func(ctx context.Context, pipe int) (func(), error) {
			release, err := l.Acquire(ctx, deviceID, fmt.Sprintf("discovery-%d", pipe), time.Minute)
			if err != nil {
				return nil, err
			}
			return func() {
				release()
				if pipe == 1 {
					scheduledRelease, schedErr := l.Acquire(ctx, deviceID, "scheduled-archive-xx05", time.Minute)
					if schedErr != nil {
						t.Fatalf("scheduled poll could not interleave between pipes: %v", schedErr)
					}
					scheduledAcquired = true
					scheduledRelease()
				}
			}, nil
		},
		func(_ context.Context, pipe int, _ time.Time) (string, error) {
			if pipe <= 2 {
				return fmt.Sprintf("pipe%d", pipe), nil
			}
			return "", archive.ErrVKMInvalidPipe
		})
	if err != nil {
		t.Fatal(err)
	}
	if !scheduledAcquired {
		t.Fatal("scheduled poll never acquired lease between discovery pipes")
	}
}
