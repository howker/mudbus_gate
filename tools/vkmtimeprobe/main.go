package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
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
	tcpMBAP := flag.Bool("tcp-mbap", false, "TCP mode framing: true = Modbus TCP/MBAP; false = raw RTU over TCP")
	timeout := flag.Duration("timeout", time.Second, "response timeout")
	retries := flag.Int("retries", 3, "transport retries")
	skipSession := flag.Bool("skip-session", false, "skip byte-order/auth session handshake")
	correctTime := flag.Bool("correct-time", false, "correct VKM clock via HR1009 if absolute drift is 1..99 seconds")
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
		fmt.Fprintln(os.Stderr, "vkmtimeprobe: specify either --com <port> or --host/--port (add --tcp-mbap for Modbus TCP)")
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
			fmt.Fprintf(os.Stderr, "vkmtimeprobe: открытие сессии: %v\n", err)
			fmt.Fprintln(os.Stderr, "  попробуйте --skip-session")
			os.Exit(1)
		}
		fmt.Println("сессия открыта (byte-order/авторизация пройдены)")
	} else {
		fmt.Println("сессия пропущена (--skip-session)")
	}

	u := uint8(*unit)

	fmt.Println("\n=== Часы ВКМ: 1800-1805 HR ===")
	deviceTime, midpoint, err := readVKMClock(ctx, tr, isTCP, u)
	if err != nil {
		fmt.Printf("ошибка чтения часов: %v\n", err)
	} else {
		printClockComparison(deviceTime, midpoint)
	}

	fmt.Println("\n=== Блок 1005/1007/1008/1009 HR ===")
	if data, err := modbus.ReadPoint(ctx, tr, isTCP, u, "HR", 1005, "uint32"); err != nil {
		fmt.Printf("HR 1005 (IP NTP-сервера): ошибка: %v\n", err)
	} else if v, err := codec.DecodeUint32(data, "0123"); err != nil {
		fmt.Printf("HR 1005 (IP NTP-сервера): сырые байты = % X, decode error: %v\n", data, err)
	} else {
		fmt.Printf("HR 1005 (IP NTP-сервера): %d.%d.%d.%d\n", byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
	readInt16(ctx, tr, isTCP, u, 1007, "статус синхронизации")
	readInt16(ctx, tr, isTCP, u, 1008, "разница с NTP, сек")
	readInt16(ctx, tr, isTCP, u, 1009, "регистр коррекции времени")

	if !*correctTime {
		fmt.Println("\nРежим только чтения. Для живой проверки коррекции добавьте --correct-time.")
		return
	}

	fmt.Println("\n=== КОРРЕКЦИЯ ВРЕМЕНИ ЧЕРЕЗ HR1009 ===")
	deviceTime, midpoint, err = readVKMClock(ctx, tr, isTCP, u)
	if err != nil {
		fatal("повторное чтение часов перед коррекцией", err)
	}

	drift := midpoint.Sub(deviceTime).Seconds() // positive => device is behind server
	seconds := int(math.Round(drift))
	fmt.Printf("сервер: %s\n", midpoint.Format("02.01.2006 15:04:05"))
	fmt.Printf("ВКМ:    %s\n", deviceTime.Format("02.01.2006 15:04:05"))
	fmt.Printf("требуемая коррекция: %+d сек\n", seconds)

	if seconds == 0 {
		fmt.Println("коррекция не нужна")
		return
	}
	if seconds < -99 || seconds > 99 {
		fmt.Printf("ОТКАЗ: расхождение %+d сек больше допустимых ±99 сек для одной команды HR1009\n", seconds)
		return
	}

	// УВП-280.01: HR1009 использует повторённое десятичное значение секунд:
	// +15 сек -> 1515dec, -24 сек -> -2424dec. Поэтому command = seconds*101.
	command := int16(seconds * 101)
	pdu := modbus.BuildWriteSingleRegisterPDU(1009, uint16(command))
	resp, err := modbus.Transact(ctx, tr, isTCP, modbus.NextTxID(), u, pdu)
	if err != nil {
		fatal("запись HR1009", err)
	}
	if len(resp) < len(pdu) || !bytes.Equal(resp[:len(pdu)], pdu) {
		fatal("проверка эха HR1009", fmt.Errorf("неожиданный ответ: % X, ожидалось: % X", resp, pdu))
	}

	fmt.Printf("HR1009 записан: %+d сек -> значение %d (0x%04X)\n", seconds, command, uint16(command))
	time.Sleep(1500 * time.Millisecond)

	afterTime, afterMidpoint, err := readVKMClock(ctx, tr, isTCP, u)
	if err != nil {
		fatal("контрольное чтение часов после коррекции", err)
	}
	fmt.Println("контроль после коррекции:")
	printClockComparison(afterTime, afterMidpoint)
}

func readVKMClock(ctx context.Context, tr transport.Transport, isTCP bool, unit uint8) (time.Time, time.Time, error) {
	pdu, err := modbus.BuildReadPDUWithQty("HR", 1800, 6)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}

	before := time.Now()
	resp, err := modbus.Transact(ctx, tr, isTCP, modbus.NextTxID(), unit, pdu)
	after := time.Now()
	midpoint := before.Add(after.Sub(before) / 2)
	if err != nil {
		return time.Time{}, midpoint, err
	}
	if len(resp) < 14 || resp[0] != 0x03 || resp[1] != 12 {
		return time.Time{}, midpoint, fmt.Errorf("неожиданный ответ часов: % X", resp)
	}

	vals := make([]int, 6)
	for i := 0; i < 6; i++ {
		vals[i] = int(int16(binary.BigEndian.Uint16(resp[2+i*2 : 4+i*2])))
	}

	day, month, year2 := vals[0], vals[1], vals[2]
	hour, minute, second := vals[3], vals[4], vals[5]
	if year2 >= 0 && year2 < 100 {
		year2 += 2000
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return time.Time{}, midpoint, fmt.Errorf("значения часов вне диапазона: %02d.%02d.%04d %02d:%02d:%02d", day, month, year2, hour, minute, second)
	}

	t := time.Date(year2, time.Month(month), day, hour, minute, second, 0, time.Local)
	if t.Year() != year2 || int(t.Month()) != month || t.Day() != day {
		return time.Time{}, midpoint, fmt.Errorf("некорректная дата: %02d.%02d.%04d", day, month, year2)
	}
	return t, midpoint, nil
}

func printClockComparison(deviceTime, serverTime time.Time) {
	drift := deviceTime.Sub(serverTime).Seconds()
	fmt.Printf("ВКМ:    %s\n", deviceTime.Format("02.01.2006 15:04:05"))
	fmt.Printf("сервер: %s\n", serverTime.Format("02.01.2006 15:04:05"))
	fmt.Printf("расхождение ВКМ - сервер: %+.1f сек\n", drift)
}

func readInt16(ctx context.Context, tr transport.Transport, isTCP bool, unit uint8, addr int, label string) (int16, error) {
	data, err := modbus.ReadPoint(ctx, tr, isTCP, unit, "HR", addr, "int16")
	if err != nil {
		fmt.Printf("HR %-4d (%s): ошибка: %v\n", addr, label, err)
		return 0, err
	}
	v, err := codec.DecodeInt16(data)
	if err != nil {
		fmt.Printf("HR %-4d (%s): сырые байты = % X, decode error: %v\n", addr, label, data, err)
		return 0, err
	}
	fmt.Printf("HR %-4d (%s): %d (байты % X)\n", addr, label, v, data)
	return v, nil
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "vkmtimeprobe: %s: %v\n", what, err)
	os.Exit(1)
}
