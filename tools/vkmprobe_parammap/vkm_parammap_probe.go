// vkm_parammap_probe.go — standalone probe for the ЭЛЕМЕР-ВКМ-360
// "чтение карты параметров" mechanism (registri_mbrrtu_vkm.pdf, стр.8):
//
//	7600 HR, int16  write (pipe<<8 | rowIndex) — старшие 8 бит номер
//	                трубопровода, младшие 8 бит индекс строки карты.
//	7700 HR..7770   ANSI string[140] — имя запрошенного параметра.
//	7800 HR..7870   ANSI string[140] — значение запрошенного параметра.
//	Пустая строка (нулевая длина) = строки с таким индексом нет — конец
//	карты для этого трубопровода.
//
// Independent of the mbgw module — plain net + encoding/binary, no
// internal package imports — so it can be built and run standalone
// without touching or risking the main codebase.
//
// Build:
//
//	go build -o vkmprobe_parammap.exe vkm_parammap_probe.go
//
// Run:
//
//	.\vkmprobe_parammap.exe -addr 192.168.x.x:502 -unit 1 -pipe 1 -maxrows 60
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "", "device address, host:port (Modbus TCP)")
	unit := flag.Int("unit", 1, "Modbus unit/slave id")
	pipe := flag.Int("pipe", 1, "pipe number (номер трубопровода)")
	maxRows := flag.Int("maxrows", 60, "stop after this many empty/consecutive-empty rows")
	timeout := flag.Duration("timeout", 5*time.Second, "per-request timeout")
	flag.Parse()

	if *addr == "" {
		fmt.Fprintln(os.Stderr, "usage: vkmprobe_parammap -addr host:port [-unit N] [-pipe N] [-maxrows N]")
		os.Exit(2)
	}

	conn, err := net.DialTimeout("tcp", *addr, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect %s: %v\n", *addr, err)
		os.Exit(1)
	}
	defer conn.Close()

	fmt.Printf("Карта параметров трубопровода %d (%s):\n\n", *pipe, *addr)

	emptyStreak := 0
	found := 0
	for row := 0; row < *maxRows; row++ {
		code := uint16(*pipe)<<8 | uint16(row&0xFF)

		if err := writeSingleRegister(conn, byte(*unit), 7600, code, *timeout); err != nil {
			fmt.Fprintf(os.Stderr, "row %d: write 7600 failed: %v\n", row, err)
			break
		}

		// The device needs a brief moment to populate 7700/7800 after the
		// write; documentation doesn't state a required delay, so use a
		// small conservative one.
		time.Sleep(50 * time.Millisecond)

		name, err := readANSIString(conn, byte(*unit), 7700, 35, *timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "row %d: read 7700 failed: %v\n", row, err)
			break
		}
		val, err := readANSIString(conn, byte(*unit), 7800, 35, *timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "row %d: read 7800 failed: %v\n", row, err)
			break
		}

		if name == "" {
			emptyStreak++
			if emptyStreak >= 3 {
				fmt.Printf("... (3 пустых строки подряд, останавливаюсь на row=%d)\n", row)
				break
			}
			continue
		}
		emptyStreak = 0
		found++
		fmt.Printf("row %3d: %-30s = %s\n", row, name, val)
	}

	fmt.Printf("\nВсего найдено параметров: %d\n", found)
}

// --- minimal Modbus TCP (MBAP) client, just what this probe needs ---

var txID uint16

func nextTxID() uint16 {
	txID++
	return txID
}

func writeSingleRegister(conn net.Conn, unit byte, addr int, value uint16, timeout time.Duration) error {
	pdu := make([]byte, 5)
	pdu[0] = 0x06
	binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
	binary.BigEndian.PutUint16(pdu[3:5], value)

	resp, err := transact(conn, unit, pdu, timeout)
	if err != nil {
		return err
	}
	if len(resp) > 0 && resp[0]&0x80 != 0 {
		return fmt.Errorf("modbus exception 0x%02X", resp[1])
	}
	return nil
}

