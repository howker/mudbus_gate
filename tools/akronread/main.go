// Command akronread is a standalone southbound reader for a REAL Акрон-01 /
// Акрон-02-1 flow meter: it opens a raw-RTU-over-TCP connection to the
// device, sends the identification (101) and current-values (102) commands,
// decodes the responses, and prints them.
//
// This is the client counterpart to tools/akronprobe (which drives our
// northbound carrier). akronread instead talks to the ACTUAL device, using
// the same protocol/codec the gateway uses everywhere else:
//
//	transport (raw RTU over TCP) -> modbus.Transact -> akron PDU -> codec
//
// It writes nothing to the database — it is a read-only "proof of life"
// that the gateway can poll the real Акрон. Wiring these readings into
// SQLite (so the northbound carrier serves real data) is the next step and
// reuses the existing persist path.
//
// Usage (TCP — device reachable as a raw socket, e.g. via a transparent
// Ethernet/RS-485 converter that does NOT emulate a COM port):
//
//	akronread --host 10.48.228.50 --port 5001 [--unit 1] [--timeout 1s]
//
// Usage (COM port — the more common case here: an Ethernet/RS-485
// converter's own software emulates a COM port on the ЭС machine, e.g.
// COM105):
//
//	akronread --com COM105 --baud 9600 --unit 1 [--parity none] [--stopbits 1]
//
//	[--hourly N]   # also dump N rows of the hourly archive as hex
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/codec"
	"mbgw/internal/protocol/akron"
	"mbgw/internal/protocol/modbus"
	"mbgw/internal/storage"
	sqliterepo "mbgw/internal/storage/sqlite"
	"mbgw/internal/transport"
)

// akronOrder32 is the 4-byte wire order for Акрон: little-endian ("младшим
// байтом вперёд" per the device document), which this codec spells "3210".
const akronOrder32 = "3210"

