// Command akronbackfill fills archive_hourly with N hours of REAL
// historical archive rows read directly from a real Акрон-01 device,
// bypassing the normal per-cycle poller (which only ever fetches the
// single newest row, per internal/device/akron_hourly.go's comment "collect
// once, serve many times").
//
// Why this exists: once the northbound carrier (internal/northbound/
// akron_live.go) started answering real archive requests, Энергосфера's
// own catch-up logic asked for OLDER rows (observed live: i=141, n=28 —
// roughly 6 days back, matching её "Основные интервалы: 7 сут" debt) and
// got an empty reply, because archive_hourly only had the single most
// recent row the regular poller had collected since startup — there was
// no real history to serve. The real device DOES hold that history in its
// own memory (confirmed earlier: akronread --hourly 10 matched the
// vendor's own archive tool byte-for-byte); this tool pulls it once and
// seeds our database with it, closing the gap in one pass.
//
// This is a stand-in for a general configurable-depth backfill inside the
// gateway itself (planned, per the operator: "в дальнейшем будем
// настраивать в МШ какую глубину тянуть с прибора") — not the final
// design, just what's needed right now to let Энергосфера's dozabor
// actually find real rows.
//
// Usage:
//
//	akronbackfill --com COM105 --baud 9600 --unit 1 --hours 170 \
//	              --db mbgw.db --device akron_real
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/protocol/modbus"
	"mbgw/internal/storage"
	sqliterepo "mbgw/internal/storage/sqlite"
	"mbgw/internal/transport"
)

const akronOrder32 = "3210" // Акрон wire order: little-endian ("младшим байтом вперёд")

var hourlyRowLayout = []archive.RecordLayoutField{
	{Offset: 0, Name: "volume", Type: "akron_volume", Unit: "м³"},
	{Offset: 5, Name: "hour", Type: "bcd"},
	{Offset: 6, Name: "day", Type: "bcd"},
	{Offset: 7, Name: "month", Type: "bcd"},
	{Offset: 8, Name: "year", Type: "bcd"},
}

