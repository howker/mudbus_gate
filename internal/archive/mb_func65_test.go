package archive

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

type func65Tx struct {
	lastReq []byte
	resp    []byte
}

func (f *func65Tx) Transact(_ context.Context, req []byte) ([]byte, error) {
	f.lastReq = append([]byte(nil), req...)
	return append([]byte(nil), f.resp...), nil
}

func TestMBFunc65IndexReadSetsRecordTimestamp(t *testing.T) {
	wantTS := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	raw := make([]byte, 8)
	binary.BigEndian.PutUint32(raw[0:4], uint32(wantTS.Unix()))
	binary.BigEndian.PutUint32(raw[4:8], math.Float32bits(12.5))

	tx := &func65Tx{resp: append([]byte{0x41, byte(len(raw))}, raw...)}
	reader := NewMBFunc65()
	recs, err := reader.Read(context.Background(), nil, tx, ArchiveQuery{
		FromIndex: 0,
		Params:    map[string]any{"archive_type": 0},
		RecordLayout: []RecordLayoutField{
			{Offset: 0, Name: "archive_time", Type: "uint32", Epoch: "1970-01-01"},
			{Offset: 4, Name: "v_plus", Type: "float", Unit: "m3"},
		},
		WordOrder32: "0123",
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	wantReq := []byte{0x41, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00}
	if !bytes.Equal(tx.lastReq, wantReq) {
		t.Fatalf("request = % X, want % X", tx.lastReq, wantReq)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if !recs[0].RecordTS.Equal(wantTS) {
		t.Fatalf("RecordTS = %v, want %v", recs[0].RecordTS, wantTS)
	}
	if got, ok := recs[0].Fields["v_plus"].(float64); !ok || got != 12.5 {
		t.Fatalf("v_plus = %#v, want 12.5", recs[0].Fields["v_plus"])
	}
}

func TestMBFunc65MissingRecordMarkerIsEmpty(t *testing.T) {
	raw := []byte{0xff, 0xff, 0xff, 0xff}
	tx := &func65Tx{resp: append([]byte{0x41, byte(len(raw))}, raw...)}
	recs, err := NewMBFunc65().Read(context.Background(), nil, tx, ArchiveQuery{
		FromIndex: 3,
		Params:    map[string]any{"archive_type": 0},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("records = %d, want 0", len(recs))
	}
}
