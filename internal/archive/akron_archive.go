package archive

import (
	"context"
	"fmt"

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
// Required profile archive.params:
//   - "addr" (int): the device's 1-byte network address.
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

func akronAddrParam(params map[string]any) (byte, error) {
	raw, ok := params["addr"]
	if !ok {
		return 0, fmt.Errorf("akron_archive: missing required archive param %q in profile", "addr")
	}
	switch v := raw.(type) {
	case int:
		return byte(v), nil
	case int64:
		return byte(v), nil
	case float64:
		return byte(v), nil
	default:
		return 0, fmt.Errorf("akron_archive: archive param %q has unsupported type %T", "addr", raw)
	}
}

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
	addr, err := akronAddrParam(q.Params)
	if err != nil {
		return nil, err
	}
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

	var req []byte
	if kind == "hourly" {
		req = akron.BuildHourlyArchive(addr, startIndex, byte(count))
	} else {
		req = akron.BuildDailyArchive(addr, startIndex, byte(count))
	}

	respFrame, err := tx.Transact(ctx, req)
	if err != nil {
		return nil, err
	}

	_, _, data, err := akron.ParseResponse(respFrame)
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
		records = append(records, ArchiveRecord{
			Raw:    append([]byte(nil), raw...),
			Fields: fields,
			// No per-row CRC in Akron's archive format (only the
			// frame-level CRC, already validated by ParseResponse above).
			CRCOK: true,
		})
	}

	return records, nil
}

func init() { Register(NewAkronArchiveReader()) }
