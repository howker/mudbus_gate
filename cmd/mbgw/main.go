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
		fmt.Printf("Неизвестная команда: %s\n", cmd)
		printHelp()
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Println("МодбасШлюз (mbgw)")
	fmt.Println("Использование: mbgw <команда> [параметры]")
	fmt.Println("Команды:")
	fmt.Println("  run              Запустить опрос приборов")
	fmt.Println("  serve            Запустить опрос + северный интерфейс Акрона в одном процессе")
	fmt.Println("  server           Основной единый сервер: приборы и точки ЭС берутся из БД")
	fmt.Println("  cli              Выполнить служебную CLI-команду")
	fmt.Println("  simulate         Запустить симулятор прибора")
	fmt.Println("  northbound       Запустить северный Modbus TCP интерфейс")
	fmt.Println("  es-sync          Запустить прямую передачу данных в БД Энергосферы")
	fmt.Println("  install-service  Создать или обновить службу Windows «МодбасШлюз»")
}
