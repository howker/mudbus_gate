// akronprobe talks to the mbgw Akron carrier the way Энергосфера does:
// bare Modbus RTU frames over raw TCP. It sends the M3-captured request
// sequence (110, 101, 03-time, 102, 104) and prints decoded replies, so a
// full upstream smoke can be run locally without the real Энергосфера.
//
// Usage:
//
//	go run ./tools/akronprobe <addr>          // e.g. 127.0.0.1:15021
//	go run ./tools/akronprobe <addr> <unitID> // default unit 1
package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"time"

	"mbgw/internal/codec"
	"mbgw/internal/protocol/modbus"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: akronprobe <addr> [unitID]")
		os.Exit(1)
	}
	addr := os.Args[1]
	unit := uint8(1)
	if len(os.Args) > 2 {
		if v, err := strconv.Atoi(os.Args[2]); err == nil {
			unit = uint8(v)
		}
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Printf("dial %s: %v\n", addr, err)
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Printf("подключено к %s (unit %d)\n\n", addr, unit)

	probe(conn, unit, "110 статус", []byte{110}, decode110)
	probe(conn, unit, "101 идентификация", []byte{101}, decode101)
	probe(conn, unit, "03 время (0x10..0x13)", []byte{0x03, 0x00, 0x10, 0x00, 0x04}, decodeClock)
	probe(conn, unit, "102 текущие", []byte{102}, decode102)
	probe(conn, unit, "104 архив i=1 n=3", []byte{104, 0x00, 0x01, 0x03}, decode104)
}

func probe(conn net.Conn, unit uint8, title string, pdu []byte, dec func([]byte)) {
	frame := modbus.BuildRTUFrame(unit, pdu)
	fmt.Printf("== %s\n>> %s\n", title, hex.EncodeToString(frame))
	if _, err := conn.Write(frame); err != nil {
		fmt.Printf("   write: %v\n\n", err)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		fmt.Printf("   (нет ответа: %v)\n\n", err)
		return
	}
	fmt.Printf("<< %s\n", hex.EncodeToString(buf[:n]))
	_, respPDU, perr := modbus.ParseRTUFrame(buf[:n])
	if perr != nil {
		fmt.Printf("   разбор кадра: %v\n\n", perr)
		return
	}
	dec(respPDU)
	fmt.Println()
}

func decode110(pdu []byte) {
	if len(pdu) >= 3 && pdu[1] == 1 && pdu[2] == 1 {
		fmt.Println("   статус: ГОТОВ (6e 01 01)")
	} else {
		fmt.Printf("   статус: %x\n", pdu)
	}
}

func decode101(pdu []byte) {
	if len(pdu) < 8 {
		fmt.Printf("   короткий ответ: %x\n", pdu)
		return
	}
	serial := binary.LittleEndian.Uint32(pdu[4:8])
	fmt.Printf("   тип=0x%02X версия=%d.%d заводской №=%d\n", pdu[2], pdu[3]>>4, pdu[3]&0x0F, serial)
}

func decodeClock(pdu []byte) {
	if len(pdu) < 10 {
		fmt.Printf("   короткий ответ: %x\n", pdu)
		return
	}
	d := pdu[2:]
	bcd := func(b byte) int { return int(b>>4)*10 + int(b&0x0F) }
	fmt.Printf("   время прибора: %02d:%02d:%02d %02d.%02d.20%02d\n",
		bcd(d[2]), bcd(d[1]), bcd(d[0]), bcd(d[4]), bcd(d[5]), bcd(d[6]))
}

func decode102(pdu []byte) {
	if len(pdu) < 20 {
		fmt.Printf("   короткий ответ: %x\n", pdu)
		return
	}
	d := pdu[2:]
	v := float32frombytes(d[0:4])
	q := float32frombytes(d[4:8])
	vol, _ := codec.DecodeAkronVolume(d[8:13], "3210")
	acc := binary.LittleEndian.Uint32(d[13:17])
	fmt.Printf("   V=%.3f  Q=%.3f  объём=%.3f  наработка=%d мин  неисправность=0x%02X\n",
		v, q, vol, acc, d[17])
}

func decode104(pdu []byte) {
	if len(pdu) < 2 {
		fmt.Printf("   короткий ответ: %x\n", pdu)
		return
	}
	byteCount := int(pdu[1])
	if byteCount == 0 {
		fmt.Println("   архив: записей нет")
		return
	}
	rows := pdu[2:]
	if len(rows) < byteCount || byteCount%9 != 0 {
		fmt.Printf("   странная длина: byteCount=%d данных=%d\n", byteCount, len(rows))
		return
	}
	bcd := func(b byte) int { return int(b>>4)*10 + int(b&0x0F) }
	for i := 0; i < byteCount/9; i++ {
		r := rows[i*9 : i*9+9]
		vol, _ := codec.DecodeAkronVolume(r[0:5], "3210")
		fmt.Printf("   строка %d: %.3f м3 @ %02d:00 %02d.%02d.20%02d\n",
			i+1, vol, bcd(r[5]), bcd(r[6]), bcd(r[7]), bcd(r[8]))
	}
}

func float32frombytes(b []byte) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}
