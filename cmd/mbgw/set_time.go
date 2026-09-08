package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"mbgw/internal/codec"
	"mbgw/internal/config"
	"mbgw/internal/pollcore"
	"mbgw/internal/profile"
	"mbgw/internal/protocol/modbus"
	sqliterepo "mbgw/internal/storage/sqlite"
	"mbgw/internal/transport"
)

const setTimeVerifyTolerance = 2 * time.Second

// setTime writes the current system time to a VZLET device's HR 0x8000
// "time set" register (function 16, standard Modbus - see
// mb_reg_ivk_ter.pdf / modbus_regs_tsrv_024m.pdf). Unlike Merkuriy's
// correct-time, VZLET has no documented +/-4min correction mode distinct
// from a full set: the profile exposes one writable clock register only.
//
// The write therefore remains an explicit operator action. IVK-TER in
// particular accepts it only in Service/Setup mode. The command first reads
// the live clock, uses a request midpoint for the drift calculation, writes
// the new Unix timestamp, validates the function-16 acknowledgement, then
// reads the clock back and refuses to report success unless the result is
// actually close to server time.
type setTimeTarget struct {
	ProfilePath   string
	TransportKind string
	Host          string
	Port          int
	COM           string
	Baudrate      int
	Parity        string
	StopBits      int
	TimeoutMs     int
	UnitID        int
}

// loadSetTimeTarget сначала ищет прибор в production-БД mbgw_server.db,
// потому что `mbgw server` и веб-интерфейс используют её как источник
// конфигурации. Если production-БД существует, она является единственным
// источником для опасной операции записи времени: при неизвестном deviceID
// fail closed, без отката на потенциально устаревший config.yaml. Legacy-
// fallback используется только когда mbgw_server.db рядом с exe отсутствует.
func loadSetTimeTarget(deviceID string) (setTimeTarget, error) {
	dbPath := nextToExe("mbgw_server.db")
	_, statErr := os.Stat(dbPath)
	if statErr == nil {
		repo, err := sqliterepo.New(dbPath)
		if err != nil {
			return setTimeTarget{}, fmt.Errorf("не удалось открыть production-БД МодбасШлюза: %w", err)
		}
		rec, found, getErr := repo.GetDevice(context.Background(), deviceID)
		_ = repo.Close()
		if getErr != nil {
			return setTimeTarget{}, fmt.Errorf("не удалось прочитать прибор из БД МодбасШлюза: %w", getErr)
		}
		if !found {
			return setTimeTarget{}, fmt.Errorf("прибор %q не найден в production-БД mbgw_server.db; legacy config.yaml намеренно не используется для записи времени", deviceID)
		}
		return setTimeTarget{
			ProfilePath: profilePathNextToExe(rec.Profile), TransportKind: rec.TransportKind,
			Host: rec.Host, Port: rec.Port, COM: rec.COM, Baudrate: rec.Baudrate,
			Parity: rec.Parity, StopBits: rec.StopBits, TimeoutMs: rec.TimeoutMs, UnitID: rec.UnitID,
		}, nil
	}
	if !os.IsNotExist(statErr) {
		return setTimeTarget{}, fmt.Errorf("не удалось проверить production-БД mbgw_server.db: %w", statErr)
	}

	cfg, err := config.Load("config.yaml")
	if err != nil {
		return setTimeTarget{}, fmt.Errorf("production-БД mbgw_server.db отсутствует; не удалось загрузить legacy config.yaml для прибора %q: %w", deviceID, err)
	}
	for i := range cfg.Devices {
		if cfg.Devices[i].ID != deviceID {
			continue
		}
		d := &cfg.Devices[i]
		return setTimeTarget{
			ProfilePath: d.Profile, TransportKind: d.Transport.Kind, Host: d.Transport.Host, Port: d.Transport.Port,
			COM: d.Transport.COM, Baudrate: d.Transport.Baudrate, Parity: d.Transport.Parity, StopBits: d.Transport.StopBits,
			TimeoutMs: d.Transport.TimeoutMs, UnitID: int(d.Transport.UnitID),
		}, nil
	}
	return setTimeTarget{}, fmt.Errorf("прибор %q не найден в legacy config.yaml (production-БД mbgw_server.db отсутствует)", deviceID)
}

