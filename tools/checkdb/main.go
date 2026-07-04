package main

import (
    "context"
    "fmt"
    "os"

    sqliterepo "mbgw/internal/storage/sqlite"
)

func main() {
    path := "storage.json"
    if len(os.Args) > 1 {
        path = os.Args[1]
    }

    repo, err := sqliterepo.New(path)
    if err != nil {
        fmt.Printf("open error: %v\n", err)
        os.Exit(1)
    }

    readings, err := repo.GetLatestReadings(context.Background(), "")
    if err != nil {
        fmt.Printf("query error: %v\n", err)
        os.Exit(1)
    }

    fmt.Printf("%-10s %-25s %-8s %-15s %-10s %s\n", "DEVICE", "POINT", "INSTANCE", "VALUE", "QUALITY", "REASON")
    for _, r := range readings {
        fmt.Printf("%-10s %-25s %-8s %-15v %-10s %s\n", r.DeviceID, r.PointID, r.Instance, r.Value, r.Quality, r.QualityReason)
    }
}