package northbound

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"strconv"
	"time"

	"mbgw/internal/protocol/akron"
	"mbgw/internal/storage"
)

// AkronLiveResponder is the PRODUCTION counterpart of the discovery stub
// (simulator.AkronResponsePDU): it answers the same Акрон command set the
// Энергосфера driver speaks, but from the gateway's real database instead
// of YAML fixtures. Plugged into RawDiscoveryServer's responder slot, it
// turns the raw-RTU listener into the live model-B carrier:
//
//	101 → the polled device's real passport (serial/type/firmware);
//	 03 → the gateway's live wall clock for registers 0x10–0x13
//	      (archive-acceptance condition №1, M3_DISCOVERY_FINDINGS §4);
//	102 → current values from readings_current (+ latest hourly volume);
//	104 → real hourly rows from archive_hourly, encoded back to the wire
//	      row format, newest-first per Akron's i=1-is-top indexing
//	      (condition №2: real row timestamps);
//	105/106 → "no records" (daily archive and power log are not carried);
//	110 → the "ready" probe reply the driver requires before polling.
//
// Read-only by construction: it holds a storage.Repo and never any device
// transport, so it cannot touch real equipment (same safety property as
// the discovery server).
type AkronLiveResponder struct {
	repo     storage.Repo
	deviceID string

	// QueryTimeout bounds each DB lookup; zero means 5s.
	QueryTimeout time.Duration
}

func NewAkronLiveResponder(repo storage.Repo, deviceID string) *AkronLiveResponder {
	return &AkronLiveResponder{repo: repo, deviceID: deviceID}
}

func (r *AkronLiveResponder) timeout() time.Duration {
	if r.QueryTimeout > 0 {
		return r.QueryTimeout
	}
	return 5 * time.Second
}

// Respond maps an inbound RTU PDU to a response PDU (nil = stay silent).
// Matches the RawResponder signature used by RawDiscoveryServer.
func (r *AkronLiveResponder) Respond(pdu []byte) []byte {
	if len(pdu) < 1 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout())
	defer cancel()

	switch pdu[0] {
	case akron.CmdIdentification: // 101
		return r.identity(ctx)
	case 0x03, 0x04:
		return liveClockRead(pdu)
	case akron.CmdCurrentValues: // 102
		return r.currentValues(ctx)
	case akron.CmdHourlyArchive: // 104
		return r.hourlyArchive(ctx, pdu)
	case 105: // daily archive — not carried
		return []byte{105, 0x00}
	case 106: // power on/off log — the gateway keeps none
		return []byte{106, 0x00}
	case 110: // undocumented readiness probe
		return []byte{110, 0x01, 0x01}
	}
	return nil
}

// identity answers command 101 from the stored passport. With no passport
// yet (device not identified since startup) it stays SILENT rather than
// inventing a serial: the driver treats no-answer as "device unavailable",
// which is the truthful state.
func (r *AkronLiveResponder) identity(ctx context.Context) []byte {
	p, found, err := r.repo.GetDevicePassport(ctx, r.deviceID)
	if err != nil {
		log.Printf("[akron-live %s] паспорт: ошибка чтения: %v\n", r.deviceID, err)
		return nil
	}
	if !found {
		log.Printf("[akron-live %s] паспорт ещё не собран — на 101 молчим\n", r.deviceID)
		return nil
	}
	s := p.Serial
	return []byte{
		akron.CmdIdentification, 6,
		p.DeviceType, firmwareToBCD(p.Firmware),
		byte(s), byte(s >> 8), byte(s >> 16), byte(s >> 24),
	}
}

// firmwareToBCD packs a "major.minor" firmware string back into the single
// wire byte (high nibble = major, low nibble = minor): "3.7" → 0x37. This
// inverts akron.ParseIdentification's formatting. Unparseable input yields
// 0x00 — a visible "unknown version" rather than a fabricated one.
func firmwareToBCD(fw string) byte {
	var maj, min int
	if _, err := fmt.Sscanf(fw, "%d.%d", &maj, &min); err != nil {
		return 0x00
	}
	if maj < 0 || maj > 15 || min < 0 || min > 15 {
		return 0x00
	}
	return byte(maj<<4 | min)
}

