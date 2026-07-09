package main

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"mbgw/internal/config"
	"mbgw/internal/pollcore"
	"mbgw/internal/profile"
	"mbgw/internal/protocol/merkuriy"
	"mbgw/internal/session"
	"mbgw/internal/transport"
)

// correctTimeMaxDrift is the documented limit for command 0x0D
// (Коррекция времени, section 3.11): +/-4 minutes per day. This CLI
// command only ever performs a correction, never a full time set
// (BuildSetTime) - a full set is a higher-risk operation (requires
// access level 2, changes date/dow/dst too) deliberately left out of
// this automated path; if the drift exceeds the correction limit, the
// command refuses and asks for manual intervention rather than silently
// escalating to a bigger write.
const correctTimeMaxDrift = 4 * time.Minute

func correctTime() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: mbgw cli correct-time <device_id> [--yes]")
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
	if p.Meta.Protocol != "merkuriy" {
		fmt.Printf("Коррекция времени сейчас поддержана только для приборов Меркурий (протокол профиля: %q)\n", p.Meta.Protocol)
		os.Exit(1)
	}

	addrRaw, ok := p.Session.Params["addr"]
	if !ok {
		fmt.Println("В профиле не задан session.params.addr (сетевой адрес Меркурия)")
		os.Exit(1)
	}
	addr := byte(toInt(addrRaw))

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

	sess, err := session.NewFromProfile(p.Session)
	if err != nil {
		fmt.Printf("Ошибка создания сессии: %v\n", err)
		os.Exit(1)
	}
	if err := sess.Open(ctx, tr); err != nil {
		fmt.Printf("Ошибка открытия канала: %v\n", err)
		os.Exit(1)
	}
	defer sess.Close()

	reader := pollcore.New(tr, false, addr)

	readData, err := reader.Transact(ctx, merkuriy.BuildReadCurrentTimePDU())
	if err != nil {
		fmt.Printf("Ошибка чтения времени прибора: %v\n", err)
		os.Exit(1)
	}
	deviceTime, dow, isWinter, err := merkuriy.ParseCurrentTimeData(readData)
	if err != nil {
		fmt.Printf("Ошибка разбора времени прибора: %v\n", err)
		os.Exit(1)
	}

	systemTime := time.Now()
	drift := systemTime.Sub(deviceTime)

	fmt.Printf("Прибор:        %s\n", deviceID)
	fmt.Printf("Время прибора: %s (день недели=%d, %s)\n", deviceTime.Format("2006-01-02 15:04:05"), dow, winterSummer(isWinter))
	fmt.Printf("Время системы: %s\n", systemTime.Format("2006-01-02 15:04:05"))
	fmt.Printf("Расхождение:   %v\n", drift)

	if math.Abs(drift.Minutes()) > correctTimeMaxDrift.Minutes() {
		fmt.Printf("\nРасхождение превышает документированный лимит коррекции (+/-%v/сутки).\n", correctTimeMaxDrift)
		fmt.Println("Автоматическая коррекция отклонена. Для установки времени вне этого диапазона нужна полная установка времени (BuildSetTime) - выполните её осознанно, отдельно, эта команда её не делает.")
		os.Exit(1)
	}

	if drift.Abs() < time.Second {
		fmt.Println("\nВремя прибора уже синхронизировано (расхождение < 1с), коррекция не требуется.")
		return
	}

	if !autoYes {
		fmt.Print("\nВыполнить коррекцию времени прибора? [y/N]: ")
		stdin := bufio.NewReader(os.Stdin)
		answer, _ := stdin.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(answer)) != "y" {
			fmt.Println("Отменено.")
			return
		}
	}

	correctionTime := time.Now()
	pdu, err := merkuriy.BuildCorrectTimePDU(correctionTime)
	if err != nil {
		fmt.Printf("Ошибка построения запроса коррекции: %v\n", err)
		os.Exit(1)
	}

	respData, err := reader.Transact(ctx, pdu)
	if err != nil {
		fmt.Printf("Ошибка отправки коррекции: %v\n", err)
		os.Exit(1)
	}
	if len(respData) < 1 || !merkuriy.IsOK(respData[0]) {
		status := "неизвестна"
		if len(respData) >= 1 {
			status = merkuriy.ParseStatus(respData[0]).String()
		}
		fmt.Printf("Прибор отклонил коррекцию времени: статус %s\n", status)
		os.Exit(1)
	}

	fmt.Printf("Коррекция выполнена: время прибора установлено на %s\n", correctionTime.Format("2006-01-02 15:04:05"))
}

func winterSummer(isWinter bool) string {
	if isWinter {
		return "зима"
	}
	return "лето"
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	default:
		return 0
	}
}
