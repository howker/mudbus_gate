// Command vkmproxy — прозрачный Modbus TCP прокси между ЭС и реальным
// прибором, для решающего диагностического эксперимента (2026-08-10):
// мы перебрали множество комбинаций формата архивной строки (шапки
// да/нет, экспонента развёрнута/нет, масштаб полей) — ни одна не дала
// ЭС достоверное (State=0) значение через МШ, при том что тот же самый
// прибор напрямую даёт State=0 мгновенно. Значит либо дело не в тексте
// строки вообще, либо мы ещё не нашли верную комбинацию.
//
// vkmproxy пересылает КАЖДЫЙ запрос от ЭС напрямую прибору и возвращает
// ответ прибора БЕЗ ЕДИНОГО ИЗМЕНЕНИЯ (только подменяет unit id в MBAP
// заголовке — ЭС адресует точку своим "Адрес N", а на приборе физически
// может быть другой unit).
//
// Если через этот прокси канал в ЭС всё равно останется State=1 — это
// докажет, что причина НЕ в содержимом (оно тут байт-в-байт от прибора),
// а в чём-то на уровне протокола/транспорта/тайминга.
// Если заработает (State=0) — вернёт нас к разбору формата строки, но
// уже с точным знанием, что дело именно там, а не в другом месте.
//
// Использование:
//
//	vkmproxy --listen 127.0.0.1:15022 --target 10.48.228.126:502 \
//	    --es-unit 1 --device-unit 2 --log vkm_proxy.jsonl
package main

import (
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

func main() {
	listenAddr := flag.String("listen", "127.0.0.1:15022", "адрес, на котором слушаем ЭС (как раньше слушал northbound --serve-vkm)")
	targetAddr := flag.String("target", "", "адрес реального прибора, например 10.48.228.126:502 (обязателен)")
	esUnit := flag.Uint("es-unit", 1, "unit id, который использует ЭС при обращении к точке (обычно совпадает с 'Адрес N' в параметрах USD)")
	deviceUnit := flag.Uint("device-unit", 2, "unit id реального прибора на проводе (для vkm360_real — обычно 2, см. config.vkm360_real.yaml)")
	logPath := flag.String("log", "vkm_proxy.jsonl", "куда писать лог пересылаемых транзакций (тот же формат полей, что vkm_live.jsonl, для сравнения)")
	flag.Parse()

	if *targetAddr == "" {
		fmt.Fprintln(os.Stderr, "ошибка: --target обязателен (адрес реального прибора, например 10.48.228.126:502)")
		os.Exit(1)
	}

	var logMu sync.Mutex
	var logFile *os.File
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			log.Printf("vkmproxy: не могу открыть лог %s: %v — работаю без лога\n", *logPath, err)
		} else {
			logFile = f
			defer f.Close()
		}
	}

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("vkmproxy: не могу слушать %s: %v", *listenAddr, err)
	}
	log.Printf("vkmproxy: слушаю %s, ПРОЗРАЧНО пересылаю на %s (unit ЭС=%d <-> unit прибора=%d)\n",
		*listenAddr, *targetAddr, *esUnit, *deviceUnit)
	log.Println("vkmproxy: содержимое ответов НЕ МЕНЯЕТСЯ — это байт-в-байт то, что реально шлёт прибор.")

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("vkmproxy: accept error: %v\n", err)
			continue
		}
		go handleConn(conn, *targetAddr, byte(*esUnit), byte(*deviceUnit), logFile, &logMu)
	}
}

// mbapFrame — один разобранный Modbus TCP кадр (MBAP-заголовок + PDU).
type mbapFrame struct {
	txID     uint16
	protoID  uint16
	unitID   byte
	pdu      []byte
	rawBytes []byte // полный исходный кадр, как пришёл на проводе
}

// readMBAPFrame читает ровно один Modbus TCP кадр из conn.
func readMBAPFrame(conn net.Conn) (*mbapFrame, error) {
	header := make([]byte, 7)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint16(header[4:6])
	if length < 1 || length > 260 {
		return nil, fmt.Errorf("подозрительная длина MBAP: %d", length)
	}
	body := make([]byte, length-1) // length считает unitID(1)+PDU
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	full := append(append([]byte{}, header...), body...)
	return &mbapFrame{
		txID:     binary.BigEndian.Uint16(header[0:2]),
		protoID:  binary.BigEndian.Uint16(header[2:4]),
		unitID:   header[6],
		pdu:      body,
		rawBytes: full,
	}, nil
}