// readANSIString reads qty holding registers starting at addr (FC03) and
// returns the bytes decoded as a NUL-terminated ANSI (Windows-1251-ish;
// here just trimmed at the first 0x00 and returned as raw latin/cyrillic
// bytes cast to string — good enough for eyeballing tag names, which are
// ASCII per the doc's "состоит из латинских букв и цифр").
func readANSIString(conn net.Conn, unit byte, addr, qty int, timeout time.Duration) (string, error) {
	pdu := make([]byte, 5)
	pdu[0] = 0x03
	binary.BigEndian.PutUint16(pdu[1:3], uint16(addr))
	binary.BigEndian.PutUint16(pdu[3:5], uint16(qty))

	resp, err := transact(conn, unit, pdu, timeout)
	if err != nil {
		return "", err
	}
	if len(resp) > 0 && resp[0]&0x80 != 0 {
		return "", fmt.Errorf("modbus exception 0x%02X", resp[1])
	}
	if len(resp) < 2 {
		return "", fmt.Errorf("response too short")
	}
	data := resp[2:]
	end := len(data)
	for i, b := range data {
		if b == 0 {
			end = i
			break
		}
	}
	return cp1251ToUTF8(data[:end]), nil
}

// cp1251ToUTF8 decodes Windows-1251 (the device's documented ANSI string
// encoding — same as the archive string's cp1251, see cp1251.go in the
// main mbgw codebase) into a proper Go (UTF-8) string. Bytes 0x00-0x7F are
// plain ASCII (unchanged); 0x80-0xFF map to the standard cp1251 table.
// Without this, raw device bytes printed as a Go string show as mojibake
// in any UTF-8-expecting terminal — exactly what the first run displayed.
func cp1251ToUTF8(b []byte) string {
	// Standard Windows-1251 high-byte (0x80-0xFF) to Unicode code point
	// table, per the codepage's published mapping.
	var table = [128]rune{
		0x0402, 0x0403, 0x201A, 0x0453, 0x201E, 0x2026, 0x2020, 0x2021,
		0x20AC, 0x2030, 0x0409, 0x2039, 0x040A, 0x040C, 0x040B, 0x040F,
		0x0452, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
		0xFFFD, 0x2122, 0x0459, 0x203A, 0x045A, 0x045C, 0x045B, 0x045F,
		0x00A0, 0x040E, 0x045E, 0x0408, 0x00A4, 0x0490, 0x00A6, 0x00A7,
		0x0401, 0x00A9, 0x0404, 0x00AB, 0x00AC, 0x00AD, 0x00AE, 0x0407,
		0x00B0, 0x00B1, 0x0406, 0x0456, 0x0491, 0x00B5, 0x00B6, 0x00B7,
		0x0451, 0x2116, 0x0454, 0x00BB, 0x0458, 0x0405, 0x0455, 0x0457,
		0x0410, 0x0411, 0x0412, 0x0413, 0x0414, 0x0415, 0x0416, 0x0417,
		0x0418, 0x0419, 0x041A, 0x041B, 0x041C, 0x041D, 0x041E, 0x041F,
		0x0420, 0x0421, 0x0422, 0x0423, 0x0424, 0x0425, 0x0426, 0x0427,
		0x0428, 0x0429, 0x042A, 0x042B, 0x042C, 0x042D, 0x042E, 0x042F,
		0x0430, 0x0431, 0x0432, 0x0433, 0x0434, 0x0435, 0x0436, 0x0437,
		0x0438, 0x0439, 0x043A, 0x043B, 0x043C, 0x043D, 0x043E, 0x043F,
		0x0440, 0x0441, 0x0442, 0x0443, 0x0444, 0x0445, 0x0446, 0x0447,
		0x0448, 0x0449, 0x044A, 0x044B, 0x044C, 0x044D, 0x044E, 0x044F,
	}
	out := make([]rune, 0, len(b))
	for _, c := range b {
		if c < 0x80 {
			out = append(out, rune(c))
		} else {
			out = append(out, table[c-0x80])
		}
	}
	return string(out)
}

// transact sends one MBAP-framed PDU and returns the PDU of the response
// (function code + data, no MBAP header).
func transact(conn net.Conn, unit byte, pdu []byte, timeout time.Duration) ([]byte, error) {
	id := nextTxID()
	frame := make([]byte, 7+len(pdu))
	binary.BigEndian.PutUint16(frame[0:2], id)
	binary.BigEndian.PutUint16(frame[2:4], 0) // protocol id, always 0
	binary.BigEndian.PutUint16(frame[4:6], uint16(1+len(pdu)))
	frame[6] = unit
	copy(frame[7:], pdu)

	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(frame); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	header := make([]byte, 7)
	if _, err := readFull(conn, header); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	length := binary.BigEndian.Uint16(header[4:6])
	if length < 1 {
		return nil, fmt.Errorf("bad MBAP length %d", length)
	}
	body := make([]byte, length-1) // minus unit id byte already read
	if len(body) > 0 {
		if _, err := readFull(conn, body); err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
	}
	return body, nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
