package main

import (
	"context"
	"fmt"
	"mbgw/internal/config"
	sqliterepo "mbgw/internal/storage/sqlite"
	"os"
)

func cli() {
	if len(os.Args) < 3 {
		fmt.Println("Использование: mbgw cli <команда>")
		fmt.Println("Доступные команды: show-current, correct-time, set-time")
		os.Exit(1)
	}
	cmd := os.Args[2]
	switch cmd {
	case "show-current":
		showCurrent()
	case "correct-time":
		correctTime()
	case "set-time":
		setTime()
	default:
		fmt.Printf("Неизвестная CLI-команда: %s\n", cmd)
		os.Exit(1)
	}
}
func showCurrent() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		fmt.Printf("Не удалось загрузить конфигурацию: %v\n", err)
		os.Exit(1)
	}
	repo, err := sqliterepo.New(cfg.App.StoragePath)
	if err != nil {
		fmt.Printf("Не удалось открыть базу данных: %v\n", err)
		os.Exit(1)
	}
	readings, err := repo.GetLatestReadings(context.Background(), "")
	if err != nil {
		fmt.Printf("Не удалось прочитать данные из базы: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("========================================================================================")
	fmt.Printf("%-10s | %-25s | %-8s | %-15s | %-8s | %s\n", "ПРИБОР", "ТОЧКА", "ЭКЗЕМПЛЯР", "ЗНАЧЕНИЕ", "КАЧЕСТВО", "ЕД.")
	fmt.Println("========================================================================================")
	if len(readings) == 0 {
		fmt.Println("В хранилище нет данных.")
	} else {
		for _, r := range readings {
			fmt.Printf("%-10s | %-25s | %-8s | %-15v | %-8s | %s\n", r.DeviceID, r.PointID, r.Instance, r.Value, r.Quality, r.Unit)
		}
	}
	fmt.Println("========================================================================================")
}
