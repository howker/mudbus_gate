//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// installService registers mbgw as a Windows Service that launches
// `mbgw.exe server` (the single-process, DB-driven mode — see
// cmd/mbgw/server.go) automatically on every Windows boot, with no
// console window and no manually-typed command ever again. This is the
// intended production launch method — `mbgw server --db ... --port ...`
// typed by hand in PowerShell is for development/testing only.
//
// --db and --port (same flags cmd/mbgw/server.go itself accepts) can be
// passed to `mbgw install-service` and are baked into the service
// definition ONCE, at install time — e.g.:
//
//	mbgw.exe install-service --port 9090
//
// If omitted, server's own defaults apply (mbgw_server.db, port 8080).
// Re-running install-service after uninstalling replaces the service
// with a new binPath, so changing the port later just means uninstall +
// reinstall with a new flag, not editing a config file by hand.
//
// Must be run from an elevated (Administrator) command prompt — sc.exe
// create requires it; a clear message below explains this if it fails.
func installService() {
	fmt.Println("=== установка службы Windows ===")

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
		fmt.Printf("ошибка получения пути к файлу: %v\n", err)
		return
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		fmt.Printf("ошибка формирования абсолютного пути: %v\n", err)
		return
	}

	// Full command line, baked into the service definition once — this is
	// what makes "mbgw server --db mbgw_server.db --port 8080" something
	// nobody ever has to type by hand again after this install step.
	binPath := fmt.Sprintf(`"%s" server --db "%s" --port %s`, exePath, dbPath, port)

	// sc.exe create mbgw_service, auto-start on boot, no console window
	// (Windows Services never show one regardless).
	cmd := exec.Command("sc", "create", "mbgw_service", "binPath=", binPath, "start=", "auto", "DisplayName=", "Modbus Gateway (mbgw)")

	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("ошибка регистрации службы: %v\nвывод системы: %s\n", err, string(output))
		fmt.Println("\nПодсказка: для установки службы командную строку нужно запускать от имени администратора!")
		return
	}

	fmt.Println("[OK] Служба 'mbgw_service' успешно создана.")
	fmt.Printf("[INFO] Команда запуска: %s\n", binPath)
	fmt.Println("[INFO] Запустить сейчас: sc start mbgw_service")
	fmt.Println("[INFO] Или через 'Службы' Windows (services.msc) — там же можно посмотреть статус, остановить, настроить автозапуск.")
	fmt.Println("[INFO] После запуска адрес веб-интерфейса появится в файле mbgw_web_address.txt рядом с exe")
	fmt.Println("[INFO]   (на случай, если настроенный порт окажется занят — сервер сам подберёт свободный и запишет реальный адрес туда).")
	fmt.Println("[INFO] Логи службы пишутся в mbgw_server.log рядом с exe (тот же файл, что и при обычном запуске).")
}
