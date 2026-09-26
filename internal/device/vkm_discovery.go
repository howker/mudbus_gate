package device

import (
	"context"
	"errors"
	"fmt"
	"time"

	"mbgw/internal/archive"
)

const (
	VKMPipeDiscoveryAvailable = "available"
	VKMPipeDiscoveryAbsent    = "absent"
	VKMPipeDiscoveryUncertain = "uncertain"
	VKMPipeDiscoveryError     = "error"
)

// VKMPipeDiscoveryResult is one physical archive-probe result. Discovery is
// deliberately separate from vkm_active_pipes: it reports what the meter
// answered, while the operator keeps control of which confirmed pipes the
// normal poller should actually read. Raw is retained only long enough for the
// web layer to extract and persist discovered source tags; discovery itself
// does not write archive_hourly/archive_vkm_raw and therefore cannot trigger ES
// healing as a side effect.
type VKMPipeDiscoveryResult struct {
	PipeNo int
	Status string
	Detail string
	Raw    string
}

type vkmDiscoveryReadFunc func(ctx context.Context, pipe int, periodStart time.Time) (raw string, err error)

// discoverVKMPipesAt contains the policy that distinguishes a genuinely
// unsupported pipe from a valid pipe whose most recent archive period is
// simply empty. The newest fully completed half-hour is tried first. Only the
// explicit protocol status "invalid pipe number" proves absence. "No records"
// causes one retry against the adjacent previous completed period; if both are
// empty the result stays uncertain rather than disabling anything.
func discoverOneVKMPipeAt(ctx context.Context, now time.Time, pipe int, read vkmDiscoveryReadFunc) (VKMPipeDiscoveryResult, error) {
	latest := now.Truncate(vkmArchivePeriod).Add(-vkmArchivePeriod)
	previous := latest.Add(-vkmArchivePeriod)
	if err := ctx.Err(); err != nil {
		return VKMPipeDiscoveryResult{}, err
	}

	raw, err := read(ctx, pipe, latest)
	switch {
	case err == nil:
		return VKMPipeDiscoveryResult{PipeNo: pipe, Status: VKMPipeDiscoveryAvailable,
			Detail: fmt.Sprintf("архив ответил за период до %s", latest.Add(vkmArchivePeriod).Format("02.01.2006 15:04")), Raw: raw}, nil
	case errors.Is(err, archive.ErrVKMInvalidPipe):
		return VKMPipeDiscoveryResult{PipeNo: pipe, Status: VKMPipeDiscoveryAbsent, Detail: err.Error()}, nil
	case errors.Is(err, archive.ErrVKMNoRecords):
		// Retry adjacent completed period while still holding this pipe's lease.
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return VKMPipeDiscoveryResult{}, err
	default:
		return VKMPipeDiscoveryResult{PipeNo: pipe, Status: VKMPipeDiscoveryError, Detail: err.Error()}, nil
	}

	raw, err = read(ctx, pipe, previous)
	switch {
	case err == nil:
		return VKMPipeDiscoveryResult{PipeNo: pipe, Status: VKMPipeDiscoveryAvailable,
			Detail: fmt.Sprintf("архив ответил за предыдущий период до %s", previous.Add(vkmArchivePeriod).Format("02.01.2006 15:04")), Raw: raw}, nil
	case errors.Is(err, archive.ErrVKMInvalidPipe):
		return VKMPipeDiscoveryResult{PipeNo: pipe, Status: VKMPipeDiscoveryAbsent, Detail: err.Error()}, nil
	case errors.Is(err, archive.ErrVKMNoRecords):
		return VKMPipeDiscoveryResult{PipeNo: pipe, Status: VKMPipeDiscoveryUncertain,
			Detail: "нет записей в двух соседних завершённых периодах"}, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return VKMPipeDiscoveryResult{}, err
	default:
		return VKMPipeDiscoveryResult{PipeNo: pipe, Status: VKMPipeDiscoveryError, Detail: err.Error()}, nil
	}
}

func discoverVKMPipesAt(ctx context.Context, now time.Time, read vkmDiscoveryReadFunc) ([]VKMPipeDiscoveryResult, error) {
	results := make([]VKMPipeDiscoveryResult, 0, vkmMaxPipe-vkmMinPipe+1)
	for pipe := vkmMinPipe; pipe <= vkmMaxPipe; pipe++ {
		res, err := discoverOneVKMPipeAt(ctx, now, pipe, read)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}

type vkmDiscoveryAcquireFunc func(ctx context.Context, pipe int) (func(), error)

func discoverVKMPipesWithLeaseAt(ctx context.Context, now time.Time, acquire vkmDiscoveryAcquireFunc, read vkmDiscoveryReadFunc) ([]VKMPipeDiscoveryResult, error) {
	results := make([]VKMPipeDiscoveryResult, 0, vkmMaxPipe-vkmMinPipe+1)
	for pipe := vkmMinPipe; pipe <= vkmMaxPipe; pipe++ {
		release, err := acquire(ctx, pipe)
		if err != nil {
			return nil, err
		}
		res, probeErr := discoverOneVKMPipeAt(ctx, now, pipe, read)
		release() // mandatory yield point before the next pipe
		if probeErr != nil {
			return nil, probeErr
		}
		results = append(results, res)
	}
	return results, nil
}

// DiscoverVKMPipes physically checks VKM archive instances 1..10 without
// writing the returned period into the normal archive store. The raw response
// is returned to the caller solely so the discovered source-tag list can be
// persisted as discovery metadata. Normal archive/ES flows remain unchanged.
func (d *Device) DiscoverVKMPipes(ctx context.Context) ([]VKMPipeDiscoveryResult, error) {
	if d.Profile == nil {
		return nil, fmt.Errorf("профиль прибора не загружен")
	}
	archiveIndex := -1
	for i := range d.Profile.Archives {
		if d.Profile.Archives[i].Strategy == "mb_request_poll_string" {
			archiveIndex = i
			break
		}
	}
	if archiveIndex < 0 {
		return nil, fmt.Errorf("в профиле нет архива VKM mb_request_poll_string")
	}
	a := d.Profile.Archives[archiveIndex]

	return discoverVKMPipesWithLeaseAt(ctx, time.Now(),
		func(acquireCtx context.Context, pipe int) (func(), error) {
			release, err := d.acquireLeaseWithRetry(acquireCtx, fmt.Sprintf("vkm_pipe_discovery_%d", pipe), 45*time.Second)
			if err != nil {
				return nil, fmt.Errorf("сканирование трубопровода %d: %w", pipe, err)
			}
			return release, nil
		},
		func(readCtx context.Context, pipe int, periodStart time.Time) (string, error) {
			rec, found, err := d.readVKMPeriodUnlocked(readCtx, a, pipe, periodStart)
			if err != nil {
				return "", err
			}
			if !found {
				return "", archive.ErrVKMNoRecords
			}
			return string(rec.Raw), nil
		})
}