// writeMBAPFrame отправляет кадр с указанными txID/unitID, сохраняя PDU
// как есть (для ответа от прибора PDU не трогаем вообще — прозрачность).
func writeMBAPFrame(conn net.Conn, txID uint16, unitID byte, pdu []byte) error {
	frame := make([]byte, 7+len(pdu))
	binary.BigEndian.PutUint16(frame[0:2], txID)
	binary.BigEndian.PutUint16(frame[2:4], 0) // protocol id всегда 0 для Modbus TCP
	binary.BigEndian.PutUint16(frame[4:6], uint16(1+len(pdu)))
	frame[6] = unitID
	copy(frame[7:], pdu)
	_, err := conn.Write(frame)
	return err
}

// handleConn обслуживает одно TCP-соединение от ЭС: на каждый запрос
// открывает (переиспользует) соединение к реальному прибору, пересылает
// PDU 1-в-1 (меняя только unit id), получает ответ и пересылает его
// клиенту (ЭС) снова 1-в-1, с unit id, каким его ожидает клиент.
func handleConn(client net.Conn, targetAddr string, esUnit, deviceUnit byte, logFile *os.File, logMu *sync.Mutex) {
	defer client.Close()
	remote := client.RemoteAddr().String()

	device, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
	if err != nil {
		log.Printf("vkmproxy: не могу подключиться к прибору %s: %v (клиент %s)\n", targetAddr, err, remote)
		return
	}
	defer device.Close()
	log.Printf("vkmproxy: клиент %s подключился, проксирую на прибор %s\n", remote, targetAddr)

	for {
		req, err := readMBAPFrame(client)
		if err != nil {
			if err != io.EOF {
				log.Printf("vkmproxy: клиент %s: ошибка чтения запроса: %v\n", remote, err)
			}
			return
		}
		writeLog(logFile, logMu, remote, "request", req.unitID, req.pdu)

		// Пересылаем прибору тот же PDU, но с unit id прибора.
		if err := writeMBAPFrame(device, req.txID, deviceUnit, req.pdu); err != nil {
			log.Printf("vkmproxy: клиент %s: ошибка записи прибору: %v\n", remote, err)
			return
		}

		device.SetReadDeadline(time.Now().Add(10 * time.Second))
		resp, err := readMBAPFrame(device)
		if err != nil {
			log.Printf("vkmproxy: клиент %s: ошибка чтения ответа от прибора: %v\n", remote, err)
			return
		}
		writeLog(logFile, logMu, remote, "response", esUnit, resp.pdu)

		// Возвращаем клиенту тот же PDU от прибора БЕЗ ИЗМЕНЕНИЙ, но с
		// unit id, каким его ожидает клиент (ЭС адресовала запрос своим
		// unit — прозрачность требует ответить тем же unit, иначе ЭС
		// сама отбросит ответ как несовпадающий по адресу).
		if err := writeMBAPFrame(client, req.txID, esUnit, resp.pdu); err != nil {
			log.Printf("vkmproxy: клиент %s: ошибка записи ответа клиенту: %v\n", remote, err)
			return
		}
	}
}

// writeLog пишет одну строку в том же духе, что vkm_live.jsonl (unit,
// function, payload_hex) — чтобы можно было напрямую сравнивать с уже
// накопленными логами northbound за сегодня.
func writeLog(f *os.File, mu *sync.Mutex, remote, direction string, unit byte, pdu []byte) {
	if f == nil {
		return
	}
	var function *byte
	if len(pdu) > 0 {
		function = &pdu[0]
	}
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(f, `{"ts":%q,"remote":%q,"direction":%q,"unit":%d,"function":%v,"payload_hex":%q}`+"\n",
		time.Now().Format(time.RFC3339Nano), remote, direction, unit, funcOrNull(function), hex.EncodeToString(pdu))
}

func funcOrNull(b *byte) interface{} {
	if b == nil {
		return nil
	}
	return int(*b)
}
