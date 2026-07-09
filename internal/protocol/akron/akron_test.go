package akron

import (
    "bytes"
    "testing"
)

func TestBuildCurrentValuesPDU_Golden(t *testing.T) {
    // Document's own worked example, RTU frame: 01 66 80 0А (addr=01,
    // code=0x66=102, CRC=80 0A). The PDU (bare, no address/CRC - those
    // are RTU transport framing) is just the single code byte: 66.
    got := BuildCurrentValuesPDU()
    want := []byte{0x66}
    if !bytes.Equal(got, want) {
        t.Fatalf("got % X, want % X", got, want)
    }
}

func TestParseResponsePDU_CurrentValues_Golden(t *testing.T) {
    // Same worked example's response PDU (address/CRC already stripped
    // by the RTU transport layer before reaching this package):
    // code=0x66, byteCount=0x12, 18 data bytes.
    pdu := []byte{
        0x66, 0x12,
        0xBD, 0x6D, 0xF7, 0x3E, 0x3B, 0xBB, 0xE7, 0x41,
        0xC3, 0x16, 0x00, 0x00, 0x02, 0xAE, 0x06, 0x00, 0x00, 0x00,
    }
    code, data, err := ParseResponsePDU(pdu)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if code != CmdCurrentValues {
        t.Fatalf("want code %d, got %d", CmdCurrentValues, code)
    }
    if len(data) != 18 {
        t.Fatalf("want 18 data bytes, got %d", len(data))
    }
}

func TestParseResponsePDU_ByteCountMismatch(t *testing.T) {
    // Declares byteCount=5 but only 2 data bytes follow.
    pdu := []byte{0x66, 0x05, 0xAA, 0xBB}
    if _, _, err := ParseResponsePDU(pdu); err == nil {
        t.Fatal("expected error for byte count mismatch")
    }
}

func TestParseResponsePDU_TooShort(t *testing.T) {
    if _, _, err := ParseResponsePDU([]byte{0x66}); err == nil {
        t.Fatal("expected error for a PDU with no byteCount byte")
    }
}

func TestBuildHourlyArchivePDU_ParamEncoding(t *testing.T) {
    // startIndex=1 (top of archive), n=5 rows.
    got := BuildHourlyArchivePDU(1, 5)
    want := []byte{0x68, 0x00, 0x01, 0x05} // code=104(0x68), i_hi, i_lo, n
    if !bytes.Equal(got, want) {
        t.Fatalf("got % X, want % X", got, want)
    }
}

func TestBuildDailyArchivePDU_ParamEncoding(t *testing.T) {
    got := BuildDailyArchivePDU(100, 10)
    want := []byte{0x69, 0x00, 0x64, 0x0A} // code=105(0x69), index 100=0x0064, n=10
    if !bytes.Equal(got, want) {
        t.Fatalf("got % X, want % X", got, want)
    }
}