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
// Usage:
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
		log.Fatalf("[FATAL] es-sync: %v", err)
	}
	if dryRun {
		cfg.DryRun = true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitForShutdownSignal(cancel, "es-sync")

	log.Printf("es-sync: старт, исходная БД %s, конфиг %s\n", dbPath, configPath)
	if err := integration.RunEnergosphereSync(ctx, dbPath, cfg); err != nil {
		log.Fatalf("[FATAL] es-sync: %v", err)
	}
}

func printESSyncUsage() {
	fmt.Println("Usage:")
	fmt.Println("  mbgw es-sync --db <mbgw_vkm.db> [--config <es_sync.txt>] [--dry-run]")
	fmt.Println()
	fmt.Println("  Пишет 4 величины (тепло, масса, температура, давление) из архива")
	fmt.Println("  ВКМ-360 напрямую в таблицу Mains базы Энергосферы, минуя драйверы ЭС.")
	fmt.Println("  Параметры подключения к SQL Server и множители единиц — в файле --config")
	fmt.Println("  (пароль хранится только там, не в коде и не в командной строке).")
	fmt.Println()
	fmt.Println("  --dry-run  ничего не пишет в БД ЭС, только логирует, что было бы записано.")
}
