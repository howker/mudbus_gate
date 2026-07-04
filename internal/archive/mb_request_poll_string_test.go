package archive

import (
    "context"
    "testing"
    "time"

    "mbgw/internal/protocol/modbus"
)

// sequentialTransactor returns pre-programmed responses in call order,
// regardless of request content. Sufficient for testing the happy-path
// sequencing of a multi-step archive strategy like mb_request_poll_string.
type sequentialTransactor struct {
    responses [][]byte
    errs      []error
    calls     int
    requests  [][]byte
}

func (s *sequentialTransactor) Transact(ctx context.Context, req []byte) ([]byte, error) {
    s.requests = append(s.requests, append([]byte(nil), req...))
    idx := s.calls
    s.calls++

    var err error
    if idx < len(s.errs) {
        err = s.errs[idx]
    }
    if err != nil {
        return nil, err
    }
    if idx >= len(s.responses) {
        return nil, context.DeadlineExceeded
    }
    return s.responses[idx], nil
}

// echoWritePDU builds a fake PDU response for a write request, mimicking
// what a real device (and client.ModbusClient.wrapAndSend) would return:
// function code + echoed data, exception bit clear.
func echoWriteSingleResp() []byte {
    return []byte{0x06, 0x00, 0xC8, 0x00, 0x01}
}

func echoWriteMultipleResp(addr int, qty int) []byte {
    return []byte{0x10, byte(addr >> 8), byte(addr), byte(qty >> 8), byte(qty)}
}

func statusResp(status uint16) []byte {
    return []byte{0x04, 0x02, byte(status >> 8), byte(status)}
}

func lenResp(n uint16) []byte {
    return []byte{0x04, 0x02, byte(n >> 8), byte(n)}
}

func dataResp(payload []byte) []byte {
    return append([]byte{0x04, byte(len(payload))}, payload...)
}

func TestMBRequestPollString_HappyPath(t *testing.T) {
    payload := "V01{асход}=123.45 кг/с;V02{асса}=678.90 кг;"

    tx := &sequentialTransactor{
        responses: [][]byte{
            echoWriteSingleResp(),                 // write id (7900)
            echoWriteSingleResp(),                 // write pipe (7901)
            echoWriteMultipleResp(7902, 6),         // write start time
            echoWriteMultipleResp(7908, 6),         // write end time
            echoWriteSingleResp(),                 // write options (7914)
            statusResp(vkmArchStatusCollecting),    // poll status: collecting
            statusResp(vkmArchStatusReady),         // poll status: ready
            lenResp(uint16(len(payload))),          // read length
            dataResp([]byte(payload)),              // read string
        },
    }

    r := NewMBRequestPollString()
    q := ArchiveQuery{
        DeviceID:  "vkm_1",
        ArchiveID: "main",
        Instance:  1,
        From:      time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
        To:        time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
    }

    recs, err := r.Read(context.Background(), tx, q)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(recs) != 1 {
        t.Fatalf("expected 1 record, got %d", len(recs))
    }

    fields := recs[0].Fields
    if v, ok := fields["V01"].(float64); !ok || v != 123.45 {
        t.Fatalf("expected V01=123.45, got %v", fields["V01"])
    }
    if u, ok := fields["V01_unit"].(string); !ok || u != "кг/с" {
        t.Fatalf("expected V01_unit='кг/с', got %v", fields["V01_unit"])
    }
    if v, ok := fields["V02"].(float64); !ok || v != 678.90 {
        t.Fatalf("expected V02=678.90, got %v", fields["V02"])
    }

    if tx.calls != 9 {
        t.Fatalf("expected exactly 9 transactions, got %d", tx.calls)
    }
}

func TestMBRequestPollString_BusyThenSucceeds(t *testing.T) {
    payload := "V01{асход}=1.00 кг/с;"

    tx := &sequentialTransactor{
        responses: [][]byte{
            nil, // idx0: forced error below
        },
        errs: []error{
            &modbus.ExceptionError{Code: 6}, // first write attempt: BUSY
        },
    }
    // After the forced busy error at call 0, subsequent calls succeed normally.
    tx.responses = [][]byte{
        nil,                                 // idx0 unused (err forced)
        echoWriteSingleResp(),               // idx1: retry of write id succeeds
        echoWriteSingleResp(),                // write pipe
        echoWriteMultipleResp(7902, 6),        // write start time
        echoWriteMultipleResp(7908, 6),        // write end time
        echoWriteSingleResp(),                 // write options
        statusResp(vkmArchStatusReady),         // poll status: ready immediately
        lenResp(uint16(len(payload))),          // read length
        dataResp([]byte(payload)),              // read string
    }

    r := NewMBRequestPollString()
    q := ArchiveQuery{Instance: 1, From: time.Now(), To: time.Now()}

    recs, err := r.Read(context.Background(), tx, q)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(recs) != 1 {
        t.Fatalf("expected 1 record, got %d", len(recs))
    }
}

func TestMBRequestPollString_NoRecords(t *testing.T) {
    tx := &sequentialTransactor{
        responses: [][]byte{
            echoWriteSingleResp(),
            echoWriteSingleResp(),
            echoWriteMultipleResp(7902, 6),
            echoWriteMultipleResp(7908, 6),
            echoWriteSingleResp(),
            statusResp(vkmArchStatusNoRecords),
        },
    }

    r := NewMBRequestPollString()
    q := ArchiveQuery{Instance: 1, From: time.Now(), To: time.Now()}

    _, err := r.Read(context.Background(), tx, q)
    if err == nil {
        t.Fatal("expected error for no_records status, got nil")
    }
}