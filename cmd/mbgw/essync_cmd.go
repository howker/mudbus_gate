package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"mbgw/internal/integration"
)

// runESSync is the `mbgw es-sync` subcommand: push the four heat-metering
// values from mbgw_vkm.db straight into the Энергосфера Mains table,
// bypassing the ЭС device drivers. See internal/integration/
// energosphere_sync.go for the full why and internal/integration/extdb.go
// for the SQL Server client (the only place that imports the mssql
// driver, per LLD.md's dependency rules).
//
// Использование:
//
//	mbgw es-sync --db mbgw_vkm.db [--config es_sync.txt] [--dry-run]
//
// The SQL Server connection (including password) and unit-conversion
// factors live in the --config text file (default es_sync.txt next to the
// exe), never on the command line or in code — see integration.LoadConfig.
func runESSync() {
	dbPath := "mbgw_vkm.db"
	configPath := "es_sync.txt"
	dryRun := false

	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--db":
			if i+1 < len(os.Args) {
				dbPath = os.Args[i+1]
				i++
			}
		case "--config":
			if i+1 < len(os.Args) {
				configPath = os.Args[i+1]
				i++
			}
		case "--dry-run":
			dryRun = true
		case "--help", "-h":
			printESSyncUsage()
			os.Exit(0)
		}
	}

	cfg, err := integration.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("[КРИТИЧНО] синхронизация с ЭС: %v", err)
	}
	if dryRun {
		cfg.DryRun = true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "синхронизация с ЭС")

	log.Printf("синхронизация с ЭС: старт, исходная БД %s, конфигурация %s\n", dbPath, configPath)
	// nil — в этом отдельном режиме запуска (устаревшая схема "четыре
	// окна", без единого веб-сервера) нет ни кнопки "Синхронизировать
	// сейчас", ни принудительного переопроса, которые могли бы попросить
	// внеплановый проход — канал-триггер просто некому подключать. nil
	// безопасен: select на nil-канале никогда не срабатывает, обычный
	// тикер и ctx.Done() продолжают работать как раньше.
	if err := integration.RunEnergosphereSync(ctx, dbPath, cfg, nil); err != nil {
		log.Fatalf("[КРИТИЧНО] синхронизация с ЭС: %v", err)
	}
}

func printESSyncUsage() {
	fmt.Println("Использование:")
	fmt.Println("  mbgw es-sync --db <mbgw_vkm.db> [--config <es_sync.txt>] [--dry-run]")
	fmt.Println()
	fmt.Println("  Пишет 4 величины (тепло, масса, температура, давление) из архива")
	fmt.Println("  ВКМ-360 напрямую в таблицу Mains базы Энергосферы, минуя драйверы ЭС.")
	fmt.Println("  Параметры подключения к SQL Server и множители единиц — в файле --config")
	fmt.Println("  (пароль хранится только там, не в коде и не в командной строке).")
	fmt.Println()
	fmt.Println("  --dry-run  ничего не пишет в БД ЭС, только логирует, что было бы записано.")
}
