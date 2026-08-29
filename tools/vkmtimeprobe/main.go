// Command vkmtimeprobe — узкий, ТОЛЬКО ЧИТАЮЩИЙ диагностический
// инструмент: проверяет, отвечает ли РЕАЛЬНЫЙ прибор (ВКМ-360, опрашиваемый
// по протоколу, совместимому с УВП-280.01) на блок регистров NTP/коррекции
// времени, задокументированный в modbus_uvp280_01.pdf (стр. 6):
//
//	1005HR, uint32  IP адрес NTP-сервера эталонного времени
//	1007HR, int16   статус синхронизации (при чтении) / команда управления (при записи)
//	1008HR, int16   разница в секундах между временем прибора и NTP (read-only)
//	1009HR, int16   перевод текущего времени на ±99с (назначение регистра при ЧТЕНИИ
//	                документацией не описано отдельно — читаем из любопытства, ничего
//	                не значащее значение здесь совершенно ожидаемо)
//	1800-1805HR     текущее время прибора (день/месяц/год/часы/минуты/секунды) —
//	                этот блок уже подтверждён рабочим в документации ВКМ-360
//	                (registri_mbrrtu_vkm.pdf) и используется здесь как контрольная
//	                точка: если ОН тоже не отвечает — дело не в блоке 1005-1009,
//	                а в самом подключении/unit-адресе.
//
// НИЧЕГО НЕ ПИШЕТ В ПРИБОР — используется исключительно Modbus-функция 03
// (чтение holding-регистров). Инструмент нужен, чтобы БЕЗОПАСНО проверить,
// прежде чем проектировать реальную функцию коррекции времени в mbgw,
// отвечает ли конкретный физический прибор на этот блок вообще — карта
// регистров ВКМ-360 (registri_mbrrtu_vkm.pdf) описывает часы 1800-1805 как
// "только чтение" и не документирует блок 1005-1009 совсем, но ЭС успешно
// корректирует время того же прибора через драйвер УВП280А, чья
// документация (modbus_uvp280_01.pdf) этот блок как раз описывает — то есть
// физическая прошивка прибора вполне может отвечать на оба набора адресов
// одновременно, вопрос в том, отвечает ли КОНКРЕТНО ваш экземпляр.
//
// Usage (TCP, raw RTU framing over socket — как у большинства приборов
// в этом проекте):
//
//	vkmtimeprobe --host 10.48.228.126 --port 502 --unit 2
//
// Usage (Modbus TCP/MBAP):
//
//	vkmtimeprobe --host 10.48.228.126 --port 502 --unit 2 --tcp-mbap
//
// Usage (COM-порт):
//
//	vkmtimeprobe --com COM106 --baud 9600 --unit 1
//
// --skip-session пропускает открытие сессии (byte-order handshake +
// авторизация) — используйте, если рукопожатие не проходит на этом
// приборе; чтение простых int16-регистров часто работает и без него.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"mbgw/internal/codec"
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
	tcpMBAP := flag.Bool("tcp-mbap", false, "TCP mode framing: true = real Modbus TCP/MBAP; false (default) = raw RTU framing over a TCP socket (более частый случай в этом проекте)")
	timeout := flag.Duration("timeout", time.Second, "response timeout")
	retries := flag.Int("retries", 3, "transport retries")
	skipSession := flag.Bool("skip-session", false, "пропустить открытие сессии (byte-order handshake + авторизация) и читать регистры напрямую")
	flag.Parse()

	var params transport.Params
	var isTCP bool
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
			Kind:            transport.KindTCPSerial,
			Host:            *host,
			Port:            *port,
			ResponseTimeout: *timeout,
			Retries:         *retries,
		}
		isTCP = false
	default:
		fmt.Fprintln(os.Stderr, "vkmtimeprobe: specify either --com <port> (serial mode) or --host/--port (TCP mode; add --tcp-mbap for real Modbus TCP)")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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

	if !*skipSession {
		sess, err := session.New("modbus_byteorder_auth")
		if err != nil {
			fatal("создание сессии", err)
		}
		if err := sess.Open(ctx, tr); err != nil {
			fmt.Fprintf(os.Stderr, "vkmtimeprobe: открытие сессии (byte-order/авторизация): %v\n", err)
			fmt.Fprintln(os.Stderr, "  попробуйте --skip-session, чтобы читать регистры напрямую без рукопожатия")
			os.Exit(1)
		}
		fmt.Println("сессия открыта (byte-order обнаружен, авторизация пройдена)")
	} else {
		fmt.Println("сессия пропущена (--skip-session) — идём сразу к чтению регистров")
	}

	u := uint8(*unit)

	fmt.Println("\n=== 0) контрольная точка: часы прибора (1800-1805 HR) — уже подтверждённый рабочий блок ===")
	clockNames := []string{"день", "месяц", "год", "часы", "минуты", "секунды"}
	clockOK := true
	for i, name := range clockNames {
		if _, err := readInt16(ctx, tr, isTCP, u, 1800+i, name); err != nil {
			clockOK = false
		}
	}
	if !clockOK {
		fmt.Println("\nВНИМАНИЕ: даже контрольный блок часов (1800-1805) не отвечает — проблема, скорее")
		fmt.Println("всего, в самом подключении/unit-адресе, а не в блоке 1005-1009 ниже. Дальнейшие")
		fmt.Println("результаты стоит трактовать с осторожностью.")
	}

	fmt.Println("\n=== 1) блок NTP/коррекции времени (1005, 1007, 1008, 1009 HR) — по modbus_uvp280_01.pdf ===")

	// 1005HR, uint32 — IP адрес NTP-сервера, старшим байтом вперёд (0123).
	if data, err := modbus.ReadPoint(ctx, tr, isTCP, u, "HR", 1005, "uint32"); err != nil {
		fmt.Printf("HR 1005 (IP NTP-сервера)          : ошибка: %v\n", err)
	} else if v, err := codec.DecodeUint32(data, "0123"); err != nil {
		fmt.Printf("HR 1005 (IP NTP-сервера)          : сырые байты = % X (не разобралось как uint32: %v)\n", data, err)
	} else {
		fmt.Printf("HR 1005 (IP NTP-сервера)          : %d.%d.%d.%d (сырое значение %d, байты % X)\n",
			byte(v>>24), byte(v>>16), byte(v>>8), byte(v), v, data)
	}

	// 1007HR, int16 — при чтении: биты статуса синхронизации.
	if v, err := readInt16(ctx, tr, isTCP, u, 1007, "статус синхронизации"); err == nil {
		fmt.Printf("  расшифровка: бит0 (связь с NTP)=%v, бит1 (точное время <30с назад)=%v, бит2 (авто-синхронизация запущена)=%v\n",
			v&1 != 0, v&2 != 0, v&4 != 0)
	}

	// 1008HR, int16 — read-only, разница в секундах.
	readInt16(ctx, tr, isTCP, u, 1008, "разница с NTP, сек (+ = часы прибора спешат)")

	// 1009HR, int16 — по документации это регистр ЗАПИСИ команды сдвига;
	// назначение при чтении не описано отдельно — читаем на всякий случай,
	// осмысленного значения может не быть, это не ошибка инструмента.
	readInt16(ctx, tr, isTCP, u, 1009, "регистр сдвига (1009, значение при чтении не документировано)")

	fmt.Println("\n=== ИТОГ ===")
	fmt.Println("Если 1800-1805 читаются, а 1005/1007/1008 отвечают ошибкой Modbus (ILLEGAL DATA")
	fmt.Println("ADDRESS) — этот блок на данном приборе не реализован, коррекцию времени через него")
	fmt.Println("делать нельзя.")
	fmt.Println("Если 1005/1007/1008 отвечают осмысленными числами (не мусором) — блок реализован,")
	fmt.Println("можно проектировать функцию коррекции в mbgw на основе регистра 1009 (запись сдвига)")
	fmt.Println("и 1008 (проверка расхождения перед записью).")
}

// readInt16 reads one HR int16 register and prints the result or error;
// returns the decoded value (0 on error) and the error, so callers can
// branch on success without re-reading.
func readInt16(ctx context.Context, tr transport.Transport, isTCP bool, unit uint8, addr int, label string) (int16, error) {
	data, err := modbus.ReadPoint(ctx, tr, isTCP, unit, "HR", addr, "int16")
	if err != nil {
		fmt.Printf("HR %-4d (%s) : ошибка: %v\n", addr, label, err)
		return 0, err
	}
	v, err := codec.DecodeInt16(data)
	if err != nil {
		fmt.Printf("HR %-4d (%s) : сырые байты = % X (не int16: %v)\n", addr, label, data, err)
		return 0, err
	}
	fmt.Printf("HR %-4d (%s) : значение = %d (сырые байты % X)\n", addr, label, v, data)
	return v, nil
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "vkmtimeprobe: %s: %v\n", what, err)
	os.Exit(1)
}
