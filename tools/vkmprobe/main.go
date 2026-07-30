// Command vkmprobe is a standalone southbound reader for a REAL
// ЭЛЕМЕР-ВКМ-360 flow computer: it opens a connection, reads a handful of
// current-value points as a proof of life, then exercises the full
// mb_request_poll_string archive dance (write request registers 7900-7914
// -> poll status 8000 -> read length 8002 -> read the tagged ANSI string
// at 8003+) against the real device and prints BOTH the raw string and
// what our parser makes of it.
//
// This is the client counterpart to tools/akronread, for the one thing
// M4's VKM work has never been able to verify: the real device's archive
// string format. Everything else (northbound serving, config/status
// registers, the parser itself) was built and tested against invented
// data per profiles/vkm360.yaml + CONTRACTS.md §6.1 — vkmprobe is what
// confirms or corrects that against actual bytes.
//
// It writes nothing to the database — read-only proof of life plus a
// diagnostic dump. Wiring real VKM readings into the normal poll cycle
// (device.PollArchives) is the next step once the string format is
// confirmed, mirroring how akronread's findings fed into
// internal/device/akron_hourly.go.
//
// Usage (TCP):
//
//	vkmprobe --host 10.48.228.60 --port 502 --unit 1 --pipe 1 \
//	  --minutes-back 60
//
// Usage (COM port, e.g. a converter-emulated port):
//
//	vkmprobe --com COM106 --baud 9600 --unit 1 --pipe 1 --minutes-back 60
//
// --minutes-back sets the archive request window to [now-N, now]; use
// --from/--to (RFC3339) instead for a specific historical window.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"mbgw/internal/archive"
	"mbgw/internal/codec"
	"mbgw/internal/pollcore"
	"mbgw/internal/protocol/modbus"
	"mbgw/internal/session"
	"mbgw/internal/transport"
)

func main() {
	host := flag.String("host", "", "device IP/hostname (TCP mode)")
	port := flag.Int("port", 0, "device TCP port (TCP mode)")
	com := flag.String("com", "", "COM port, e.g. COM106 (serial mode)")
	baud := flag.Int("baud", 9600, "baud rate (serial mode)")
	parity := flag.String("parity", "none", "parity (serial mode): none|even|odd")
	stopbits := flag.Int("stopbits", 1, "stop bits (serial mode): 1|2")
	unit := flag.Int("unit", 1, "device bus address (Modbus unit id)")
	tcpMBAP := flag.Bool("tcp-mbap", false, "TCP mode framing: true = real Modbus TCP/MBAP (a proper Modbus TCP gateway); false (default) = raw RTU framing over a TCP socket (an Ethernet/RS-485 converter that does NOT speak MBAP — the more common setup in this project, e.g. our real Акрон-01 on COM105/RawTCP). Ignored in --com (serial) mode, which is always RTU framing.")
	timeout := flag.Duration("timeout", time.Second, "response timeout")
	retries := flag.Int("retries", 3, "transport retries")
	pipe := flag.Int("pipe", 1, "pipe/line number for archive request and the 'Массовый расход' point's addr_formula (profiles/vkm360.yaml: instances.pipe, count=2)")
	minutesBack := flag.Int("minutes-back", 60, "archive request window: [now-N minutes, now] (ignored if --from/--to given)")
	fromStr := flag.String("from", "", "archive window start, RFC3339 (overrides --minutes-back)")
	toStr := flag.String("to", "", "archive window end, RFC3339 (overrides --minutes-back)")
	skipCurrent := flag.Bool("skip-current", false, "skip the current-value proof-of-life reads, go straight to the archive dance")
	skipArchive := flag.Bool("skip-archive", false, "skip the archive request, only read current values")
	skipSession := flag.Bool("skip-session", false, "skip session.Open() (byte-order detect + auth) entirely and go straight to current-value/archive reads. Use this if the byte-order handshake fails (e.g. modbus exception on register 110) — many real Modbus devices allow plain register reads with no handshake at all; this tells you whether that's the case here.")
	probeControlRegs := flag.Bool("probe-control-regs", false, "diagnostic: instead of the normal flow, try reading the CONTRACTS.md §byte-order control constant (expected int32=1234567890 at register 110) across HR/IR spaces and 109/110 addressing, and print the raw results. Use this when the handshake's register-110 read fails with an address error, to find out where the value actually lives on THIS device.")
	flag.Parse()

	var params transport.Params
	var isTCP bool // true = MBAP framing (function code + address/qty, no CRC); false = RTU framing (CRC, no MBAP header) — must match the transport.Kind chosen below, not just "is it TCP".
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
		isTCP = false
	case *host != "" && *port != 0 && *tcpMBAP:
		params = transport.Params{
			Kind:            transport.KindModbusTCP,
			Host:            *host,
			Port:            *port,
			ResponseTimeout: *timeout,
			Retries:         *retries,
		}
		isTCP = true
	case *host != "" && *port != 0:
		params = transport.Params{
			Kind:            transport.KindTCPSerial, // raw RTU framing over a TCP socket
			Host:            *host,
			Port:            *port,
			ResponseTimeout: *timeout,
			Retries:         *retries,
		}
		isTCP = false
	default:
		fmt.Fprintln(os.Stderr, "vkmprobe: specify either --com <port> (serial mode) or --host/--port (TCP mode; add --tcp-mbap for real Modbus TCP)")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tr, err := transport.New(params)
	if err != nil {
		fatal("создание транспорта", err)
	}
	if err := tr.Open(ctx); err != nil {
		fatal("подключение к прибору", err)
	}
	defer tr.Close()
	fmt.Println("подключение открыто")

	if *probeControlRegs {
		probeControlRegisters(ctx, tr, isTCP, uint8(*unit))
		return
	}

	// VKM's profile declares session type "modbus_byteorder_auth" —
	// unlike Akron (session "none"), this device needs a real handshake
	// (byte-order detection + authorization) before register access
	// works. Must run before either the current-value reads or the
	// archive dance below, on this same transport.
	if !*skipSession {
		sess, err := session.New("modbus_byteorder_auth")
		if err != nil {
			fatal("создание сессии", err)
		}
		if err := sess.Open(ctx, tr); err != nil {
			fmt.Fprintf(os.Stderr, "vkmprobe: открытие сессии (byte-order/авторизация): %v\n", err)
			fmt.Fprintln(os.Stderr, "  прибор не принял control-константу — попробуй:")
			fmt.Fprintln(os.Stderr, "  1) --probe-control-regs  чтобы найти, где она реально лежит (HR/IR, адрес 109/110)")
			fmt.Fprintln(os.Stderr, "  2) --skip-session        чтобы пропустить рукопожатие и проверить, читаются ли точки вообще без него")
			os.Exit(1)
		}
		fmt.Println("сессия открыта (byte-order обнаружен, авторизация пройдена)")
	} else {
		fmt.Println("сессия пропущена (--skip-session) — идём сразу к чтению точек")
	}

	reader := pollcore.New(tr, isTCP, uint8(*unit))

	if !*skipCurrent {
		readCurrentValues(ctx, reader, *pipe)
	}

	if !*skipArchive {
		from, to, err := archiveWindow(*minutesBack, *fromStr, *toStr)
		if err != nil {
			fatal("разбор периода архива", err)
		}
		readArchive(ctx, reader, *pipe, from, to)
	}
}

