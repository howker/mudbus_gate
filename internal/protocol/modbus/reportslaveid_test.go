package modbus

import (
    "bytes"
    "testing"
)

func TestBuildReportSlaveIDPDU(t *testing.T) {
    got := BuildReportSlaveIDPDU()
    want := []byte{0x11}
    if !bytes.Equal(got, want) {
        t.Fatalf("expected %x, got %x", want, got)
    }
}

func TestParseReportSlaveIDResponse_Basic(t *testing.T) {
    // funcCode(0x11) + byteCount(3) + [runStatus(0xFF), 'A', 'B']
    pdu := []byte{0x11, 0x03, 0xFF, 'A', 'B'}

    resp, err := ParseReportSlaveIDResponse(pdu)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if resp.RunStatus != 0xFF {
        t.Fatalf("expected RunStatus=0xFF, got 0x%02X", resp.RunStatus)
    }
    if !bytes.Equal(resp.RawData, []byte{'A', 'B'}) {
        t.Fatalf("expected RawData=AB, got %v", resp.RawData)
    }
}

func TestParseReportSlaveIDResponse_Truncated(t *testing.T) {
    pdu := []byte{0x11, 0x05, 0xFF} // claims 5 bytes, has only 1
    if _, err := ParseReportSlaveIDResponse(pdu); err == nil {
        t.Fatal("expected error for truncated response, got nil")
    }
}

func TestParseReportSlaveIDResponse_Exception(t *testing.T) {
    pdu := []byte{0x91, 0x01} // 0x11 | 0x80 = exception, code 01
    if _, err := ParseReportSlaveIDResponse(pdu); err == nil {
        t.Fatal("expected exception error, got nil")
    }
}

func TestParseReportSlaveIDResponse_WrongFuncCode(t *testing.T) {
    pdu := []byte{0x03, 0x02, 0x00, 0x01}
    if _, err := ParseReportSlaveIDResponse(pdu); err == nil {
        t.Fatal("expected error for wrong function code, got nil")
    }
}