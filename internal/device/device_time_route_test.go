package device

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mbgw/internal/codec"
	"mbgw/internal/devicestatus"
	"mbgw/internal/profile"
)

type clockRouteClient struct {
	readRawResp   []byte
	readRawErr    error
	readRawCalls  int
	transactCalls int
}

func (c *clockRouteClient) ReadRaw(ctx context.Context, space string, addr int, dataType string) ([]byte, error) {
	c.readRawCalls++
	if c.readRawErr != nil {
		return nil, c.readRawErr
	}
	return append([]byte(nil), c.readRawResp...), nil
}

func (c *clockRouteClient) Transact(ctx context.Context, req []byte) ([]byte, error) {
	c.transactCalls++
	return nil, fmt.Errorf("unexpected Transact call: % X", req)
}

func TestUpdateTimeDrift_VZLETUsesUnixClockPoint(t *testing.T) {
	addr := 32768
	deviceClock := time.Now().In(time.Local).Add(-90 * time.Second).Truncate(time.Second)
	wallClockUnix := time.Date(
		deviceClock.Year(), deviceClock.Month(), deviceClock.Day(),
		deviceClock.Hour(), deviceClock.Minute(), deviceClock.Second(),
		0, time.UTC,
	).Unix()
	raw, err := codec.EncodeUint32(uint32(wallClockUnix), "0123")
	if err != nil {
		t.Fatalf("encode clock: %v", err)
	}

	cli := &clockRouteClient{readRawResp: raw}
	p := &profile.Profile{
		Meta:  profile.Meta{Vendor: "VZLET", Model: "IVK-TER"},
		Codec: profile.Codec{WordOrder32: "0123"},
		Points: []profile.Point{
			{
				Name:   "current_time",
				Space:  "IR",
				Addr:   &addr,
				Type:   "uint32",
				Epoch:  "1970-01-01",
				Access: "read",
			},
		},
	}

	id := fmt.Sprintf("test-vzlet-time-%d", time.Now().UnixNano())
	d := &Device{ID: id, Profile: p, Client: cli}
	d.updateTimeDrift(context.Background())

	got, ok := devicestatus.Get(id)
	if !ok {
		t.Fatal("no time drift status published")
	}
	if !got.Reliable {
		t.Fatalf("time drift marked unreliable: %s", got.Note)
	}
	if got.DriftSeconds > -87 || got.DriftSeconds < -93 {
		t.Fatalf("drift=%+.3f sec, want about -90 sec", got.DriftSeconds)
	}
	if cli.readRawCalls != 1 {
		t.Fatalf("ReadRaw calls=%d, want 1", cli.readRawCalls)
	}
	if cli.transactCalls != 0 {
		t.Fatalf("Transact calls=%d, want 0 (VZLET must not use VKM HR1800 clock read)", cli.transactCalls)
	}
}

func TestUpdateTimeDrift_UnknownProfileDoesNotFallBackToVKM(t *testing.T) {
	cli := &clockRouteClient{}
	p := &profile.Profile{Meta: profile.Meta{Model: "UNKNOWN-MODBUS"}}
	id := fmt.Sprintf("test-unknown-time-%d", time.Now().UnixNano())
	d := &Device{ID: id, Profile: p, Client: cli}

	d.updateTimeDrift(context.Background())

	got, ok := devicestatus.Get(id)
	if !ok {
		t.Fatal("no time drift status published")
	}
	if got.Reliable {
		t.Fatalf("unknown profile unexpectedly marked reliable: %+v", got)
	}
	if cli.readRawCalls != 0 || cli.transactCalls != 0 {
		t.Fatalf("unknown profile performed I/O: ReadRaw=%d Transact=%d", cli.readRawCalls, cli.transactCalls)
	}
}
