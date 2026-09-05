package device

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"mbgw/internal/storage"
)

type timeCorrectionTestClient struct {
	deviceTime time.Time
	writes     [][]byte
	cancel     context.CancelFunc
}

func (c *timeCorrectionTestClient) ReadRaw(context.Context, string, int, string) ([]byte, error) {
	return nil, fmt.Errorf("unexpected ReadRaw call")
}

func (c *timeCorrectionTestClient) Transact(_ context.Context, req []byte) ([]byte, error) {
	if len(req) == 5 && req[0] == 0x03 && req[1] == 0x07 && req[2] == 0x08 && req[3] == 0x00 && req[4] == 0x06 {
		return encodeVKMClockResponse(c.deviceTime), nil
	}
	if len(req) == 5 && req[0] == 0x06 {
		copyReq := append([]byte(nil), req...)
		c.writes = append(c.writes, copyReq)
		if c.cancel != nil {
			c.cancel()
		}
		return copyReq, nil
	}
	return nil, fmt.Errorf("unexpected transaction: % X", req)
}

func encodeVKMClockResponse(t time.Time) []byte {
	t = t.In(time.Local)
	resp := make([]byte, 14)
	resp[0] = 0x03
	resp[1] = 12
	values := []int{t.Day(), int(t.Month()), t.Year() % 100, t.Hour(), t.Minute(), t.Second()}
	for i, v := range values {
		binary.BigEndian.PutUint16(resp[2+i*2:4+i*2], uint16(v))
	}
	return resp
}

// timeCorrectionMemoryRepo embeds storage.Repo only to satisfy Device.Repo.
// These tests exercise only the two clock-correction accounting methods below,
// so no real SQLite file is needed and Windows cannot hold a temporary DB open.
type timeCorrectionMemoryRepo struct {
	storage.Repo
	used int
}

func (r *timeCorrectionMemoryRepo) TimeCorrectionUsedLast24Hours(context.Context, string, time.Time) (int, error) {
	return r.used, nil
}

func (r *timeCorrectionMemoryRepo) RecordTimeCorrection(_ context.Context, _ string, correctionSeconds int, _ time.Time) error {
	if correctionSeconds < 0 {
		r.used -= correctionSeconds
	} else {
		r.used += correctionSeconds
	}
	return nil
}

func correctionSecondsFromWrite(t *testing.T, req []byte) int {
	t.Helper()
	if len(req) != 5 || req[0] != 0x06 || binary.BigEndian.Uint16(req[1:3]) != 1009 {
		t.Fatalf("unexpected HR1009 write PDU: % X", req)
	}
	command := int16(binary.BigEndian.Uint16(req[3:5]))
	if int(command)%101 != 0 {
		t.Fatalf("unexpected HR1009 command value %d: not divisible by 101", command)
	}
	return int(command) / 101
}

func TestVKMTimeCorrectionDeadbandPreventsWrite(t *testing.T) {
	repo := &timeCorrectionMemoryRepo{}
	client := &timeCorrectionTestClient{
		deviceTime: time.Now().In(time.Local).Add(-20 * time.Second).Truncate(time.Second),
	}
	dev := &Device{
		ID:                              "vkm_deadband",
		Client:                          client,
		Repo:                            repo,
		TimeCorrectionDeadbandSeconds:   30,
		TimeCorrectionMaxStepSeconds:    10,
		TimeCorrectionDailyLimitSeconds: 100,
	}

	dev.updateVKMTimeDrift(context.Background())

	if len(client.writes) != 0 {
		t.Fatalf("deadband must prevent correction, got %d write(s)", len(client.writes))
	}
	if repo.used != 0 {
		t.Fatalf("deadband must not reserve daily budget, used=%d", repo.used)
	}
}

func TestVKMTimeCorrectionCapsSingleStep(t *testing.T) {
	repo := &timeCorrectionMemoryRepo{}
	ctx, cancel := context.WithCancel(context.Background())
	client := &timeCorrectionTestClient{
		deviceTime: time.Now().In(time.Local).Add(-20 * time.Second).Truncate(time.Second),
		cancel:     cancel,
	}
	dev := &Device{
		ID:                              "vkm_step",
		Client:                          client,
		Repo:                            repo,
		TimeCorrectionDeadbandSeconds:   1,
		TimeCorrectionMaxStepSeconds:    7,
		TimeCorrectionDailyLimitSeconds: 100,
	}

	dev.updateVKMTimeDrift(ctx)

	if len(client.writes) != 1 {
		t.Fatalf("want exactly 1 correction write, got %d", len(client.writes))
	}
	if got := correctionSecondsFromWrite(t, client.writes[0]); got != 7 {
		t.Fatalf("want correction capped to +7 sec, got %+d", got)
	}
	if repo.used != 7 {
		t.Fatalf("want 7 sec reserved in daily budget, got %d", repo.used)
	}
}

func TestVKMTimeCorrectionCapsByDailyRemaining(t *testing.T) {
	repo := &timeCorrectionMemoryRepo{used: 8}
	ctx, cancel := context.WithCancel(context.Background())
	client := &timeCorrectionTestClient{
		deviceTime: time.Now().In(time.Local).Add(-20 * time.Second).Truncate(time.Second),
		cancel:     cancel,
	}
	dev := &Device{
		ID:                              "vkm_daily",
		Client:                          client,
		Repo:                            repo,
		TimeCorrectionDeadbandSeconds:   1,
		TimeCorrectionMaxStepSeconds:    10,
		TimeCorrectionDailyLimitSeconds: 10,
	}

	dev.updateVKMTimeDrift(ctx)

	if len(client.writes) != 1 {
		t.Fatalf("want exactly 1 correction write, got %d", len(client.writes))
	}
	if got := correctionSecondsFromWrite(t, client.writes[0]); got != 2 {
		t.Fatalf("want correction capped by remaining daily budget to +2 sec, got %+d", got)
	}
	if repo.used != 10 {
		t.Fatalf("want daily budget exactly exhausted at 10 sec, got %d", repo.used)
	}
}
