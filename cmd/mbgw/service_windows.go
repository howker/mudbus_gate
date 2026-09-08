//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// installService создаёт ИЛИ обновляет службу mbgw_service. Команду можно
// безопасно запускать повторно после замены exe: она не требует ручного
// удаления уже существующей службы.
func installService() {
	fmt.Println("=== настройка службы Windows «МодбасШлюз» ===")

	dbPath := "mbgw_server.db"
	port := "8080"
	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--db":
			if i+1 < len(os.Args) {
				dbPath = os.Args[i+1]
				i++
			}
		case "--port":
			if i+1 < len(os.Args) {
				port = os.Args[i+1]
				i++
			}
		}
	}

	exePath, err := os.Executable()
	if err != nil {
		fmt.Printf("ОШИБКА: не удалось определить путь к mbgw.exe: %v\n", err)
		return
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		fmt.Printf("ОШИБКА: не удалось получить абсолютный путь к mbgw.exe: %v\n", err)
		return
	}
	binPath := fmt.Sprintf(`"%s" server --db "%s" --port %s`, exePath, dbPath, port)

	exists := serviceExists()
	var args []string
	if exists {
		args = []string{"config", mbgwServiceName, "binPath=", binPath, "start=", "auto", "DisplayName=", "МодбасШлюз"}
		fmt.Println("Служба уже существует — обновляю параметры запуска.")
	} else {
		args = []string{"create", mbgwServiceName, "binPath=", binPath, "start=", "auto", "DisplayName=", "МодбасШлюз"}
	}
	if out, err := exec.Command("sc.exe", args...).CombinedOutput(); err != nil {
		fmt.Printf("ОШИБКА: не удалось настроить службу: %v\n%s\n", err, strings.TrimSpace(string(out)))
		fmt.Println("Запустите PowerShell или командную строку от имени администратора.")
		return
	}

	// Описание не влияет на работоспособность, но делает services.msc
	// понятнее оператору.
	_, _ = exec.Command("sc.exe", "description", mbgwServiceName,
		"МодбасШлюз: опрос приборов и передача данных в Энергосферу").CombinedOutput()

	// Recovery: три последовательных аварийных отказа -> рестарт через
	// 1, 2 и 5 минут; счётчик сбрасывается через сутки. Watchdog завершает
	// процесс с ошибкой только когда он действительно запущен службой.
	if out, err := exec.Command("sc.exe", "failure", mbgwServiceName,
		"reset=", "86400", "actions=", "restart/60000/restart/120000/restart/300000").CombinedOutput(); err != nil {
		fmt.Printf("ПРЕДУПРЕЖДЕНИЕ: служба настроена, но политику автоматического восстановления задать не удалось: %v\n%s\n",
			err, strings.TrimSpace(string(out)))
	} else {
		fmt.Println("ОК: автоматическое восстановление настроено (1, 2 и 5 минут).")
	}
	// На поддерживаемых версиях Windows это просит применять Recovery и
	// к ненулевому коду завершения процесса. Если команда недоступна на
	// конкретной старой системе, основная конфигурация службы остаётся рабочей.
	_, _ = exec.Command("sc.exe", "failureflag", mbgwServiceName, "1").CombinedOutput()

	fmt.Println("ОК: служба «МодбасШлюз» настроена на автоматический запуск.")
	fmt.Printf("Команда процесса: %s\n", binPath)
	fmt.Println("Запустить сейчас: C:\\Windows\\System32\\sc.exe start mbgw_service")
	fmt.Println("Основной лог: mbgw_server.log; журнал службы: mbgw_service_log.db — рядом с mbgw.exe.")
}

func serviceExists() bool {
	cmd := exec.Command("sc.exe", "query", mbgwServiceName)
	return cmd.Run() == nil
}
