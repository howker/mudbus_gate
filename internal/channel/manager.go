package channel

import (
	"context"
	"fmt"
	"sync"

	"mbgw/internal/transport"
)

// Package channel implements CONTRACTS.md section 7's channel state
// machine: ok -> degraded (>=3 consecutive failures) -> down (>=5) ->
// failover to the device's backup channel. Failback (automatically
// resuming the primary once it recovers) is OFF by default per the
// contract - a channel that recovers goes back to state "ok", but the
// device's *active* channel selection does not automatically switch back;
// only an explicit failback policy (not built here - see backlog) would
// do that.

// State is a channel's current health state.
type State string

const (
	StateOK       State = "ok"
	StateDegraded State = "degraded"
	StateDown     State = "down"
)

// Default consecutive-failure thresholds per CONTRACTS.md section 7:
// "Порог по умолчанию: 3 подряд неуспеха -> degraded; 5 -> down -> failover."
const (
	DefaultDegradedThreshold = 3
	DefaultDownThreshold     = 5
)

// EventRecorder receives channel lifecycle events (degraded/down/failover).
// This is a local interface (mirrors archive.ArchiveSession's layering
// rationale) so internal/channel does not depend on internal/monitor,
// which does not exist yet (IMPLEMENTATION_BACKLOG.md T12). Wire a real
// monitor.Bus-backed implementation once T12 is built; until then,
// NoopEventRecorder or a simple log-based one is enough.
type EventRecorder interface {
	RecordChannelEvent(deviceID, channelID, stage, detail string)
}

// NoopEventRecorder discards all events.
type NoopEventRecorder struct{}

func (NoopEventRecorder) RecordChannelEvent(deviceID, channelID, stage, detail string) {}

// ChannelSpec describes one physical channel's connection parameters.
type ChannelSpec struct {
	ID     string
	Params transport.Params
}

// DeviceChannels is a device's primary (required) and optional backup
// channel, per FINAL_TRD.md section 3.1: "Основной/резервный канал на
// прибор."
type DeviceChannels struct {
	Primary ChannelSpec
	Backup  *ChannelSpec
}

type channelRuntime struct {
	spec                ChannelSpec
	tr                  transport.Transport
	state               State
	consecutiveFailures int
}

// Manager tracks channel health per device and selects which channel to
// use for each transaction. Not safe for use by multiple Manager
// instances against the same device - like Transport itself, a given
// device's channels are owned by one Manager.
type Manager struct {
	mu                sync.Mutex
	devices           map[string]DeviceChannels
	runtimes          map[string]*channelRuntime // keyed by channel ID
	active            map[string]string          // deviceID -> active channel ID
	degradedThreshold int
	downThreshold     int
	events            EventRecorder
}

// NewManager creates a channel Manager. events may be nil (defaults to
// NoopEventRecorder).
func NewManager(events EventRecorder) *Manager {
	if events == nil {
		events = NoopEventRecorder{}
	}
	return &Manager{
		devices:           make(map[string]DeviceChannels),
		runtimes:          make(map[string]*channelRuntime),
		active:            make(map[string]string),
		degradedThreshold: DefaultDegradedThreshold,
		downThreshold:     DefaultDownThreshold,
		events:            events,
	}
}

// Register adds a device's channel configuration. Must be called before
// Pick/ReportResult for that device. Calling Register again for the same
// deviceID replaces its configuration and resets its active channel to
// the (new) primary.
func (m *Manager) Register(deviceID string, dc DeviceChannels) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.devices[deviceID] = dc
	if _, exists := m.runtimes[dc.Primary.ID]; !exists {
		m.runtimes[dc.Primary.ID] = &channelRuntime{spec: dc.Primary, state: StateOK}
	}
	m.active[deviceID] = dc.Primary.ID
	if dc.Backup != nil {
		if _, exists := m.runtimes[dc.Backup.ID]; !exists {
			m.runtimes[dc.Backup.ID] = &channelRuntime{spec: *dc.Backup, state: StateOK}
		}
	}
}

