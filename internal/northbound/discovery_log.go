package northbound

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// DiscoveryEntry is one logged Modbus TCP frame observed by DiscoveryServer
// (TRD addendum §B.4). Written as JSON Lines — one JSON object per line —
// so a captured session can be tailed, grepped, or replayed without
// parsing a wrapping array.
type DiscoveryEntry struct {
	Timestamp  time.Time `json:"ts"`
	RemoteAddr string    `json:"remote_addr"`
	Unit       byte      `json:"unit"`
	Function   byte      `json:"function"`
	// Address/Quantity are a quick-glance summary of the first two 16-bit
	// words of the PDU body, present whenever the body has at least 4
	// bytes. Their meaning depends on the function: for 03/04 Quantity is
	// a register count; for 05/06 it is actually the write value, not a
	// count; for 0F/10 it is a register/coil count (the byte-count and
	// data that follow are not summarized here). PayloadHex is always the
	// authoritative full PDU — these two fields exist only to make the
	// log skimmable.
	Address    uint16 `json:"address,omitempty"`
	Quantity   uint16 `json:"quantity,omitempty"`
	Direction  string `json:"direction"` // "request" | "response"
	PayloadHex string `json:"payload_hex"`
}

// DiscoveryLog appends DiscoveryEntry records to a JSONL file. Safe for
// concurrent use — discovery traffic is low-rate and human-inspected
// afterward, not a hot path, so a single mutex per write is fine.
type DiscoveryLog struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

// NewDiscoveryLog opens (creating if necessary, appending if it exists)
// the JSONL file at path.
func NewDiscoveryLog(path string) (*DiscoveryLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("northbound: open discovery log %s: %w", path, err)
	}
	return &DiscoveryLog{file: f, enc: json.NewEncoder(f)}, nil
}

// Write appends one entry as a single JSON line. Timestamp defaults to
// now if unset.
func (l *DiscoveryLog) Write(e DiscoveryEntry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enc.Encode(e) // json.Encoder.Encode appends the trailing newline itself
}

func (l *DiscoveryLog) Close() error {
	return l.file.Close()
}
