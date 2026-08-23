package archive

import (
	"context"
	"encoding/hex"
	"fmt"

	"mbgw/internal/dbg"
	"mbgw/internal/protocol/akron"
)

// AkronArchiveReader is the archive-read strategy for Akron-01/02's
// hourly (command 104) and daily (command 105) archives, per
// "ВЗАИМОДЕЙСТВИЕ С КОНТРОЛЛЕРОМ СЕТИ MODBUS ПРИБОРОВ Акрон-01 и
// Акрон-02-1". Both commands share one row format: U(4B)+Pu(1B)+
// [H(1B)/]D(1B)+M(1B)+Y(1B), all BCD except U/Pu - hourly rows include
// the extra H (hour) field, daily rows don't, so row size (and hence
// which archive_kind) is what distinguishes them; the profile's own
// record_layout determines the actual row size.
//
// Requests/responses here are bare PDUs (code+params / code+byteCount+
// data) - no device address, no CRC. Those are RTU transport framing
// concerns (address = unitID at the transport/pollcore.Reader level, CRC
// = protocol/modbus.Transact's BuildRTUFrame/ParseRTUFrame), exactly the
// same layering as VZLET's function 65. An earlier version of this
// strategy built complete RTU frames itself (address+code+params+CRC)
// and required a special raw transactor to avoid double-framing - that
// was the wrong fix for the wrong layer; the real fix is building a bare
// PDU here and letting the normal Transact path (used by every other
// strategy) frame it once, correctly.
//
// Required profile archive.params:
//   - "archive_kind" (string): "hourly" (command 104, N<=31 rows,
//     M=1925 total) or "daily" (command 105, N<=36 rows, M=2200 total).
//
// ArchiveQuery.FromIndex/ToIndex are 0-based (consistent with every
// other strategy in this package); Akron's own "i" convention is 1-based
// (i=1 is the archive's most recent row, i=M its oldest) - the +1
// translation happens only at the point of building the request, so
// callers never need to think in Akron's 1-based terms.
type AkronArchiveReader struct{}

func NewAkronArchiveReader() *AkronArchiveReader { return &AkronArchiveReader{} }

func (r *AkronArchiveReader) Strategy() string { return "akron_archive" }

func akronArchiveKindParam(params map[string]any) (string, error) {
	raw, ok := params["archive_kind"]
	if !ok {
		return "", fmt.Errorf("akron_archive: missing required archive param %q in profile", "archive_kind")
	}
	kind, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("akron_archive: archive param %q has unsupported type %T", "archive_kind", raw)
	}
	if kind != "hourly" && kind != "daily" {
		return "", fmt.Errorf("akron_archive: archive param %q must be \"hourly\" or \"daily\", got %q", "archive_kind", kind)
	}
	return kind, nil
}

// maxRowsFor returns the documented maximum record count N per request
// for the given archive kind (section 2, table 1).
func maxRowsFor(kind string) int {
	if kind == "hourly" {
		return 31
	}
	return 36
}

func (r *AkronArchiveReader) Read(ctx context.Context, sess ArchiveSession, tx Transactor, q ArchiveQuery) ([]ArchiveRecord, error) {
	kind, err := akronArchiveKindParam(q.Params)
	if err != nil {
		return nil, err
	}

	rowSize := recordLayoutSize(q.RecordLayout)
	if rowSize <= 0 {
		return nil, fmt.Errorf("akron_archive: record_layout is empty or has zero total size")
	}

	count := 1
	if q.ToIndex > q.FromIndex {
		count = q.ToIndex - q.FromIndex + 1
	}
	maxN := maxRowsFor(kind)
	if count > maxN {
		count = maxN
	}

	// Akron's own "i" is 1-based (i=1 is the top/most recent row);
	// ArchiveQuery.FromIndex is 0-based per this package's convention.
	startIndex := uint16(q.FromIndex + 1)

	var reqPDU []byte
	if kind == "hourly" {
		reqPDU = akron.BuildHourlyArchivePDU(startIndex, byte(count))
	} else {
		reqPDU = akron.BuildDailyArchivePDU(startIndex, byte(count))
	}

	// DIAGNOSTIC LOGGING (added 2026-08-22): raw request/response hex, so a
	// physically-impossible decoded value (e.g. an hourly volume ~200x a
	// normal reading, observed live in Энергосфера's Mains table for a
	// period no longer present in mbgw's own archive_hourly — meaning the
	// bad value was already gone by the time this was investigated, with
	// no raw bytes left to diagnose it from) can be traced to its exact
	// wire bytes the NEXT time it happens, instead of reasoning from the
	// decoded value alone. Deliberately logs the readable request/response
	// bytes only — no behavior change.
	dbg.Printf("[akron_archive] запрос: kind=%s startIndex=%d count=%d pdu=%s\n",
		kind, startIndex, count, hex.EncodeToString(reqPDU))

	respPDU, err := tx.Transact(ctx, reqPDU)
	if err != nil {
		return nil, err
	}

	dbg.Printf("[akron_archive] ответ: pdu=%s\n", hex.EncodeToString(respPDU))

	_, data, err := akron.ParseResponsePDU(respPDU)
	if err != nil {
		return nil, err
	}

	if len(data)%rowSize != 0 {
		return nil, fmt.Errorf("akron_archive: response length %d is not a multiple of row size %d", len(data), rowSize)
	}

	numRows := len(data) / rowSize
	records := make([]ArchiveRecord, 0, numRows)
	for i := 0; i < numRows; i++ {
		raw := data[i*rowSize : (i+1)*rowSize]
		fields := decodeByLayout(raw, q.RecordLayout, q.WordOrder32, q.WordOrder64)

		// Per-row raw bytes too — cheap, and this is exactly what's needed
		// to tell "bad bytes on the wire" (real device/transport problem)
		// apart from "good bytes, bad decode" (our code's problem) the
		// next time a value like the 2026-08-21 22:00 anomaly (V≈2.19e6
		// m3, ~15000x a normal hourly reading — physically impossible,
		// confirmed by the operator) shows up.
		dbg.Printf("[akron_archive] строка %d: raw=%s поля=%v\n", i, hex.EncodeToString(raw), fields)

		records = append(records, ArchiveRecord{
			Raw:    append([]byte(nil), raw...),
			Fields: fields,
			// No per-row CRC in Akron's archive format (only the RTU
			// frame-level CRC, already validated by the transport layer
			// before this code ever sees the PDU).
			CRCOK: true,
		})
	}

	return records, nil
}

func init() { Register(NewAkronArchiveReader()) }
