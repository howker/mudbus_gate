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

// DefaultLogMaxBytes caps a carrier/discovery log so a stuck master
// hammering the same request cannot grow the file without bound (a live
// Энергосфера retry loop produced a 3 MB file in minutes — see VKM smoke,
// 21.07.2026). 0 means unbounded.
const DefaultLogMaxBytes int64 = 4 << 20 // 4 MiB

// DiscoveryLog appends DiscoveryEntry records to a JSONL file. Safe for
// concurrent use — discovery traffic is low-rate and human-inspected
// afterward, not a hot path, so a single mutex per write is fine.
//
// If MaxBytes > 0, writing stops once the file reaches that size (one final
// marker line is written), so a runaway master can't fill the disk. The
// cap is a safety net, not rotation: the intended fix for a flood is to
// stop the master looping (answer the register it's stuck on), not to
// silently discard evidence.
type DiscoveryLog struct {
	mu       sync.Mutex
	file     *os.File
	maxBytes int64
	written  int64
	capped   bool
}

// NewDiscoveryLog opens (creating if necessary, appending if it exists)
// the JSONL file at path, with the default size cap applied.
func NewDiscoveryLog(path string) (*DiscoveryLog, error) {
	return NewDiscoveryLogCapped(path, DefaultLogMaxBytes)
}

// NewDiscoveryLogCapped is NewDiscoveryLog with an explicit size cap in
// bytes (0 = unbounded).
func NewDiscoveryLogCapped(path string, maxBytes int64) (*DiscoveryLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("northbound: open discovery log %s: %w", path, err)
	}
	var start int64
	if info, statErr := f.Stat(); statErr == nil {
		start = info.Size()
	}
	return &DiscoveryLog{file: f, maxBytes: maxBytes, written: start}, nil
}

// Write appends one entry as a single JSON line. Timestamp defaults to
// now if unset. Once the size cap is reached, further writes are dropped
// (after a single "[log capped]" marker line).
func (l *DiscoveryLog) Write(e DiscoveryEntry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.maxBytes > 0 && l.written >= l.maxBytes {
		if !l.capped {
			l.capped = true
			marker := []byte(fmt.Sprintf(`{"ts":%q,"direction":"meta","note":"log capped at %d bytes; master likely stuck in a retry loop"}`+"\n",
				time.Now().Format(time.RFC3339), l.maxBytes))
			_, _ = l.file.Write(marker)
		}
		return nil
	}

	n, err := l.file.Write(line)
	l.written += int64(n)
	return err
}

func (l *DiscoveryLog) Close() error {
	return l.file.Close()
}
