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
	case "serve":
		serve()
	case "server":
		runServer()
	case "cli":
		cli()
	case "simulate":
		simulate()
	case "northbound":
		runNorthbound()
	case "es-sync":
		runESSync()
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
	fmt.Println("  run              Start the gateway server (southbound only)")
	fmt.Println("  serve            Start southbound + northbound Akron carrier in one process")
	fmt.Println("                   (needs northbound_akron: section in --config; see config.akron_real.yaml)")
	fmt.Println("  server           Single-process server, device list + ES channels from the DB")
	fmt.Println("                   (mbgw server --db mbgw_server.db) — replaces the 4-window setup")
	fmt.Println("  cli              Run CLI command")
	fmt.Println("  simulate         Run device simulator")
	fmt.Println("  northbound       Start the northbound Modbus TCP slave")
	fmt.Println("  es-sync          Push heat/mass/temp/pressure straight into the Энергосфера DB")
	fmt.Println("                   (bypasses ЭС device drivers; see 'mbgw es-sync --help')")
	fmt.Println("  install-service  Install as OS service")
}
