package main

import (
    "fmt"
    "log"
    "os"

    "mbgw/internal/simulator"
)

func simulate() {
    if len(os.Args) < 3 {
        fmt.Println("Usage: mbgw simulate <device> [--addr host:port]")
        os.Exit(1)
    }

    device := os.Args[2]
    addr := "127.0.0.1:15020"

    for i := 3; i < len(os.Args)-1; i++ {
        if os.Args[i] == "--addr" {
            addr = os.Args[i+1]
        }
    }

    fmt.Printf("Starting simulator for %s on %s...\n", device, addr)
    if err := simulator.Run(device, addr); err != nil {
        log.Fatalf("simulator error: %v", err)
    }
}