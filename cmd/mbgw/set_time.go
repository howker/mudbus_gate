package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
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

// setTime writes the current system time to a VZLET device's HR 0x8000
// "time set" register (function 16, standard Modbus - see
// mb_reg_ivk_ter.pdf / modbus_regs_tsrv_024m.pdf). Unlike Merkuriy's
// correct-time, VZLET has no documented +/-4min "correction" mode
// distinct from a full set - there is just one write register, so this
// command always performs a full set (still gated behind confirmation,
// since it is still a write to a live metering device).
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
		fmt.Printf("set-time сейчас поддержана только для приборов на базовом Modbus (протокол профиля: %q). Для Меркурия используйте correct-time.\n", p.Meta.Protocol)
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

	isTCP := devCfg.Transport.Kind == "modbus_tcp"
	trParams := transport.Params{
		Kind:            transport.Kind(devCfg.Transport.Kind),
		Host:            devCfg.Transport.Host,
		Port:            devCfg.Transport.Port,
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

	reader := pollcore.New(tr, isTCP, 1)

	readData, err := reader.ReadRaw(ctx, readPt.Space, readPt.AddrOrZero(), readPt.Type)
	if err != nil {
		fmt.Printf("Ошибка чтения времени прибора: %v\n", err)
		os.Exit(1)
	}
	rawTime, err := codec.DecodeUint32(readData, p.Codec.WordOrder32)
	if err != nil {
		fmt.Printf("Ошибка разбора времени прибора: %v\n", err)
		os.Exit(1)
	}
	deviceTime := time.Unix(int64(rawTime), 0).Local()

	systemTime := time.Now()
	drift := systemTime.Sub(deviceTime)

	fmt.Printf("Прибор:        %s\n", deviceID)
	fmt.Printf("Время прибора: %s\n", deviceTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("Время системы: %s\n", systemTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("Расхождение:   %v\n", drift)

	if drift.Abs() < time.Second {
		fmt.Println("\nВремя прибора уже синхронизировано (расхождение < 1с), установка не требуется.")
		return
	}

	if !autoYes {
		fmt.Print("\nВыполнить установку времени прибора? [y/N]: ")
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

	if _, err := reader.Transact(ctx, reqPDU); err != nil {
		fmt.Printf("Ошибка записи времени: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Установка выполнена: время прибора установлено на %s\n", writeTime.Format("2006-01-02 15:04:05"))
}