// Pick returns the currently active Transport for deviceID (opening it on
// first use) and the ID of the channel it belongs to. Callers must call
// ReportResult after each transaction attempt to drive the health state
// machine - Pick itself does not know whether the transaction succeeded.
func (m *Manager) Pick(ctx context.Context, deviceID string) (transport.Transport, string, error) {
	m.mu.Lock()
	dc, ok := m.devices[deviceID]
	if !ok {
		m.mu.Unlock()
		return nil, "", fmt.Errorf("channel: device %q not registered", deviceID)
	}
	activeID, ok := m.active[deviceID]
	if !ok {
		activeID = dc.Primary.ID
		m.active[deviceID] = activeID
	}
	rt := m.runtimes[activeID]
	m.mu.Unlock()

	if rt.tr == nil {
		tr, err := transport.New(rt.spec.Params)
		if err != nil {
			return nil, "", fmt.Errorf("channel: failed to create transport for %q: %w", activeID, err)
		}
		if err := tr.Open(ctx); err != nil {
			return nil, "", fmt.Errorf("channel: failed to open transport for %q: %w", activeID, err)
		}
		rt.tr = tr
	}
	return rt.tr, activeID, nil
}

// ReportResult updates channelID's health state machine after a
// transaction attempt for deviceID, triggering degraded/down/failover
// transitions per the thresholds. Call this once per transaction attempt
// (success or failure) on the Transport returned by Pick.
func (m *Manager) ReportResult(deviceID, channelID string, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rt, ok := m.runtimes[channelID]
	if !ok {
		return
	}

	if success {
		// A successful transaction resets this channel's own failure
		// counter and restores its state to ok - the "успешный
		// тест/опрос -> ok" transition. This does NOT switch the
		// device's active channel back to a recovered primary (that
		// would be failback, off by default per the contract).
		rt.consecutiveFailures = 0
		rt.state = StateOK
		return
	}

	rt.consecutiveFailures++
	switch {
	case rt.consecutiveFailures >= m.downThreshold:
		if rt.state != StateDown {
			rt.state = StateDown
			m.events.RecordChannelEvent(deviceID, channelID, "down", fmt.Sprintf("%d consecutive failures", rt.consecutiveFailures))
			m.tryFailover(deviceID, channelID)
		}
	case rt.consecutiveFailures >= m.degradedThreshold:
		if rt.state == StateOK {
			rt.state = StateDegraded
			m.events.RecordChannelEvent(deviceID, channelID, "degraded", fmt.Sprintf("%d consecutive failures", rt.consecutiveFailures))
		}
	}
}

// tryFailover switches deviceID's active channel away from channelID (which
// just went down) to its backup/primary counterpart, if one is configured
// and channelID is currently the active one. Must be called with m.mu held.
func (m *Manager) tryFailover(deviceID, channelID string) {
	dc := m.devices[deviceID]
	if dc.Backup == nil {
		return // no backup configured, nothing to fail over to
	}
	if m.active[deviceID] != channelID {
		return // the failed channel isn't even the active one
	}

	var target string
	if channelID == dc.Primary.ID {
		target = dc.Backup.ID
	} else {
		target = dc.Primary.ID
	}
	m.active[deviceID] = target
	m.events.RecordChannelEvent(deviceID, channelID, "failover", "switched to "+target)
}

// Test performs a lightweight round-trip on channelID to verify it is
// reachable, per CONTRACTS.md section 7: "Тест канала: лёгкая транзакция
// (чтение известного регистра / тест связи Меркурия)." The actual
// protocol-level probe is the caller's responsibility via testFunc; Test
// manages opening the transport (if needed) and updating the channel's
// health state based on testFunc's result.
func (m *Manager) Test(ctx context.Context, channelID string, testFunc func(context.Context, transport.Transport) error) error {
	m.mu.Lock()
	rt, ok := m.runtimes[channelID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("channel: channel %q not registered", channelID)
	}

	if rt.tr == nil {
		tr, err := transport.New(rt.spec.Params)
		if err != nil {
			return fmt.Errorf("channel: failed to create transport for %q: %w", channelID, err)
		}
		if err := tr.Open(ctx); err != nil {
			return fmt.Errorf("channel: failed to open transport for %q: %w", channelID, err)
		}
		rt.tr = tr
	}

	err := testFunc(ctx, rt.tr)

	m.mu.Lock()
	if err == nil {
		rt.consecutiveFailures = 0
		rt.state = StateOK
	} else {
		rt.consecutiveFailures++
	}
	m.mu.Unlock()

	return err
}

// State returns the current health state of channelID.
func (m *Manager) State(channelID string) (State, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.runtimes[channelID]
	if !ok {
		return "", false
	}
	return rt.state, true
}

// ActiveChannel returns which channel ID is currently active for deviceID.
func (m *Manager) ActiveChannel(deviceID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.active[deviceID]
	return id, ok
}

// Close closes every open transport this Manager has created.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for _, rt := range m.runtimes {
		if rt.tr != nil {
			if err := rt.tr.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
