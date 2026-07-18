package simulator

import (
	"encoding/binary"
	"log"
	"net"

	"mbgw/internal/protocol/modbus"
)

// akron01Registers holds static current-value/time registers for the
// Akron-01 vertical-slice emulator, addressed as 16-bit Modbus holding
// registers (function 03), per "ВЗАИМОДЕЙСТВИЕ С КОНТРОЛЛЕРОМ СЕТИ
// MODBUS ПРИБОРОВ Акрон-01 и Акрон-02-1" table 2. Multi-byte values are
// little-endian on the wire (word_order_32="3210" in the profile).
//
// This emulator speaks raw Modbus RTU framing (address+PDU+CRC16) over a
// plain TCP socket, matching how a real RS-485-to-Ethernet converter
// transparently passes RTU bytes through (transport.KindTCPSerial in
// this project) - NOT the MBAP-wrapped Modbus TCP framing used by, e.g.,
// the IVK-TER/TSRV-024 simulators (which model devices connected via a
// true Modbus TCP gateway). Akron-01 is connected via RS-485 directly in
// this project's real deployment, so RTU-over-TCP is the correct model
// to test against here.
var akron01Registers = map[int][]byte{
	0:  {0xBD, 0x6D, 0xF7, 0x3E}, // V = 0.483 m/s
	2:  {0x3B, 0xBB, 0xE7, 0x41}, // Q = 28.96 m3/h
	4:  {0x00, 0x00, 0x83, 0x42}, // am = 65.5 mV
	10: {0xAE, 0x06, 0x00, 0x00}, // acc_time = 1710 min
	16: {0x00, 0x55},             // second=00(BCD), minute=55(BCD)
	17: {0x10, 0x03},             // hour=10(BCD), day_of_week=3
	18: {0x05, 0x03},             // date=05(BCD), month=03(BCD)
	19: {0x08, 0x00},             // year=08(BCD,2008), id_am unused
}

// akron01HourlyRow is a fixed test hourly-archive row (9 bytes): the same
// U=5827/Pu=2 (->582.7 m3) and 10:00 05.03.2008 values already verified
// in archive/merkuriy_long_response_test.go's TestAkronArchive_HourlyRead.
var akron01HourlyRow = []byte{0xC3, 0x16, 0x00, 0x00, 0x02, 0x10, 0x05, 0x03, 0x08}

func RunAkron01(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go handleAkron01Conn(conn)
	}
}

func handleAkron01Conn(conn net.Conn) {
	defer conn.Close()

	buf := make([]byte, 512)
	for {
		n, err := conn.Read(buf)
		if err != nil || n < 4 {
			// RTU minimum frame: addr(1)+code(1)+CRC(2) = 4 bytes.
			return
		}
		frame := append([]byte(nil), buf[:n]...)

		unitID, pdu, err := modbus.ParseRTUFrame(frame)
		if err != nil {
			log.Printf("[sim akron-01] bad RTU frame: %v\n", err)
			continue
		}
		if len(pdu) < 1 {
			continue
		}

		respPDU := buildAkron01Response(pdu)
		if respPDU == nil {
			continue
		}

		respFrame := modbus.BuildRTUFrame(unitID, respPDU)
		if _, err := conn.Write(respFrame); err != nil {
			return
		}
	}
}

func buildAkron01Response(pdu []byte) []byte {
	funcCode := pdu[0]

	// Command 101 (identification): the device's static passport. Added so
	// that the gateway's one-time DetectPassport (startup, cmd/mbgw/run.go)
	// works against this emulator too, not only against the discovery
	// responder — same reply shape and fixture identity as
	// akron_responder.go's akronIdentityPDU: type 0x00, firmware 3.7 (0x37),
	// serial 12345 little-endian.
	if funcCode == 101 {
		return []byte{101, 6, 0x00, 0x37, 0x39, 0x30, 0x00, 0x00}
	}

	// User-Defined commands (100-110 decimal): hourly (104) and daily
	// (105) archive. PDU shape: code(1) + i-hi(1) + i-lo(1) + n(1) - see
	// internal/protocol/akron.BuildRequestPDU. This vertical-slice
	// emulator always returns the same fixed row(s) regardless of the
	// requested start index, up to the requested count.
	if funcCode == 104 || funcCode == 105 {
		if len(pdu) < 4 {
			return nil
		}
		n := int(pdu[3])
		if n < 1 {
			n = 1
		}
		data := make([]byte, 0, n*len(akron01HourlyRow))
		for i := 0; i < n; i++ {
			data = append(data, akron01HourlyRow...)
		}
		return append([]byte{funcCode, byte(len(data))}, data...)
	}

	if funcCode != 0x03 && funcCode != 0x04 {
		return nil
	}
	if len(pdu) < 5 {
		return nil
	}

	addr := int(binary.BigEndian.Uint16(pdu[1:3]))
	qty := int(binary.BigEndian.Uint16(pdu[3:5]))

	data := readAkron01Registers(addr, qty)
	if data == nil {
		return nil
	}
	resp := make([]byte, 2+len(data))
	resp[0] = funcCode
	resp[1] = byte(len(data))
	copy(resp[2:], data)
	return resp
}

func readAkron01Registers(addr int, qty int) []byte {
	out := make([]byte, 0, qty*2)
	remaining := qty * 2
	a := addr
	for remaining > 0 {
		raw, ok := akron01Registers[a]
		if !ok {
			return nil
		}
		out = append(out, raw...)
		remaining -= len(raw)
		a += len(raw) / 2
	}
	if remaining != 0 {
		return nil
	}
	return out
}
