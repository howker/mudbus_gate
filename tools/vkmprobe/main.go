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
	"bytes"
	"context"
	"flag"
	"fmt"
	"math"
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
	probeArchiveRegs := flag.Bool("probe-archive-regs", false, "diagnostic: instead of the normal flow, try READING (not writing — safe) the archive-request register block (7900-7914) via HR function 03, at a few nearby address offsets, to check whether the block exists at all on this firmware before the archive dance's WRITE attempt hits it.")
	probeArchiveRead := flag.Bool("probe-archive-read", false, "diagnostic: assumes a request is already 'ready' (run the normal flow first, or this reads whatever result — even someone else's — is currently cached). Bundles several checks for the 'read archive string' step failing: exact length-register value, a sweep of read quantities (to find the device's real per-request register cap), alternate data-start addresses, and a fresh status re-check.")
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

	if *probeArchiveRegs {
		probeArchiveRegisters(ctx, tr, isTCP, uint8(*unit))
		return
	}

	if *probeArchiveRead {
		probeArchiveReadStep(ctx, tr, isTCP, uint8(*unit))
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
	fmt.Println("\n--- диагностика: control-константа int32 (ожидаем 1234567890, рег.110) ---")

	spaces := []string{"HR", "IR"}
	addrs110 := []int{110, 109}
	orders32 := []string{"0123", "1032", "2301", "3210"}

	for _, space := range spaces {
		for _, addr := range addrs110 {
			funcName := map[string]string{"IR": "04 (Input Registers)", "HR": "03 (Holding Registers)"}[space]
			data, err := modbus.ReadPoint(ctx, tr, isTCP, unit, space, addr, "int32")
			if err != nil {
				fmt.Printf("%s, регистр %d, функция %s: ошибка: %v\n", space, addr, funcName, err)
				continue
			}
			fmt.Printf("%s, регистр %d, функция %s: сырые байты = % X\n", space, addr, funcName, data)
			for _, order := range orders32 {
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

	fmt.Println("\n--- диагностика: control-константа double (ожидаем 123.4567890123456, рег.114) ---")
	fmt.Println("(ожидаемые сырые байты в порядке 01234567: 40 5E DD 3C 07 FB 4C 93)")

	golden114 := []byte{0x40, 0x5E, 0xDD, 0x3C, 0x07, 0xFB, 0x4C, 0x93}
	orders64 := []string{"01234567", "10325476", "76543210"}
	addrs114 := []int{114, 113}

	for _, space := range spaces {
		for _, addr := range addrs114 {
			funcName := map[string]string{"IR": "04 (Input Registers)", "HR": "03 (Holding Registers)"}[space]
			data, err := modbus.ReadPoint(ctx, tr, isTCP, unit, space, addr, "double")
			if err != nil {
				fmt.Printf("%s, регистр %d, функция %s: ошибка: %v\n", space, addr, funcName, err)
				continue
			}
			exactMatch := ""
			if bytes.Equal(data, golden114) {
				exactMatch = "  <-- РОВНО совпадает с ожидаемыми сырыми байтами!"
			}
			fmt.Printf("%s, регистр %d, функция %s: сырые байты = % X%s\n", space, addr, funcName, data, exactMatch)
			for _, order := range orders64 {
				v, err := codec.DecodeFloat64(data, order)
				if err != nil {
					continue
				}
				mark := ""
				if math.Abs(v-123.4567890123456) <= 1e-9 {
					mark = "  <-- СОВПАДЕНИЕ с золотой константой!"
				}
				fmt.Printf("    как double, порядок байт %s: %v%s\n", order, v, mark)
			}
		}
	}

	fmt.Println("\nЕсли выше нигде нет пометки «СОВПАДЕНИЕ» — значит эта прошивка не отдаёт золотую")
	fmt.Println("константу так, как описано в CONTRACTS.md, ни в одном из проверенных вариантов.")
	fmt.Println("Сравни сырые байты double вручную с ожидаемыми (40 5E DD 3C 07 FB 4C 93) — если это")
	fmt.Println("такие же байты, но в другом порядке (не 4-байтовыми парами, а иначе перемешаны),")
	fmt.Println("порядок придётся добавить в internal/codec/codec.go:Reorder64 отдельным случаем.")
}

// probeArchiveRegisters is a one-shot diagnostic bundling several
// hypotheses for why startRequest's WRITE to register 7900
// (CONTRACTS.md §6.1 / registri_mbrrtu_vkm.pdf) fails with "illegal data
// address" — deliberately checking all of them in one run instead of
// iterating server round-trips one guess at a time:
//
//  1. Does the whole 7900-8002 block even exist on THIS firmware, in
//     EITHER register space (HR, which the doc implies, or IR)?
//  2. Does a plain, isolated WRITE to 7900 alone (the same mechanism
//     that already proved itself working for the auth registers 200/201
//     during session.Open()) succeed or fail, and what EXACTLY does the
//     device say?
//  3. If the write fails, does a READ of 7900 immediately after still
//     show a sane value (register exists, write specifically rejected)
//     or the same address error (register plain doesn't exist here)?
//  4. Sanity check against a block we KNOW works from the current-value
//     read (real-time clock 1800-1805HR) — confirms our general
//     register I/O isn't itself flaky, so a failure on 7900+ really is
//     specific to that block.
//  5. Firmware/serial identification (1807HR version, 1810HR serial) —
//     if this is a different firmware revision than CONTRACTS.md's
//     source documentation assumed, the archive register map could
//     simply differ for this unit.
func probeArchiveRegisters(ctx context.Context, tr transport.Transport, isTCP bool, unit uint8) {
	fmt.Println("\n=== диагностика архивного блока (7900-8002) — несколько гипотез разом ===")

	fmt.Println("\n--- 0) контрольная проверка: часы прибора (1800-1805 HR) — точно рабочий диапазон ---")
	clockNames := []string{"день", "месяц", "год", "часы", "минуты", "секунды"}
	for i, name := range clockNames {
		addr := 1800 + i
		readInt16(ctx, tr, isTCP, unit, "HR", addr, name)
	}

	fmt.Println("\n--- 0b) прошивка/серийник — вдруг это другая ревизия карты регистров ---")
	readInt16(ctx, tr, isTCP, unit, "HR", 1807, "номер версии ПО")
	if data, err := modbus.ReadPoint(ctx, tr, isTCP, unit, "HR", 1810, "int32"); err != nil {
		fmt.Printf("HR 1810 (серийный номер, int32)            : ошибка: %v\n", err)
	} else if v, err := codec.DecodeInt32(data, "0123"); err == nil {
		fmt.Printf("HR 1810 (серийный номер, int32)            : %d (сырые байты % X)\n", v, data)
	}

	fmt.Println("\n--- 1) блок 7900-7914 (запрос) и 8000-8002 (статус) — обе пространства ---")
	archAddrs := []struct {
		name string
		addr int
	}{
		{"7900 (id запроса)", 7900},
		{"7901 (труба)", 7901},
		{"7902 (нач.время,день)", 7902},
		{"7908 (кон.время,день)", 7908},
		{"7914 (опции)", 7914},
		{"8000 (статус)", 8000},
		{"8001 (эхо id)", 8001},
		{"8002 (длина результата)", 8002},
	}
	for _, a := range archAddrs {
		readInt16(ctx, tr, isTCP, unit, "HR", a.addr, a.name)
	}
	fmt.Println("  (то же самое, но через IR — вдруг пространство не HR):")
	for _, a := range archAddrs {
		readInt16(ctx, tr, isTCP, unit, "IR", a.addr, a.name)
	}

	fmt.Println("\n--- 2) изолированная запись+чтение рег.7900 (та же механика, что уже сработала для 200/201) ---")
	writeErr := writeSingleRegister(ctx, tr, isTCP, unit, 7900, 1)
	if writeErr != nil {
		fmt.Printf("ЗАПИСЬ HR 7900 = 1 (unit=%d) : ошибка: %v\n", unit, writeErr)
	} else {
		fmt.Printf("ЗАПИСЬ HR 7900 = 1 (unit=%d) : успех\n", unit)
	}
	fmt.Println("  чтение сразу после попытки записи (регистр существует, но запись отклонена, или тот же address error?):")
	readInt16(ctx, tr, isTCP, unit, "HR", 7900, "7900 после попытки записи")

	fmt.Println("\n--- 3) запись НЕСКОЛЬКИХ регистров разом (функция 16) — так пишутся поля времени 7902-7913 ---")
	fmt.Println("(7900/7901/7914 пишутся по одному регистру функцией 06 — она уже подтверждена в пункте 2)")
	now := time.Now()
	timeRegs := []uint16{
		uint16(now.Day()), uint16(now.Month()), uint16(now.Year()),
		uint16(now.Hour()), uint16(now.Minute()), uint16(now.Second()),
	}
	if err := writeMultipleRegisters(ctx, tr, isTCP, unit, 7902, timeRegs); err != nil {
		fmt.Printf("ЗАПИСЬ HR 7902 (6 рег., функция 16) = %v : ошибка: %v\n", timeRegs, err)
	} else {
		fmt.Printf("ЗАПИСЬ HR 7902 (6 рег., функция 16) = %v : успех\n", timeRegs)
	}
	fmt.Println("  чтение всех 6 регистров сразу после записи:")
	for i, label := range []string{"день", "месяц", "год", "часы", "минуты", "секунды"} {
		readInt16(ctx, tr, isTCP, unit, "HR", 7902+i, "7902+"+fmt.Sprint(i)+" ("+label+")")
	}

	// Deliberately LAST: this alt-unit-address test previously caused a
	// transport-level "not open" error (the device likely just doesn't
	// answer a "wrong" unit at all on this TCP connection, which our
	// transport treats as needing a fresh Open) — not a meaningful
	// modbus-level answer, and risks leaving the connection in a state
	// that would contaminate any diagnostic run AFTER it. Kept only for
	// completeness; the earlier run's result already argues against this
	// hypothesis mattering here (register reads/writes work fine at the
	// device's real unit id regardless).
	fmt.Println("\n--- 4) (для полноты, менее вероятно) архивный блок на СВОЁМ unit-адресе для TCP-клиентов ---")
	fmt.Println("(registri_mbrrtu_vkm.pdf: 'Протоколу Modbus/TCP выделено 17 адресов (0-16)'; предыдущий")
	fmt.Println(" прогон уже дал транспортную ошибку, а не осмысленный ответ прибора — маловероятная версия)")
	for _, altUnit := range []uint8{0, 1, unit + 1} {
		if altUnit == unit {
			continue
		}
		err := writeSingleRegister(ctx, tr, isTCP, altUnit, 7900, 1)
		if err != nil {
			fmt.Printf("ЗАПИСЬ HR 7900 = 1 (unit=%d) : ошибка: %v\n", altUnit, err)
		} else {
			fmt.Printf("ЗАПИСЬ HR 7900 = 1 (unit=%d) : успех  <-- ЕСЛИ ВИДИШЬ ЭТО, ДЕЛО В UNIT-АДРЕСЕ\n", altUnit)
		}
	}

	fmt.Println("\n=== ИТОГ ===")
	fmt.Println("если 1)/2) прошли, а 3) провалилась — функция 16 (запись нескольких регистров) не")
	fmt.Println("работает на этом приборе/подключении, хотя функция 06 (один регистр) работает. Тогда")
	fmt.Println("правим startRequest — писать все 15 регистров 7900-7914 ПООДИНОЧНЕ функцией 06.")
	fmt.Println("если 1)/2)/3) все прошли — значит и запись работает полностью, и запрос архива должен")
	fmt.Println("собираться штатно; тогда проблема в другом шаге (waitReady/чтение результата) — пришли")
	fmt.Println("вывод обычного (не диагностического) запуска, разберём его отдельно.")
}

// readInt16 reads one HR/IR int16 register and prints the result or error,
// used by several probe helpers to keep output format consistent.
func readInt16(ctx context.Context, tr transport.Transport, isTCP bool, unit uint8, space string, addr int, label string) {
	data, err := modbus.ReadPoint(ctx, tr, isTCP, unit, space, addr, "int16")
	if err != nil {
		fmt.Printf("%s %-30s : ошибка: %v\n", space, label, err)
		return
	}
	v, err := codec.DecodeInt16(data)
	if err != nil {
		fmt.Printf("%s %-30s : сырые байты = % X (не int16: %v)\n", space, label, data, err)
		return
	}
	fmt.Printf("%s %-30s : значение = %-8d (сырые байты % X)\n", space, label, v, data)
}

// writeSingleRegister writes one HR register via Modbus function 06.
func writeSingleRegister(ctx context.Context, tr transport.Transport, isTCP bool, unit uint8, addr int, value uint16) error {
	reqPDU := modbus.BuildWriteSingleRegisterPDU(addr, value)
	_, err := modbus.Transact(ctx, tr, isTCP, nextProbeTxID(), unit, reqPDU)
	return err
}

// writeMultipleRegisters writes a block of HR registers via Modbus
// function 16 — the mechanism startRequest uses for the 6-register time
// fields (7902-7907, 7908-7913), as opposed to writeSingleRegister's
// function 06 used for the single-value fields (7900, 7901, 7914).
func writeMultipleRegisters(ctx context.Context, tr transport.Transport, isTCP bool, unit uint8, addr int, values []uint16) error {
	reqPDU, err := modbus.BuildWriteMultipleRegistersPDU(addr, values)
	if err != nil {
		return err
	}
	_, err = modbus.Transact(ctx, tr, isTCP, nextProbeTxID(), unit, reqPDU)
	return err
}

var probeTxID uint16

func nextProbeTxID() uint16 {
	probeTxID++
	return probeTxID
}

// probeArchiveReadStep bundles several hypotheses for why readResultString's
// final read (the data block at 8003+) fails with Modbus exception 0x03
// (illegal data VALUE, as opposed to 0x02 illegal ADDRESS — the register
// itself is accepted, something about the request's shape isn't):
//
//  1. Exact current length-register (8002) value — the source of truth for
//     how many registers readResultString computes and requests.
//  2. A sweep of read quantities (1, 2, 5, 10, 20, 50, 100, and the exact
//     computed value) at the data-start address, to find whether the
//     device caps how many registers it will return in one read (some
//     devices cap well below the Modbus spec's 125-register ceiling).
//  3. Alternate data-start addresses (8003, 8002, 8004) in case of an
//     off-by-one in the documented base.
//  4. A fresh status (8000) re-check — the result-cache window is only
//     ~300s per the doc, so if this runs a while after the last real
//     request, "ready" may have already reverted to "no records"/expired,
//     which would explain a value error on the FOLLOWING read differently
//     than a genuine quantity/address problem.
func probeArchiveReadStep(ctx context.Context, tr transport.Transport, isTCP bool, unit uint8) {
	fmt.Println("\n=== диагностика шага чтения результата (8002 длина, 8003+ данные) ===")

	fmt.Println("\n--- 0) статус сейчас (окно кэша результата ~300с — мог уже истечь) ---")
	if data, err := modbus.ReadPoint(ctx, tr, isTCP, unit, "HR", 8000, "int16"); err != nil {
		fmt.Printf("HR статус (8000) : ошибка: %v\n", err)
	} else {
		v, _ := codec.DecodeInt16(data)
		labels := map[int16]string{0: "expired (истёк)", 1: "collecting (собирается)", 2: "ready (готово)", 3: "no records (нет записей)", 4: "bad start", 5: "bad end", 6: "bad pipe", 7: "too large"}
		label, ok := labels[v]
		if !ok {
			label = "неизвестно"
		}
		fmt.Printf("HR статус (8000) : значение = %d (%s)\n", v, label)
	}

	fmt.Println("\n--- 1) точное значение длины (8002) ---")
	lenData, lenErr := modbus.ReadPoint(ctx, tr, isTCP, unit, "HR", 8002, "int16")
	var strLen int
	if lenErr != nil {
		fmt.Printf("HR 8002 (длина) : ошибка: %v\n", lenErr)
	} else {
		v, _ := codec.DecodeInt16(lenData)
		strLen = int(v)
		neededRegs := (strLen + 1) / 2
		fmt.Printf("HR 8002 (длина) : значение = %d символов -> расчётно нужно %d регистров\n", strLen, neededRegs)
	}

	fmt.Println("\n--- 2) перебор количества регистров за одно чтение начиная с 8003 ---")
	qtys := []int{1, 2, 5, 10, 20, 50, 100}
	if strLen > 0 {
		needed := (strLen + 1) / 2
		already := false
		for _, q := range qtys {
			if q == needed {
				already = true
			}
		}
		if !already && needed > 0 {
			qtys = append(qtys, needed)
		}
	}
	for _, qty := range qtys {
		pdu, err := modbus.BuildReadPDUWithQty("HR", 8003, uint16(qty))
		if err != nil {
			fmt.Printf("чтение 8003, qty=%-4d : ошибка сборки PDU: %v\n", qty, err)
			continue
		}
		resp, err := modbus.Transact(ctx, tr, isTCP, nextProbeTxID(), unit, pdu)
		if err != nil {
			fmt.Printf("чтение 8003, qty=%-4d : ошибка: %v\n", qty, err)
			continue
		}
		fmt.Printf("чтение 8003, qty=%-4d : успех, получено байт данных = %d\n", qty, len(resp)-2)
	}

	fmt.Println("\n--- 3) альтернативные адреса начала данных (вдруг не 8003) ---")
	for _, addr := range []int{8002, 8003, 8004} {
		pdu, _ := modbus.BuildReadPDUWithQty("HR", addr, 5)
		resp, err := modbus.Transact(ctx, tr, isTCP, nextProbeTxID(), unit, pdu)
		if err != nil {
			fmt.Printf("чтение с адреса %d, qty=5 : ошибка: %v\n", addr, err)
			continue
		}
		fmt.Printf("чтение с адреса %d, qty=5 : успех, сырые байты = % X\n", addr, resp[2:])
	}

	fmt.Println("\n=== ИТОГ ===")
	fmt.Println("если пункт 2) показывает успех для МАЛЫХ qty, но ошибку для большего — найден реальный")
	fmt.Println("потолок регистров за одно чтение у этого прибора; тогда readResultString нужно читать")
	fmt.Println("результат несколькими чтениями по кругу, а не одним большим запросом.")
	fmt.Println("если 2) провалилась ВЕЗДЕ, а 3) на каком-то адресе — успех, значит адрес данных другой.")
	fmt.Println("если статус в 0) уже НЕ 'готово' — окно кэша истекло, и это просто гонка по времени,")
	fmt.Println("а не баг: нужно читать данные быстрее после готовности статуса.")
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
