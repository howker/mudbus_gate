package channel

import (
	"context"
	"fmt"
	"net"
	"testing"

	"mbgw/internal/transport"
)

// fakeEventRecorder captures RecordChannelEvent calls for assertions.
type fakeEventRecorder struct {
	events []string
}

func (f *fakeEventRecorder) RecordChannelEvent(deviceID, channelID, stage, detail string) {
	f.events = append(f.events, fmt.Sprintf("%s|%s|%s|%s", deviceID, channelID, stage, detail))
}

func testDeviceChannels(primaryID, backupID string) DeviceChannels {
	dc := DeviceChannels{
		Primary: ChannelSpec{ID: primaryID, Params: transport.Params{Kind: transport.KindModbusTCP, Host: "127.0.0.1", Port: 15099}},
	}
	if backupID != "" {
		dc.Backup = &ChannelSpec{ID: backupID, Params: transport.Params{Kind: transport.KindModbusTCP, Host: "127.0.0.1", Port: 15098}}
	}
	return dc
}

func TestManager_ActiveChannelDefaultsToPrimary(t *testing.T) {
	m := NewManager(nil)
	m.Register("dev1", testDeviceChannels("primary", "backup"))

	active, ok := m.ActiveChannel("dev1")
	if !ok {
		t.Fatal("expected dev1 to be registered")
	}
	if active != "primary" {
		t.Fatalf("expected primary to be active initially, got %q", active)
	}
}

func TestManager_BelowDegradedThreshold_StaysOK(t *testing.T) {
	m := NewManager(nil)
	m.Register("dev1", testDeviceChannels("primary", "backup"))

	m.ReportResult("dev1", "primary", false)
	m.ReportResult("dev1", "primary", false)

	state, _ := m.State("primary")
	if state != StateOK {
		t.Fatalf("expected state ok after 2 failures (threshold 3), got %s", state)
	}
}

func TestManager_DegradedThreshold(t *testing.T) {
	events := &fakeEventRecorder{}
	m := NewManager(events)
	m.Register("dev1", testDeviceChannels("primary", "backup"))

	for i := 0; i < DefaultDegradedThreshold; i++ {
		m.ReportResult("dev1", "primary", false)
	}

	state, _ := m.State("primary")
	if state != StateDegraded {
		t.Fatalf("expected state degraded after %d failures, got %s", DefaultDegradedThreshold, state)
	}
	if len(events.events) != 1 {
		t.Fatalf("expected 1 degraded event, got %d: %v", len(events.events), events.events)
	}
}

func TestManager_DownThreshold_TriggersFailover(t *testing.T) {
	events := &fakeEventRecorder{}
	m := NewManager(events)
	m.Register("dev1", testDeviceChannels("primary", "backup"))

	for i := 0; i < DefaultDownThreshold; i++ {
		m.ReportResult("dev1", "primary", false)
	}

	state, _ := m.State("primary")
	if state != StateDown {
		t.Fatalf("expected primary state down, got %s", state)
	}

	active, _ := m.ActiveChannel("dev1")
	if active != "backup" {
		t.Fatalf("expected failover to backup, active channel is %q", active)
	}

	foundFailover := false
	for _, e := range events.events {
		if fmt.Sprintf("%s", e) != "" && contains(e, "failover") {
			foundFailover = true
		}
	}
	if !foundFailover {
		t.Fatalf("expected a failover event to be recorded, got: %v", events.events)
	}
}

func TestManager_NoBackupConfigured_NoFailover(t *testing.T) {
	m := NewManager(nil)
	m.Register("dev1", testDeviceChannels("primary", ""))

	for i := 0; i < DefaultDownThreshold; i++ {
		m.ReportResult("dev1", "primary", false)
	}

	state, _ := m.State("primary")
	if state != StateDown {
		t.Fatalf("expected primary to be down even without a backup, got %s", state)
	}
	active, _ := m.ActiveChannel("dev1")
	if active != "primary" {
		t.Fatalf("expected active channel to remain primary (nothing to fail over to), got %q", active)
	}
}

func TestManager_NoAutoFailback(t *testing.T) {
	// This is the DoD-critical test: after failover to backup, the
	// primary recovering (reporting success) must NOT automatically
	// switch the device back to primary - per CONTRACTS.md section 7,
	// failback is off by default.
	m := NewManager(nil)
	m.Register("dev1", testDeviceChannels("primary", "backup"))

	for i := 0; i < DefaultDownThreshold; i++ {
		m.ReportResult("dev1", "primary", false)
	}
	active, _ := m.ActiveChannel("dev1")
	if active != "backup" {
		t.Fatalf("expected failover to backup, got %q", active)
	}

	// Primary "recovers" - report a success on it directly (e.g. via an
	// out-of-band Test call).
	m.ReportResult("dev1", "primary", true)
	primaryState, _ := m.State("primary")
	if primaryState != StateOK {
		t.Fatalf("expected primary's own state to return to ok, got %s", primaryState)
	}

	// But the device must still be actively using backup - no automatic
	// failback.
	active, _ = m.ActiveChannel("dev1")
	if active != "backup" {
		t.Fatalf("expected active channel to remain backup (no auto-failback), got %q", active)
	}
}

func TestManager_SuccessResetsFailureCounter(t *testing.T) {
	m := NewManager(nil)
	m.Register("dev1", testDeviceChannels("primary", "backup"))

	m.ReportResult("dev1", "primary", false)
	m.ReportResult("dev1", "primary", false)
	m.ReportResult("dev1", "primary", true) // reset

	for i := 0; i < DefaultDegradedThreshold-1; i++ {
		m.ReportResult("dev1", "primary", false)
	}
	state, _ := m.State("primary")
	if state != StateOK {
		t.Fatalf("expected state ok (counter was reset by the earlier success), got %s", state)
	}
}

func TestManager_Test_UpdatesState(t *testing.T) {
	// Test() actually opens the transport (per its contract - it manages
	// Open/Close itself), so this needs a real listener to dial into,
	// unlike the pure-state-machine tests above which never touch Pick/Test.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start test listener: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)

	m := NewManager(nil)
	m.Register("dev1", DeviceChannels{
		Primary: ChannelSpec{ID: "primary", Params: transport.Params{Kind: transport.KindModbusTCP, Host: "127.0.0.1", Port: addr.Port}},
	})

	// Force primary into a failed state first (purely via ReportResult,
	// no network needed for this part).
	for i := 0; i < DefaultDownThreshold; i++ {
		m.ReportResult("dev1", "primary", false)
	}
	state, _ := m.State("primary")
	if state != StateDown {
		t.Fatalf("expected primary down before Test, got %s", state)
	}

	testErr := m.Test(context.Background(), "primary", func(ctx context.Context, tr transport.Transport) error {
		return nil // simulate a successful probe
	})
	if testErr != nil {
		t.Fatalf("unexpected error: %v", testErr)
	}

	state, _ = m.State("primary")
	if state != StateOK {
		t.Fatalf("expected state ok after a successful Test, got %s", state)
	}
}

func TestManager_Pick_UnregisteredDevice(t *testing.T) {
	m := NewManager(nil)
	if _, _, err := m.Pick(context.Background(), "unknown"); err == nil {
		t.Fatal("expected error for an unregistered device")
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