func setTime() {
	if len(os.Args) < 4 {
		fmt.Println("Использование: mbgw cli set-time <идентификатор_прибора> [--yes]")
		os.Exit(1)
	}
	deviceID := os.Args[3]

	// Полная установка времени не должна пересекаться с работающим server.
	// Используем ту же межпроцессную защиту, что и сам сервер: если служба
	// или ручной mbgw.exe server ещё живы, оператор сначала обязан их
	// штатно остановить.
	if ok, err := acquireSingleInstanceLock(); err != nil {
		fmt.Printf("Не удалось проверить, остановлен ли основной опрос: %v\n", err)
		os.Exit(1)
	} else if !ok {
		fmt.Println("Установка времени не выполнена: МодбасШлюз сейчас запущен. Сначала остановите службу или ручной сервер, дождитесь полной остановки и повторите команду.")
		os.Exit(1)
	}

	autoYes := false
	for _, a := range os.Args[4:] {
		if a == "--yes" || a == "-y" {
			autoYes = true
		}
	}

	target, err := loadSetTimeTarget(deviceID)
	if err != nil {
		fmt.Printf("Ошибка загрузки параметров прибора: %v\n", err)
		os.Exit(1)
	}

	p, err := profile.Parse(target.ProfilePath)
	if err != nil {
		fmt.Printf("Ошибка загрузки профиля: %v\n", err)
		os.Exit(1)
	}
	if p.Meta.Protocol != "modbus" {
		fmt.Printf("Команда set-time поддерживается только для приборов базового Modbus (протокол профиля: %q). Для Меркурия используйте команду correct-time.\n", p.Meta.Protocol)
		os.Exit(1)
	}

	var readPt, writePt *profile.Point
	for i := range p.Points {
		switch p.Points[i].Name {
		case "current_time":
			readPt = &p.Points[i]
		case "time_set":
			writePt = &p.Points[i]
		}
	}
	if readPt == nil || writePt == nil {
		fmt.Println("В профиле не найдены точки \"current_time\" (чтение) и/или \"time_set\" (запись) - команда set-time для этого прибора не настроена.")
		os.Exit(1)
	}
	if readPt.Type != "uint32" || writePt.Type != "uint32" {
		fmt.Printf("Неподдерживаемый формат часов: current_time=%q, time_set=%q; ожидается uint32 Unix time.\n", readPt.Type, writePt.Type)
		os.Exit(1)
	}

	isTCP := target.TransportKind == "modbus_tcp"
	trParams := transport.Params{
		Kind:            transport.Kind(target.TransportKind),
		Host:            target.Host,
		Port:            target.Port,
		COM:             target.COM,
		Baudrate:        target.Baudrate,
		Parity:          target.Parity,
		StopBits:        target.StopBits,
		ResponseTimeout: time.Duration(target.TimeoutMs) * time.Millisecond,
	}
	tr, err := transport.New(trParams)
	if err != nil {
		fmt.Printf("Ошибка создания транспорта: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	if err := tr.Open(ctx); err != nil {
		fmt.Printf("Ошибка открытия транспорта: %v\n", err)
		os.Exit(1)
	}
	defer tr.Close()

	unitID := target.UnitID
	if unitID == 0 {
		unitID = 1
	}
	if unitID < 1 || unitID > 247 {
		fmt.Printf("Некорректный адрес Modbus (Unit ID): %d; допустимый диапазон 1..247.\n", unitID)
		os.Exit(1)
	}
	reader := pollcore.New(tr, isTCP, uint8(unitID))

	deviceTime, midpoint, err := readVZLETClockForSetTime(ctx, reader, readPt, p.Codec.WordOrder32)
	if err != nil {
		fmt.Printf("Ошибка чтения времени прибора: %v\n", err)
		os.Exit(1)
	}
	drift := deviceTime.Sub(midpoint)

	fmt.Printf("Прибор:         %s\n", deviceID)
	fmt.Printf("Адрес Modbus (Unit ID): %d\n", unitID)
	fmt.Printf("Время прибора:  %s\n", deviceTime.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("Время системы:  %s\n", midpoint.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("Расхождение (прибор - система): %.3f сек\n", drift.Seconds())

	if math.Abs(drift.Seconds()) < 1 {
		fmt.Println("\nВремя прибора уже синхронизировано (|расхождение| < 1 с), установка не требуется.")
		return
	}

	fmt.Println("\nВНИМАНИЕ: это полная установка часов ВЗЛЁТ. Для ИВК-ТЭР прибор должен находиться в сервисном режиме (Service/Setup).")
	fmt.Println("Основной МодбасШлюз должен быть остановлен на время этой команды; проверка двойного запуска выполнена автоматически.")
	if !autoYes {
		fmt.Print("Выполнить установку времени прибора? [y/N]: ")
		stdin := bufio.NewReader(os.Stdin)
		answer, _ := stdin.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(answer)) != "y" {
			fmt.Println("Отменено.")
			return
		}
	}

	writeTime := time.Now()
	valBytes, err := codec.EncodeUint32(uint32(writeTime.Unix()), p.Codec.WordOrder32)
	if err != nil {
		fmt.Printf("Ошибка кодирования времени: %v\n", err)
		os.Exit(1)
	}
	if len(valBytes) != 4 {
		fmt.Printf("Неожиданная длина закодированного значения: %d байт\n", len(valBytes))
		os.Exit(1)
	}
	regs := []uint16{
		binary.BigEndian.Uint16(valBytes[0:2]),
		binary.BigEndian.Uint16(valBytes[2:4]),
	}

	reqPDU, err := modbus.BuildWriteMultipleRegistersPDU(writePt.AddrOrZero(), regs)
	if err != nil {
		fmt.Printf("Ошибка построения запроса записи: %v\n", err)
		os.Exit(1)
	}

	respPDU, err := reader.Transact(ctx, reqPDU)
	if err != nil {
		fmt.Printf("Ошибка записи времени: %v\n", err)
		fmt.Println("Для ИВК-ТЭР проверьте, что прибор переведён в сервисный режим (Service/Setup).")
		os.Exit(1)
	}
	if !sameWriteMultipleRegistersAck(respPDU, writePt.AddrOrZero(), len(regs)) {
		fmt.Printf("Запись времени не подтверждена прибором: ответ % X\n", respPDU)
		os.Exit(1)
	}

	select {
	case <-ctx.Done():
		fmt.Printf("Контрольное чтение отменено: %v\n", ctx.Err())
		os.Exit(1)
	case <-time.After(500 * time.Millisecond):
	}

	afterDeviceTime, afterMidpoint, err := readVZLETClockForSetTime(ctx, reader, readPt, p.Codec.WordOrder32)
	if err != nil {
		fmt.Printf("Время было записано, но контрольное чтение не удалось: %v\n", err)
		os.Exit(1)
	}
	afterDrift := afterDeviceTime.Sub(afterMidpoint)
	fmt.Printf("Контрольное время прибора: %s\n", afterDeviceTime.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("Контрольное расхождение:   %.3f сек\n", afterDrift.Seconds())
	if math.Abs(afterDrift.Seconds()) > setTimeVerifyTolerance.Seconds() {
		fmt.Printf("ОШИБКА: запись подтверждена Modbus-ответом, но часы прибора после записи расходятся с сервером больше чем на %.0f сек.\n", setTimeVerifyTolerance.Seconds())
		os.Exit(1)
	}

	fmt.Printf("Установка выполнена и проверена: время прибора установлено примерно на %s\n", writeTime.Local().Format("2006-01-02 15:04:05"))
}

func readVZLETClockForSetTime(ctx context.Context, reader *pollcore.Reader, readPt *profile.Point, wordOrder32 string) (time.Time, time.Time, error) {
	before := time.Now()
	readData, err := reader.ReadRaw(ctx, readPt.Space, readPt.AddrOrZero(), readPt.Type)
	after := time.Now()
	midpoint := before.Add(after.Sub(before) / 2)
	if err != nil {
		return time.Time{}, midpoint, err
	}

	rawTime, err := codec.DecodeUint32(readData, wordOrder32)
	if err != nil {
		return time.Time{}, midpoint, err
	}
	if rawTime == 0 || rawTime == 0xFFFFFFFF {
		return time.Time{}, midpoint, fmt.Errorf("некорректное значение часов 0x%08X", rawTime)
	}
	return time.Unix(int64(rawTime), 0).UTC(), midpoint, nil
}

func sameWriteMultipleRegistersAck(resp []byte, addr, qty int) bool {
	if len(resp) < 5 || resp[0] != 0x10 || addr < 0 || addr > 0xFFFF || qty <= 0 || qty > 0x7B {
		return false
	}
	return binary.BigEndian.Uint16(resp[1:3]) == uint16(addr) &&
		binary.BigEndian.Uint16(resp[3:5]) == uint16(qty)
}
