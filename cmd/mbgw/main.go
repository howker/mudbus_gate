package main

import (
"fmt"
"os"
)

func main() {
if len(os.Args) < 2 {
printHelp()
os.Exit(1)
}

cmd := os.Args[1]
switch cmd {
case "run":
run()
case "cli":
cli()
case "simulate":
simulate()
case "install-service":
installService()
case "--help", "-h", "help":
printHelp()
default:
fmt.Printf("Unknown command: %s\n", cmd)
printHelp()
os.Exit(1)
}
}

func printHelp() {
fmt.Println("Modbus-Shlyuz (mbgw)")
fmt.Println("Usage: mbgw <command> [args]")
fmt.Println("Commands:")
fmt.Println("  run              Start the gateway server")
fmt.Println("  cli              Run CLI command")
fmt.Println("  simulate         Run device simulator")
fmt.Println("  install-service  Install as OS service")
}
