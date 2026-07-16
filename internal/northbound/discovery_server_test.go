package northbound

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func startDiscoveryServer(t *testing.T, fixture DiscoveryFixture, log *DiscoveryLog) (addr string, cancel func()) {
	t.Helper()
	srv := NewDiscoveryServer("127.0.0.1:0", fixture, log)
	srv.ReadTimeout = 300 * time.Millisecond

	ctx, cancelCtx := context.WithCancel(context.Background())
	ready := make(chan struct{})
	errCh := make(chan error, 1)

	go func() {
		go func() {
			for i := 0; i < 100; i++ {
				if srv.Addr() != nil {
					close(ready)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		errCh <- srv.Listen(ctx)
	}()

	select {
	case <-ready:
	case err := <-errCh:
		t.Fatalf("discovery server exited before binding: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("discovery server did not bind in time")
	}

	return srv.Addr().String(), func() {
		cancelCtx()
		_ = srv.Close()
	}
}

// A discovery server must answer requests for ANY unit id — unlike M1's
// Server, it does not know (or care) which unit id it "is".
func TestDiscoveryServer_AnyUnitID_GetsAResponse(t *testing.T) {
	addr, cancel := startDiscoveryServer(t, DiscoveryFixture{}, nil)
	defer cancel()

	for _, unit := range []byte{1, 9, 247} {
		conn := dial(t, addr)
		req := buildReadRequest(0x0001, unit, funcReadHolding, 0, 2)
		conn.Write(req)
		resp := readResponse(t, conn)
		conn.Close()

		if resp[6] != unit {
			t.Fatalf("unit %d: response unit mismatch, got %d", unit, resp[6])
		}
		if resp[7] != funcReadHolding {
			t.Fatalf("unit %d: expected a func 03 stub response, got func=0x%02X", unit, resp[7])
		}
	}
}

func TestDiscoveryServer_LogsRequestAndResponse(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "discovery.jsonl")
	log, err := NewDiscoveryLog(logPath)
	if err != nil {
		t.Fatal(err)
	}

	addr, cancel := startDiscoveryServer(t, DiscoveryFixture{ReadFillByte: 0x7A}, log)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()

	req := buildReadRequest(0x0002, 5, funcReadHolding, 100, 3)
	conn.Write(req)
	_ = readResponse(t, conn)

	log.Close()

	f, _ := os.Open(logPath)
	defer f.Close()

	var entries []DiscoveryEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e DiscoveryEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad JSONL line: %v", err)
		}
		entries = append(entries, e)
	}

	if len(entries) != 2 {
		t.Fatalf("expected request+response entries, got %d", len(entries))
	}

	reqEntry, respEntry := entries[0], entries[1]
	if reqEntry.Direction != "request" || reqEntry.Unit != 5 || reqEntry.Function != funcReadHolding {
		t.Fatalf("unexpected request entry: %+v", reqEntry)
	}
	if reqEntry.Address != 100 || reqEntry.Quantity != 3 {
		t.Fatalf("unexpected request address/quantity: %+v", reqEntry)
	}
	if respEntry.Direction != "response" || respEntry.PayloadHex == "" {
		t.Fatalf("unexpected response entry: %+v", respEntry)
	}
}

func TestDiscoveryServer_WriteRequest_GetsAckNotException(t *testing.T) {
	// Discovery mode's whole purpose is to keep a dialogue going, so
	// (unlike M1's Server, which always rejects writes) a write here
	// gets a plausible ack, not exception 1.
	addr, cancel := startDiscoveryServer(t, DiscoveryFixture{}, nil)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()

	req := buildWriteSingleRegisterRequest(0x0003, 1, 10, 999)
	conn.Write(req)
	resp := readResponse(t, conn)

	if resp[7] != funcWriteSingleRegister {
		t.Fatalf("expected an ack (func 0x06), got func=0x%02X", resp[7])
	}
}