func main() {
	host := flag.String("host", "", "device IP/hostname (TCP mode)")
	port := flag.Int("port", 0, "device TCP port (TCP mode)")
	com := flag.String("com", "", "COM port, e.g. COM105 (serial mode)")
	baud := flag.Int("baud", 9600, "baud rate (serial mode)")
	parity := flag.String("parity", "none", "parity (serial mode): none|even|odd")
	stopbits := flag.Int("stopbits", 1, "stop bits (serial mode): 1|2")
	unit := flag.Int("unit", 1, "device bus address (Modbus unit id)")
	timeout := flag.Duration("timeout", time.Second, "response timeout")
	retries := flag.Int("retries", 3, "transport retries")
	hours := flag.Int("hours", 170, "how many hourly rows to backfill, oldest reachable to newest")
	dbPath := flag.String("db", "", "SQLite path (required) — same file the southbound poller/northbound carrier use")
	deviceID := flag.String("device", "", "device id in the database (required) — must match config.yaml's devices[].id")
	unitLabel := flag.String("unit-label", "m3", "unit label stored alongside each Value")
	pageSize := flag.Int("page-size", 27, "max rows per single archive request — found live for THIS device/firmware (v6.6): the documented limit is 31, but 28+ gets rejected with modbus exception 0xFC while 27 works. Not assumed to be universal — override per device if a different unit/firmware rejects at a different size.")
	flag.Parse()

	if *dbPath == "" || *deviceID == "" {
		fmt.Fprintln(os.Stderr, "akronbackfill: --db and --device are required")
		os.Exit(2)
	}

	var params transport.Params
	switch {
	case *com != "":
		params = transport.Params{
			Kind: transport.KindRTUSerial, COM: *com, Baudrate: *baud,
			Parity: *parity, StopBits: *stopbits,
			ResponseTimeout: *timeout, Retries: *retries,
		}
	case *host != "" && *port != 0:
		params = transport.Params{
			Kind: transport.KindTCPSerial, Host: *host, Port: *port,
			ResponseTimeout: *timeout, Retries: *retries,
		}
	default:
		fmt.Fprintln(os.Stderr, "akronbackfill: specify either --com <port> or --host/--port")
		os.Exit(2)
	}

	tr, err := transport.New(params)
	if err != nil {
		fatal("создание транспорта", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := tr.Open(ctx); err != nil {
		fatal("подключение к прибору", err)
	}
	defer tr.Close()

	repo, err := sqliterepo.New(*dbPath)
	if err != nil {
		fatal("открытие БД", err)
	}
	defer repo.Close()
	if err := repo.InitArchiveSchema(ctx); err != nil {
		fatal("инициализация схемы архива", err)
	}

	fmt.Printf("Дозабор %d часов с реального прибора в %s (device=%s)...\n", *hours, *dbPath, *deviceID)

	// The real device rejects a single archive request bigger than its
	// own per-request cap (Modbus exception 0xFC, observed live: this
	// unit/firmware v6.6 rejects 28+ rows but accepts 27, DESPITE the
	// documented map saying 31 — found by direct probing with akronread
	// --hourly N, no rebuild needed, exactly because this limit can NOT
	// be trusted from documentation alone and will likely differ again
	// on other units/firmwares ("зоопарк приборов"). --page-size makes
	// this a runtime value, not a hardcoded guess.
	// akron.BuildHourlyArchivePDU/AkronArchiveReader don't chunk
	// automatically, so this loop pages through in blocks of ≤page-size.
	reader := archive.NewAkronArchiveReader()
	txr := akronTransactor{tr: tr, unit: uint8(*unit)}

	saved, skipped, gotTotal := 0, 0, 0
	for from := 0; from < *hours; from += *pageSize {
		to := from + *pageSize - 1
		if to > *hours-1 {
			to = *hours - 1
		}
		q := archive.ArchiveQuery{
			DeviceID:     *deviceID,
			ArchiveID:    "hourly",
			FromIndex:    from,
			ToIndex:      to, // Read() counts rows as ToIndex-FromIndex+1
			RecordLayout: hourlyRowLayout,
			WordOrder32:  akronOrder32,
			Params:       map[string]any{"archive_kind": "hourly"},
		}
		records, err := reader.Read(ctx, nil, txr, q)
		if err != nil {
			fmt.Printf("страница i=%d..%d: ошибка чтения: %v — останавливаюсь (дальше в архиве прибора, видимо, ничего нет)\n", from+1, to+1, err)
			break
		}
		if len(records) == 0 {
			fmt.Printf("страница i=%d..%d: пусто — архив прибора закончился\n", from+1, to+1)
			break
		}
		gotTotal += len(records)
		fmt.Printf("страница i=%d..%d: получено %d строк\n", from+1, to+1, len(records))

		for _, rec := range records {
			ts, tsOK := akronRowTime(rec.Fields)
			vol, volOK := fieldFloat(rec.Fields, "volume")
			if !tsOK || !volOK {
				skipped++
				continue
			}
			err := repo.SaveHourlyArchive(ctx, storage.HourlyArchiveRecord{
				DeviceID: *deviceID,
				Channel:  "",
				Param:    "V",
				TsHour:   ts,
				Value:    vol,
				Unit:     *unitLabel,
			})
			if err != nil {
				fmt.Printf("  строка %s: ошибка сохранения: %v\n", ts.Format("02.01.2006 15:00"), err)
				skipped++
				continue
			}
			saved++
		}
	}
	fmt.Printf("получено с прибора всего: %d строк\n", gotTotal)
	fmt.Printf("сохранено: %d, пропущено: %d\n", saved, skipped)
}

type akronTransactor struct {
	tr   transport.Transport
	unit uint8
}

func (a akronTransactor) Transact(ctx context.Context, req []byte) ([]byte, error) {
	return modbus.Transact(ctx, a.tr, false, 0, a.unit, req)
}

// akronRowTime mirrors internal/device/akron_hourly.go's akronRowTime and
// tools/akronread/main.go's copy of the same logic — kept in sync
// deliberately across all three call sites, including the future/past
// sanity bound (a corrupted-but-field-valid BCD decode once produced
// "07.06.2044", which then permanently shadowed real archive rows —
// see akron_hourly.go's doc comment for the full story).
const (
	akronMaxFutureSkew = 24 * time.Hour
	akronMaxPastSkew   = 400 * 24 * time.Hour
)

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
	ts := time.Date(2000+yy, time.Month(month), day, hour, 0, 0, 0, time.Local)

	now := time.Now()
	if ts.After(now.Add(akronMaxFutureSkew)) || ts.Before(now.Add(-akronMaxPastSkew)) {
		return time.Time{}, false
	}
	return ts, true
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

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "akronbackfill: %s: %v\n", what, err)
	os.Exit(1)
}
