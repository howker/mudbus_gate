package main
import (
    "context"
    "fmt"
    "os"
    "mbgw/internal/config"
    sqliterepo "mbgw/internal/storage/sqlite"
)
func cli() {
    if len(os.Args) < 3 {
        fmt.Println("Usage: mbgw cli <command>")
        fmt.Println("Available commands: show-current, correct-time")
        os.Exit(1)
    }
    cmd := os.Args[2]
    switch cmd {
    case "show-current":
        showCurrent()
    case "correct-time":
        correctTime()
    default:
        fmt.Printf("Unknown CLI command: %s\n", cmd)
        os.Exit(1)
    }
}
func showCurrent() {
    cfg, err := config.Load("config.yaml")
    if err != nil {
        fmt.Printf("Failed to load config: %v\n", err)
        os.Exit(1)
    }
    repo, err := sqliterepo.New(cfg.App.StoragePath)
    if err != nil {
        fmt.Printf("Failed to open database: %v\n", err)
        os.Exit(1)
    }
    readings, err := repo.GetLatestReadings(context.Background(), "")
    if err != nil {
        fmt.Printf("Failed to query database: %v\n", err)
        os.Exit(1)
    }
    fmt.Println("========================================================================================")
    fmt.Printf("%-10s | %-25s | %-8s | %-15s | %-8s | %s\n", "DEVICE ID", "POINT ID", "INSTANCE", "VALUE", "QUALITY", "UNIT")
    fmt.Println("========================================================================================")
    if len(readings) == 0 {
        fmt.Println("No data found in storage.")
    } else {
        for _, r := range readings {
            fmt.Printf("%-10s | %-25s | %-8s | %-15v | %-8s | %s\n", r.DeviceID, r.PointID, r.Instance, r.Value, r.Quality, r.Unit)
        }
    }
    fmt.Println("========================================================================================")
}