// readCurrentValues reads the profile's "current" group points for the
// given pipe as a quick proof of life, mirroring what akronread does for
// Акрон's 102 command — just via plain Modbus IR reads here instead of a
// vendor command.
func readCurrentValues(ctx context.Context, r *pollcore.Reader, pipe int) {
	fmt.Println("\n--- текущие значения ---")

	type point struct {
		name string
		addr int
		typ  string
	}
	// addr_formula "2000+(pipe-1)*100+8" from profiles/vkm360.yaml,
	// evaluated here for the requested pipe.
	massFlowAddr := 2000 + (pipe-1)*100 + 8
	points := []point{
		{"Массовый расход", massFlowAddr, "float"},
		{"Избыточное давление", 2000, "float"},
		{"Температура", 2004, "float"},
		{"Слово статуса лог.входа", 4002, "bitfield"},
	}

	for _, pt := range points {
		data, err := r.ReadRaw(ctx, "IR", pt.addr, pt.typ)
		if err != nil {
			fmt.Printf("%s (IR %d): ошибка чтения: %v\n", pt.name, pt.addr, err)
			continue
		}
		if pt.typ == "bitfield" {
			v, err := codec.DecodeUint16(data)
			if err != nil {
				fmt.Printf("%s (IR %d): ошибка декодирования: %v\n", pt.name, pt.addr, err)
				continue
			}
			fmt.Printf("%s (IR %d) = 0x%04X (бит6 sensor_break=%v, бит7 sensor_short=%v, бит9 no_modbus_link=%v)\n",
				pt.name, pt.addr, v, v&(1<<6) != 0, v&(1<<7) != 0, v&(1<<9) != 0)
			continue
		}
		v, err := codec.DecodeFloat32(data, "0123")
		if err != nil {
			fmt.Printf("%s (IR %d): ошибка декодирования float: %v\n", pt.name, pt.addr, err)
			continue
		}
		fmt.Printf("%s (IR %d) = %v\n", pt.name, pt.addr, v)
	}
}