// currentValues answers command 102 with the 18-byte layout captured in
// M3_DISCOVERY_FINDINGS §3: V(float LE) + Q(float LE) + U(4B LE) + Pu(1B) +
// operating time(4B LE) + fault code(1B). V/Q/acc_time come from
// readings_current; the accumulated volume U/Pu comes from the newest
// hourly archive row (the last known totalizer value), encoded via the
// same U/Pu packing the archive rows use. Missing readings encode as
// zeroes — the reply must keep its fixed shape.
func (r *AkronLiveResponder) currentValues(ctx context.Context) []byte {
	readings, err := r.repo.GetLatestReadings(ctx, r.deviceID)
	if err != nil {
		log.Printf("[akron-live %s] текущие: ошибка чтения: %v\n", r.deviceID, err)
		return nil
	}
	byName := map[string]float64{}
	for _, rd := range readings {
		if f, ok := toFloat(rd.Value); ok {
			byName[rd.PointID] = f
		}
	}

	var u int32
	var pu byte
	rows, err := r.repo.GetHourlyArchiveDesc(ctx, r.deviceID, "", "V", 0, 1)
	if err != nil {
		log.Printf("[akron-live %s] объём для 102: ошибка чтения часовок: %v\n", r.deviceID, err)
	} else if len(rows) == 1 {
		if eu, ep, eerr := akron.EncodeVolume(rows[0].Value); eerr == nil {
			u, pu = eu, ep
		}
	}

	data := make([]byte, 0, 18)
	data = appendFloatLE(data, float32(byName["V"]))
	data = appendFloatLE(data, float32(byName["Q"]))
	data = binary.LittleEndian.AppendUint32(data, uint32(u))
	data = append(data, pu)
	data = binary.LittleEndian.AppendUint32(data, uint32(int32(byName["acc_time"])))
	data = append(data, 0x00) // fault code: no fault information carried

	out := make([]byte, 0, 2+len(data))
	out = append(out, akron.CmdCurrentValues, byte(len(data)))
	return append(out, data...)
}

// hourlyArchive answers command 104. Request: 68 + i(2B big-endian) + n(1B),
// where i=1 is the newest row. Rows come straight from archive_hourly in
// newest-first order (offset = i-1), re-encoded to the 9-byte wire format
// with their REAL stored timestamps — which is what lets Энергосфера match
// rows against its ОИ-debt window. Fewer rows than requested (or none) is
// answered with exactly what exists ("чего нет — не отдаём").
func (r *AkronLiveResponder) hourlyArchive(ctx context.Context, pdu []byte) []byte {
	if len(pdu) < 4 {
		return nil
	}
	i := int(pdu[1])<<8 | int(pdu[2])
	n := int(pdu[3])
	if i < 1 || n < 1 {
		return []byte{akron.CmdHourlyArchive, 0x00}
	}
	if n > 31 { // documented per-request cap for the hourly archive
		n = 31
	}

	rows, err := r.repo.GetHourlyArchiveDesc(ctx, r.deviceID, "", "V", i-1, n)
	if err != nil {
		log.Printf("[akron-live %s] архив 104: ошибка чтения: %v\n", r.deviceID, err)
		return nil
	}

	data := make([]byte, 0, len(rows)*akron.HourlyRowSize)
	for _, row := range rows {
		wire, eerr := akron.EncodeHourlyRow(row.Value, row.TsHour)
		if eerr != nil {
			log.Printf("[akron-live %s] архив 104: строка %v не кодируется: %v\n", r.deviceID, row.TsHour, eerr)
			continue
		}
		data = append(data, wire...)
	}

	out := make([]byte, 0, 2+len(data))
	out = append(out, akron.CmdHourlyArchive, byte(len(data)))
	return append(out, data...)
}

// liveClockRead answers function 03/04 reads of the clock block
// (0x0010–0x0013) with the gateway's real current time in Akron's BCD
// register layout — the same logic the discovery responder proved against
// the live driver (simulator.akronLiveClockRead), duplicated here so
// northbound does not import the simulator package. Reads outside the
// clock block are not served (nil): the production carrier exposes no
// other registers over raw RTU.
func liveClockRead(pdu []byte) []byte {
	if len(pdu) < 5 {
		return nil
	}
	addr := int(pdu[1])<<8 | int(pdu[2])
	qty := int(pdu[3])<<8 | int(pdu[4])
	if addr != 0x0010 || qty < 1 || qty > 4 {
		return nil
	}

	now := time.Now()
	regs := [4][2]byte{
		{bcdByte(now.Second()), bcdByte(now.Minute())},
		{bcdByte(now.Hour()), byte(int(now.Weekday()))},
		{bcdByte(now.Day()), bcdByte(int(now.Month()))},
		{bcdByte(now.Year() % 100), 0x00},
	}
	data := make([]byte, 0, qty*2)
	for k := 0; k < qty; k++ {
		data = append(data, regs[k][0], regs[k][1])
	}
	out := make([]byte, 0, 2+len(data))
	out = append(out, pdu[0], byte(len(data)))
	return append(out, data...)
}

func bcdByte(v int) byte {
	v %= 100
	return byte((v/10)<<4 | (v % 10))
}

func appendFloatLE(dst []byte, f float32) []byte {
	return binary.LittleEndian.AppendUint32(dst, math.Float32bits(f))
}

// toFloat converts a stored reading value (persisted as text) to float64.
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	default:
		return 0, false
	}
}
