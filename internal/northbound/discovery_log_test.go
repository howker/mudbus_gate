package northbound

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoveryLog_WriteAndReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discovery.jsonl")

	log, err := NewDiscoveryLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Write(DiscoveryEntry{RemoteAddr: "1.2.3.4:5", Unit: 1, Function: 3, Direction: "request", PayloadHex: "0000000a"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Write(DiscoveryEntry{RemoteAddr: "1.2.3.4:5", Unit: 1, Function: 3, Direction: "response", PayloadHex: "03140000"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var lines []DiscoveryEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e DiscoveryEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad JSONL line: %v", err)
		}
		lines = append(lines, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines, got %d", len(lines))
	}
	if lines[0].Direction != "request" || lines[1].Direction != "response" {
		t.Fatalf("unexpected order/direction: %+v", lines)
	}
	if lines[0].Timestamp.IsZero() {
		t.Fatal("expected auto-filled timestamp")
	}
}

func TestDiscoveryLog_AppendsAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discovery.jsonl")

	l1, _ := NewDiscoveryLog(path)
	l1.Write(DiscoveryEntry{Unit: 1, Function: 3, Direction: "request", PayloadHex: "aa"})
	l1.Close()

	l2, _ := NewDiscoveryLog(path)
	l2.Write(DiscoveryEntry{Unit: 1, Function: 3, Direction: "response", PayloadHex: "bb"})
	l2.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	count := 0
	for sc.Scan() {
		count++
	}
	if count != 2 {
		t.Fatalf("expected 2 lines across two opens, got %d", count)
	}
}
