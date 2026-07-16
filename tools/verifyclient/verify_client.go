// verify_client.go — standalone Modbus TCP master for manual verification
// of the mbgw northbound slave. No dependency on the mbgw module — just
// stdlib, so it runs directly with `go run verify_client.go [addr] [unitID]`
// from any directory.
//
// Reads 8 holding registers starting at 0, matching testdata's
// acron_uspd.yaml layout:
//
//	reg 0-1: V (float, m/s)
//	reg 2-3: Q (float, m3/h)
//	reg 4-5: am (float, mV)
//	reg 6-7: acc_time (int32, min)
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strconv"
	"time"
)

func main() {
	addr := "127.0.0.1:1502"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	unitID := byte(1)
	if len(os.Args) > 2 {
		v, err := strconv.Atoi(os.Args[2])
		if err != nil {
			fmt.Println("bad unit id:", err)
			os.Exit(1)
		}
		unitID = byte(v)
	}

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer conn.Close()

	req := make([]byte, 12)
	binary.BigEndian.PutUint16(req[0:2], 1) // transaction id
	binary.BigEndian.PutUint16(req[2:4], 0) // protocol id
	binary.BigEndian.PutUint16(req[4:6], 6) // length
	req[6] = unitID
	req[7] = 0x03                             // read holding registers
	binary.BigEndian.PutUint16(req[8:10], 0)  // start addr
	binary.BigEndian.PutUint16(req[10:12], 8) // qty

	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(req); err != nil {
		fmt.Println("write:", err)
		os.Exit(1)
	}

	header := make([]byte, 7)
	if _, err := io.ReadFull(conn, header); err != nil {
		fmt.Println("read header:", err)
		os.Exit(1)
	}
	length := int(header[4])<<8 | int(header[5])
	payload := make([]byte, length-1)
	if _, err := io.ReadFull(conn, payload); err != nil {
		fmt.Println("read payload:", err)
		os.Exit(1)
	}

	function := payload[0]
	if function&0x80 != 0 {
		fmt.Printf("EXCEPTION: function=0x%02X code=%d\n", function, payload[1])
		return
	}

	byteCount := payload[1]
	data := payload[2 : 2+int(byteCount)]
	fmt.Printf("raw data (%d bytes): % X\n\n", len(data), data)

	if len(data) < 8 {
		fmt.Println("not enough data for V/Q")
		return
	}
	v := math.Float32frombits(binary.BigEndian.Uint32(data[0:4]))
	q := math.Float32frombits(binary.BigEndian.Uint32(data[4:8]))
	fmt.Printf("V (скорость)  = %v m/s\n", v)
	fmt.Printf("Q (расход)    = %v m3/h\n", q)

	if len(data) >= 12 {
		am := math.Float32frombits(binary.BigEndian.Uint32(data[8:12]))
		fmt.Printf("am (амплитуда)= %v mV\n", am)
	}
	if len(data) >= 16 {
		accTime := int32(binary.BigEndian.Uint32(data[12:16]))
		fmt.Printf("acc_time      = %v min\n", accTime)
	}
}
