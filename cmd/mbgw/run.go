package main

import (
    "context"
    "io"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "mbgw/internal/client"
    "mbgw/internal/config"
    "mbgw/internal/device"
    "mbgw/internal/profile"
    "mbgw/internal/session"
    sqliterepo "mbgw/internal/storage/sqlite"
    "mbgw/internal/transport"
    "mbgw/internal/web"
)

type loopbackSimulator struct{}

func (l *loopbackSimulator) Read(ctx context.Context, req []byte) ([]byte, error) {
    time.Sleep(50 * time.Millisecond)
    txIDHi, txIDLo := req[0], req[1]
    return []byte{txIDHi, txIDLo, 0x00, 0x00, 0x00, 0x07, 0x01, 0x04, 0x04, 0x42, 0xF6, 0xE9, 0xD5}, nil
}

func (l *loopbackSimulator) Close() error { return nil }

func run() {
    logFile, _ := os.OpenFile("mbgw.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
    defer logFile.Close()
    multiWriter := io.MultiWriter(os.Stdout, logFile)
    log.SetOutput(multiWriter)
    log.SetFlags(log.Ldate | log.Ltime)

    log.Println("=== запуск шлюза mbgw ===")

    cfg, err := config.Load("config.yaml")
    if err != nil {
        log.Fatalf("[FATAL] ошибка конфигурации: %v", err)
    }

    repo, err := sqliterepo.New(cfg.App.StoragePath)
    if err != nil {
        log.Fatalf("[FATAL] ошибка хранилища: %v", err)
    }
    if err := repo.InitSchema(context.Background()); err != nil {
        log.Fatalf("[FATAL] ошибка инициализации схемы: %v", err)
    }
    log.Printf("[OK] Хранилище инициализировано (%s)\n", cfg.App.StoragePath)

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    port := cfg.App.WebPort
    if port == 0 {
        port = 8080
    }
    webServer := web.NewServer(repo, port)
    go webServer.Start(ctx)

    for _, devCfg := range cfg.Devices {
        p, err := profile.Parse(devCfg.Profile)
        if err != nil {
            log.Printf("[ERROR] прибор %s: ошибка профиля %s: %v\n", devCfg.ID, devCfg.Profile, err)
            continue
        }

        sess, err := session.NewFromProfile(p.Session)
        if err != nil {
            log.Printf("[ERROR] неизвестный тип сессии %s: %v\n", p.Session.Type, err)
            continue
        }

        trParams := transport.Params{
            Kind:            transport.Kind(devCfg.Transport.Kind),
            Host:            devCfg.Transport.Host,
            Port:            devCfg.Transport.Port,
            ResponseTimeout: time.Duration(devCfg.Transport.TimeoutMs) * time.Millisecond,
        }
        tr, err := transport.New(trParams)
        if err != nil {
            log.Printf("[WARN] прибор %s недоступен. Симулятор.\n", devCfg.ID)
            tr = &loopbackSimulator{}
        }

        if err := sess.Open(ctx, tr); err != nil {
            log.Printf("[ERROR] ошибка сессии %s: %v\n", devCfg.ID, err)
            continue
        }

        isTCP := devCfg.Transport.Kind == "modbus_tcp"
        modbusClient := client.NewModbus(tr, isTCP, 1)
        dev := device.New(devCfg.ID, p, modbusClient, sess, repo)
        go dev.Start(ctx, 3*time.Second, 1*time.Hour)
    }

    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
    <-sigChan
    log.Println("=== получен сигнал завершения. остановка... ===")
}