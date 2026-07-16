package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"mbgw/internal/monitor"
	"mbgw/internal/northbound"
	sqliterepo "mbgw/internal/storage/sqlite"
)

// northbound starts the standalone Modbus TCP slave (northbound
// interface) described by --config, serving current values read from the
// SQLite database at --db. It is intentionally a separate process/command
// from `run` for now (see TASK_M1): `mbgw run` polls devices downward and
// writes readings; `mbgw northbound` reads the same database and serves
// them upward to Энергосфера. Both can point at the same --db and run
// side by side.
//
// --discovery is reserved for M3 (TRD addendum §B.4) and is not
// implemented yet — passing it exits with an explicit message rather than
// silently falling back to normal serving mode.
func runNorthbound() {
	if len(os.Args) < 3 {
		printNorthboundUsage()
		os.Exit(1)
	}

	var cfgPath, dbPath string
	discovery := false

	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--config":
			if i+1 < len(os.Args) {
				cfgPath = os.Args[i+1]
				i++
			}
		case "--db":
			if i+1 < len(os.Args) {
				dbPath = os.Args[i+1]
				i++
			}
		case "--discovery":
			discovery = true
		case "--help", "-h":
			printNorthboundUsage()
			os.Exit(0)
		}
	}

	if cfgPath == "" || dbPath == "" {
		printNorthboundUsage()
		os.Exit(1)
	}
	if discovery {
		fmt.Println("northbound: --discovery is not implemented yet (arrives with M3)")
		os.Exit(1)
	}

	uspd, err := northbound.LoadUSPDConfig(cfgPath)
	if err != nil {
		log.Fatalf("[FATAL] northbound: %v", err)
	}

	repo, err := sqliterepo.New(dbPath)
	if err != nil {
		log.Fatalf("[FATAL] northbound: ошибка хранилища: %v", err)
	}
	if err := repo.InitSchema(context.Background()); err != nil {
		log.Fatalf("[FATAL] northbound: ошибка инициализации схемы: %v", err)
	}

	eventBus := monitor.NewBus(nil)
	srv := northbound.NewServer(uspd, repo, eventBus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("northbound: получен сигнал завершения, останавливаемся...")
		cancel()
	}()

	log.Printf("northbound: слушаем %s как unit %d (устройство=%s, uspd=%s)\n",
		uspd.Listen, uspd.UnitID, uspd.DeviceID, uspd.ID)

	if err := srv.Listen(ctx); err != nil {
		log.Fatalf("[FATAL] northbound: %v", err)
	}
	log.Println("northbound: остановлен")
}

func printNorthboundUsage() {
	fmt.Println("Usage: mbgw northbound --config <uspd.yaml> --db <path.sqlite> [--discovery]")
	fmt.Println("  --config     путь к YAML-описанию логического УСПД (карта регистров наверх)")
	fmt.Println("  --db         путь к SQLite-базе mbgw (та же, что использует mbgw run)")
	fmt.Println("  --discovery  режим снятия протокола Энергосферы (M3, пока не реализован)")
}
