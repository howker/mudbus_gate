package monitor

import "time"

// Event is one comm_events-shaped occurrence (per FINAL_TRD.md section 7's
// comm_events domain entity), covering device/channel/scheduler lifecycle
// stages: channel degraded/down/failover, scheduler queue overflow, and
// any future publisher. Stage is a free-form string (matches how
// channel.EventRecorder/scheduler.EventRecorder already produce stage
// strings like "degraded"/"down"/"failover"/"queue_overflow_evicted").
type Event struct {
	DeviceID  string
	ChannelID string // optional - empty for non-channel-specific events
	Stage     string
	Detail    string
	Timestamp time.Time
}