func main() {
	host := flag.String("host", "", "device IP/hostname (TCP mode)")
	port := flag.Int("port", 0, "device TCP port (TCP mode)")
	com := flag.String("com", "", "COM port, e.g. COM105 (serial mode — use this if the converter emulates a COM port, not a raw socket)")
	baud := flag.Int("baud", 9600, "baud rate (serial mode): 1200|2400|4800|9600 per the device doc")
	parity := flag.String("parity", "none", "parity (serial mode): none|even|odd")
	stopbits := flag.Int("stopbits", 1, "stop bits (serial mode): 1|2")
	unit := flag.Int("unit", 1, "device bus address (Modbus unit id)")
	timeout := flag.Duration("timeout", time.Second, "response timeout")
	retries := flag.Int("retries", 3, "transport retries")
	hourly := flag.Int("hourly", 0, "if >0, also read that many hourly-archive rows (raw hex)")
	savePassport := flag.Bool("save-passport", false, "persist the real device passport (101 response) into SQLite, so the northbound carrier can answer identification (101) — see internal/storage.SaveDevicePassport")
	dbPath := flag.String("db", "", "SQLite path (required with --save-passport) — MUST be the same file the southbound poller (mbgw run) and northbound carrier use")
	deviceID := flag.String("device", "", "device id in the database (required with --save-passport) — MUST match config.yaml's devices[].id")
	flag.Parse()

	var params transport.Params
	switch {
	case *com != "":
		params = transport.Params{
			Kind:            transport.KindRTUSerial,
			COM:             *com,
			Baudrate:        *baud,
			Parity:          *parity,
			StopBits:        *stopbits,
			ResponseTimeout: *timeout,
			Retries:         *retries,
		}
	case *host != "" && *port != 0:
		params = transport.Params{
			Kind:            transport.KindTCPSerial, // raw RTU framing over a TCP socket
			Host:            *host,
			Port:            *port,
			ResponseTimeout: *timeout,
			Retries:         *retries,
		}
	default:
		fmt.Fprintln(os.Stderr, "akronread: specify either --com <port> (serial/virtual-COM mode) or --host/--port (raw TCP mode)")
		flag.Usage()
		os.Exit(2)
	}

	tr, err := transport.New(params)
	if err != nil {
		fatal("создание транспорта", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := tr.Open(ctx); err != nil {
		fatal("подключение к прибору", err)
	}
	defer tr.Close()

	if *com != "" {
		fmt.Printf("Подключено к %s (%d бод, адрес %d)\n\n", *com, *baud, *unit)
	} else {
		fmt.Printf("Подключено к %s:%d (адрес %d)\n\n", *host, *port, *unit)
	}

	// --- Command 101: identification ---
	var ident akronIdentity
	var identOK bool
	if data, err := transact(ctx, tr, uint8(*unit), akron.BuildIdentificationPDU(), akron.CmdIdentification); err != nil {
		fmt.Printf("101 идентификация: ОШИБКА: %v\n", err)
	} else if ident, identOK = decodeIdentification(data); identOK {
		printIdentification(ident)
	} else {
		fmt.Printf("101 идентификация: короткий ответ (% X)\n", data)
	}

	if *savePassport {
		fmt.Println()
		savePassportToDB(ctx, ident, identOK, *dbPath, *deviceID)
	}

	fmt.Println()

	// --- Command 102: current values ---
	if data, err := transact(ctx, tr, uint8(*unit), akron.BuildCurrentValuesPDU(), akron.CmdCurrentValues); err != nil {
		fmt.Printf("102 текущие значения: ОШИБКА: %v\n", err)
	} else {
		printCurrent(data)
	}

	// --- Command 104: hourly archive, DECODED via the real production
	// path (archive.AkronArchiveReader) — proves the actual decode logic
	// (BCD calendar + akron_volume scaling) works on real device bytes,
	// not just that raw bytes come back. ---
	if *hourly > 0 {
		fmt.Println()
		n := *hourly
		if n > 31 {
			n = 31 // per the document, max 31 rows per request
		}
		readHourlyArchive(ctx, tr, uint8(*unit), n)
	}
}

// hourlyRowLayout is the Акрон hourly-archive row format (9 bytes total),
// per "ВЗАИМОДЕЙСТВИЕ С КОНТРОЛЛЕРОМ СЕТИ MODBUS..." table 1: U(4B float,
// little-endian raw)+Pu(1B) as one akron_volume field (5 bytes), then
// H/D/M/Y as 1-byte BCD fields — mirrors the record_layout a real device
// profile would declare (see internal/device/akron_hourly.go).
var hourlyRowLayout = []archive.RecordLayoutField{
	{Offset: 0, Name: "volume", Type: "akron_volume", Unit: "м³"},
	{Offset: 5, Name: "hour", Type: "bcd"},
	{Offset: 6, Name: "day", Type: "bcd"},
	{Offset: 7, Name: "month", Type: "bcd"},
	{Offset: 8, Name: "year", Type: "bcd"},
}

// akronTransactor adapts our transport+unit into archive.Transactor
// (bare PDU in, bare PDU out) — the same shape every archive strategy in
// the real pipeline expects.
type akronTransactor struct {
	tr   transport.Transport
	unit uint8
}

func (a akronTransactor) Transact(ctx context.Context, req []byte) ([]byte, error) {
	return modbus.Transact(ctx, a.tr, false, 0, a.unit, req)
}

func readHourlyArchive(ctx context.Context, tr transport.Transport, unit uint8, n int) {
	reader := archive.NewAkronArchiveReader()
	q := archive.ArchiveQuery{
		DeviceID:     "akronread",
		ArchiveID:    "hourly",
		FromIndex:    0,
		ToIndex:      n - 1, // Read() counts rows as ToIndex-FromIndex+1
		RecordLayout: hourlyRowLayout,
		WordOrder32:  akronOrder32,
		Params:       map[string]any{"archive_kind": "hourly"},
	}
	records, err := reader.Read(ctx, nil, akronTransactor{tr: tr, unit: unit}, q)
	if err != nil {
		fmt.Printf("104 часовой архив: ОШИБКА: %v\n", err)
		return
	}

	fmt.Printf("104 часовой архив — %d строк (i=1 = вершина/самая свежая):\n", len(records))
	for i, rec := range records {
		ts, tsOK := akronRowTime(rec.Fields)
		vol, volOK := fieldFloat(rec.Fields, "volume")
		switch {
		case tsOK && volOK:
			fmt.Printf("  [%2d] %s   V = %.4f м³\n", i+1, ts.Format("02.01.2006 15:00"), vol)
		default:
			fmt.Printf("  [%2d] не декодировано (поля=%v, raw=% X)\n", i+1, rec.Fields, rec.Raw)
		}
	}
}

// akronRowTime mirrors internal/device/akron_hourly.go's akronRowTime
// exactly (kept in sync deliberately — this tool exists to validate that
// same production decode path against real device bytes). "year" is
// year-2000 (two BCD digits), matching the device's own convention.
func akronRowTime(fields map[string]any) (time.Time, bool) {
	hour, ok1 := fieldInt(fields, "hour")
	day, ok2 := fieldInt(fields, "day")
	month, ok3 := fieldInt(fields, "month")
	yy, ok4 := fieldInt(fields, "year")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return time.Time{}, false
	}
	if hour < 0 || hour > 23 || day < 1 || day > 31 || month < 1 || month > 12 || yy < 0 || yy > 99 {
		return time.Time{}, false
	}
	return time.Date(2000+yy, time.Month(month), day, hour, 0, 0, 0, time.Local), true
}

func fieldInt(fields map[string]any, key string) (int, bool) {
	switch n := fields[key].(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func fieldFloat(fields map[string]any, key string) (float64, bool) {
	switch n := fields[key].(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// transact sends one Akron command PDU over RTU and returns the response
// data field (after code + byteCount), validating the echoed command code.
func transact(ctx context.Context, tr transport.Transport, unit uint8, reqPDU []byte, wantCode byte) ([]byte, error) {
	// isTCP=false: Akron uses raw RTU framing (address + PDU + CRC), even
	// when that RTU frame is carried over a TCP socket.
	respPDU, err := modbus.Transact(ctx, tr, false, 0, unit, reqPDU)
	if err != nil {
		return nil, err
	}
	code, data, err := akron.ParseResponsePDU(respPDU)
	if err != nil {
		return nil, err
	}
	if code != wantCode {
		return nil, fmt.Errorf("прибор ответил кодом %d, ожидался %d", code, wantCode)
	}
	return data, nil
}

// akronIdentity is the decoded command-101 response.
type akronIdentity struct {
	DeviceType byte
	VerBCD     byte
	Serial     uint32
}

func decodeIdentification(data []byte) (akronIdentity, bool) {
	// [type 1B][version 1B BCD][serial 4B LE word], per the document.
	if len(data) < 6 {
		return akronIdentity{}, false
	}
	return akronIdentity{
		DeviceType: data[0],
		VerBCD:     data[1],
		Serial:     binary.LittleEndian.Uint32(data[2:6]),
	}, true
}

func printIdentification(id akronIdentity) {
	fmt.Println("101 идентификация:")
	fmt.Printf("  тип прибора:     0x%02X\n", id.DeviceType)
	fmt.Printf("  версия ПО:       %d.%d\n", id.VerBCD>>4, id.VerBCD&0x0F)
	fmt.Printf("  заводской номер: %d\n", id.Serial)
}

// savePassportToDB writes the REAL, just-read device identity into the
// SAME SQLite database the southbound poller (mbgw run) and northbound
// carrier (mbgw northbound --serve-akron) both use, via
// storage.SaveDevicePassport — the exact call tools/seedakron uses for its
// fake test values, but here with values read live off the actual device
// instead of hand-picked ones.
//
// Why this is needed at all: the northbound carrier stays SILENT on
// command 101 until a passport row exists ("паспорт ещё не собран — на
// 101 молчим", internal/northbound/akron_live.go) — the southbound poller
// (mbgw run) never writes one on its own, since identification is a
// one-off static fact about the device, not a per-cycle reading. This is
// a manual, run-once (or run-when-it-changes) step, not part of the
// regular poll loop.
func savePassportToDB(ctx context.Context, id akronIdentity, identOK bool, dbPath, deviceID string) {
	if dbPath == "" || deviceID == "" {
		fmt.Println("--save-passport требует --db <путь> и --device <id>")
		os.Exit(2)
	}
	if !identOK {
		fmt.Println("--save-passport: идентификация не получена, паспорт не сохранён")
		os.Exit(1)
	}

	repo, err := sqliterepo.New(dbPath)
	if err != nil {
		fatal("открытие БД для сохранения паспорта", err)
	}
	defer repo.Close()

	fw := fmt.Sprintf("%d.%d", id.VerBCD>>4, id.VerBCD&0x0F)
	err = repo.SaveDevicePassport(ctx, storage.DevicePassport{
		DeviceID:   deviceID,
		DeviceType: id.DeviceType,
		Firmware:   fw,
		Serial:     id.Serial,
	})
	if err != nil {
		fatal("сохранение паспорта", err)
	}
	fmt.Printf("паспорт сохранён: device=%s type=0x%02X fw=%s serial=%d (%s)\n",
		deviceID, id.DeviceType, fw, id.Serial, dbPath)
}

func printCurrent(data []byte) {
	// [V 4B float][Q 4B float][U 4B][Pu 1B][runtime 4B][fault 1B] = 18 bytes.
	if len(data) < 18 {
		fmt.Printf("102 текущие значения: короткий ответ (% X)\n", data)
		return
	}
	v := floatLE(data[0:4])
	q := floatLE(data[4:8])
	vol, volErr := codec.DecodeAkronVolume(data[8:13], akronOrder32) // U(4)+Pu(1)
	runtime, _ := codec.DecodeInt32(data[13:17], akronOrder32)
	fault := data[17]

	fmt.Println("102 текущие значения:")
	fmt.Printf("  V (скорость):     %.4f м/с\n", v)
	fmt.Printf("  Q (расход):       %.4f м³/ч\n", q)
	if volErr != nil {
		fmt.Printf("  U (объём):        ошибка декодирования: %v\n", volErr)
	} else {
		fmt.Printf("  U (объём):        %.4f м³\n", vol)
	}
	fmt.Printf("  время работы:     %d мин\n", runtime)
	if fault == 0 {
		fmt.Printf("  код неисправности: 0 (неисправностей нет)\n")
	} else {
		fmt.Printf("  код неисправности: %d\n", fault)
	}
}

// floatLE decodes a little-endian IEEE-754 float32 (Акрон wire order).
func floatLE(b []byte) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "akronread: %s: %v\n", what, err)
	os.Exit(1)
}
