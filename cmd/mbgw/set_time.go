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
func setTime() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: mbgw cli set-time <device_id> [--yes]")
		os.Exit(1)
	}
	deviceID := os.Args[3]
	autoYes := false
	for _, a := range os.Args[4:] {
		if a == "--yes" || a == "-y" {
			autoYes = true
		}
	}

	cfg, err := config.Load("config.yaml")
	if err != nil {
		fmt.Printf("Ошибка загрузки конфигурации: %v\n", err)
		os.Exit(1)
	}

	var devCfg *config.DeviceConfig
	for i := range cfg.Devices {
		if cfg.Devices[i].ID == deviceID {
			devCfg = &cfg.Devices[i]
			break
		}
	}
	if devCfg == nil {
		fmt.Printf("Прибор %q не найден в config.yaml\n", deviceID)
		os.Exit(1)
	}

	p, err := profile.Parse(devCfg.Profile)
	if err != nil {
		fmt.Printf("Ошибка загрузки профиля: %v\n", err)
		os.Exit(1)
	}
	if p.Meta.Protocol != "modbus" {
		fmt.Printf("set-time поддержана только для приборов базового Modbus (протокол профиля: %q). Для Меркурия используйте correct-time.\n", p.Meta.Protocol)
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
		fmt.Println("В профиле не найдены точки \"current_time\" (чтение) и/или \"time_set\" (запись) - set-time для этого прибора не настроена.")
		os.Exit(1)
	}
	if readPt.Type != "uint32" || writePt.Type != "uint32" {
		fmt.Printf("Неподдерживаемый формат часов: current_time=%q, time_set=%q; ожидается uint32 Unix time.\n", readPt.Type, writePt.Type)
		os.Exit(1)
	}

	isTCP := devCfg.Transport.Kind == "modbus_tcp"
	trParams := transport.Params{
		Kind:            transport.Kind(devCfg.Transport.Kind),
		Host:            devCfg.Transport.Host,
		Port:            devCfg.Transport.Port,
		COM:             devCfg.Transport.COM,
		Baudrate:        devCfg.Transport.Baudrate,
		Parity:          devCfg.Transport.Parity,
		StopBits:        devCfg.Transport.StopBits,
		ResponseTimeout: time.Duration(devCfg.Transport.TimeoutMs) * time.Millisecond,
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

	unitID := devCfg.Transport.UnitID
	if unitID == 0 {
		unitID = 1
	}
	reader := pollcore.New(tr, isTCP, unitID)

	deviceTime, midpoint, err := readVZLETClockForSetTime(ctx, reader, readPt, p.Codec.WordOrder32)
	if err != nil {
		fmt.Printf("Ошибка чтения времени прибора: %v\n", err)
		os.Exit(1)
	}
	drift := deviceTime.Sub(midpoint)

	fmt.Printf("Прибор:         %s\n", deviceID)
	fmt.Printf("Modbus Unit ID: %d\n", unitID)
	fmt.Printf("Время прибора:  %s\n", deviceTime.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("Время системы:  %s\n", midpoint.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("Расхождение (прибор - система): %.3f сек\n", drift.Seconds())

	if math.Abs(drift.Seconds()) < 1 {
		fmt.Println("\nВремя прибора уже синхронизировано (|расхождение| < 1 с), установка не требуется.")
		return
	}

	fmt.Println("\nВНИМАНИЕ: это полная установка часов VZLET. Для ИВК-ТЭР прибор должен находиться в Service/Setup mode.")
	fmt.Println("Не выполняйте set-time параллельно с работающим опросом этого же физического прибора; на время команды остановите службу/опросчик.")
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
		fmt.Println("Для ИВК-ТЭР проверьте, что прибор переведён в Service/Setup mode.")
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