// readArchive exercises the full request/poll/read dance for one archive
// window and dumps the raw string plus the parsed field map. This is the
// whole point of the tool — everything above is just proof of life.
func readArchive(ctx context.Context, r *pollcore.Reader, pipe int, from, to time.Time) {
	fmt.Printf("\n--- архив: пробуем период %s .. %s (труба %d) ---\n",
		from.Format(time.RFC3339), to.Format(time.RFC3339), pipe)

	strategy, ok := archive.Get("mb_request_poll_string")
	if !ok {
		fatal("архив", fmt.Errorf("стратегия mb_request_poll_string не зарегистрирована"))
	}

	// NOTE: internal/archive/mb_request_poll_string.go currently reads its
	// register addresses (7900, 8000, 8002, 8003...) from package-level
	// constants, not from q.Params — they happen to match
	// profiles/vkm360.yaml's archives[0].params block, but that block is
	// not actually consumed by this strategy today. Not fixed here (out
	// of scope for a probe tool); flagging so it doesn't look like an
	// oversight if the two ever drift.
	q := archive.ArchiveQuery{
		DeviceID:  "vkm_probe",
		ArchiveID: "main",
		Instance:  pipe,
		From:      from,
		To:        to,
	}

	records, err := strategy.Read(ctx, nil, r, q)
	if err != nil {
		fmt.Printf("ошибка запроса архива: %v\n", err)
		fmt.Println("(если статус ушёл в 'no records' — попробуй другой период, например --minutes-back 1440")
		fmt.Println(" для суток назад, раз архив хранится не бесконечно; если статус 'expired'/'bad start'/")
		fmt.Println(" 'bad end' — стоит свериться с CONTRACTS.md §6.1 и часовым поясом прибора)")
		return
	}

	if len(records) == 0 {
		fmt.Println("прибор вернул пустой результат (0 записей) — период, вероятно, без данных")
		return
	}

	rec := records[0]
	fmt.Printf("\nСЫРАЯ СТРОКА ОТ ПРИБОРА (%d байт):\n%s\n", len(rec.Raw), string(rec.Raw))

	fmt.Println("\nКАК ЭТО РАЗОБРАЛ НАШ ПАРСЕР (parseTaggedString):")
	if len(rec.Fields) == 0 {
		fmt.Println("  (ничего — парсер не нашёл ни одного тег=значение; сверь сырую строку выше с ожидаемым")
		fmt.Println("   форматом 'тег{шапка}=значение ед.изм;...' из CONTRACTS.md §6.1 — вероятно, реальный")
		fmt.Println("   формат отличается, и парсер надо будет подправить под то, что реально пришло)")
		return
	}
	for k, v := range rec.Fields {
		fmt.Printf("  %s = %v\n", k, v)
	}
}

// probeControlRegisters is a diagnostic for when the normal byte-order
// handshake (internal/session/modbus_byteorder_auth.go) fails to read
// register 110 (expected int32 golden constant 1234567890, per
// CONTRACTS.md's documented VKM byte-order-detect contract). The handshake
// hardcodes IR (function 04) and register 110 as given in the contract
// doc; a real device answering "illegal data address" there means EITHER
// the constant lives in a different register space (HR, function 03)
// OR the document's register numbering is 1-based and the real zero-based
// address is 109, OR this particular firmware simply doesn't implement
// the self-test registers at all. This tries every combination and prints
// raw bytes plus a few byte-order decodes, so the answer comes from the
// device instead of another guess.
func probeControlRegisters(ctx context.Context, tr transport.Transport, isTCP bool, unit uint8) {
	fmt.Println("\n--- диагностика: control-константа (ожидаем int32=1234567890 где-то рядом с рег.110) ---")

	spaces := []string{"IR", "HR"}
	addrs := []int{110, 109}
	orders := []string{"0123", "1032", "2301", "3210"}

	for _, space := range spaces {
		for _, addr := range addrs {
			funcName := map[string]string{"IR": "04 (Input Registers)", "HR": "03 (Holding Registers)"}[space]
			data, err := modbus.ReadPoint(ctx, tr, isTCP, unit, space, addr, "int32")
			if err != nil {
				fmt.Printf("%s, регистр %d, функция %s: ошибка: %v\n", space, addr, funcName, err)
				continue
			}
			fmt.Printf("%s, регистр %d, функция %s: сырые байты = % X\n", space, addr, funcName, data)
			for _, order := range orders {
				v, err := codec.DecodeInt32(data, order)
				if err != nil {
					continue
				}
				mark := ""
				if v == 1234567890 {
					mark = "  <-- СОВПАДЕНИЕ с золотой константой!"
				}
				fmt.Printf("    как int32, порядок байт %s: %d%s\n", order, v, mark)
			}
		}
	}
	fmt.Println("\nЕсли выше нигде нет пометки «СОВПАДЕНИЕ» — значит эта прошивка не отдаёт золотую")
	fmt.Println("константу так, как описано в CONTRACTS.md, ни в одном из проверенных вариантов.")
	fmt.Println("Тогда byte-order-детект придётся либо перепроверить по актуальной документации")
	fmt.Println("на конкретно эту прошивку ВКМ-360, либо задавать порядок байт в конфиге вручную,")
	fmt.Println("а не автоопределением через эту самопроверку.")
}

func archiveWindow(minutesBack int, fromStr, toStr string) (time.Time, time.Time, error) {
	if fromStr != "" || toStr != "" {
		from, err := time.Parse(time.RFC3339, fromStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--from: %w", err)
		}
		to, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--to: %w", err)
		}
		return from, to, nil
	}
	now := time.Now()
	return now.Add(-time.Duration(minutesBack) * time.Minute), now, nil
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "vkmprobe: %s: %v\n", what, err)
	os.Exit(1)
}
