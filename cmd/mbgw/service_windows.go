//go:build windows

package main

import (
"fmt"
"os"
"os/exec"
"path/filepath"
)

func installService() {
fmt.Println("=== становка службы Windows ===")

// 1. олучаем полный абсолютный путь к нашему mbgw.exe
exePath, err := os.Executable()
if err != nil {
fmt.Printf("шибка получения пути к файлу: %v\n", err)
return
}
exePath, err = filepath.Abs(exePath)
if err != nil {
fmt.Printf("шибка формирования абсолютного пути: %v\n", err)
return
}

// 2. ормируем команду для запуска в режиме run
binPath := fmt.Sprintf(`"%s" run`, exePath)

// 3. ызываем встроенную системную утилиту sc.exe для создания службы
// мя службы: mbgw_service
// start= auto означает автоматический запуск при старте Windows
cmd := exec.Command("sc", "create", "mbgw_service", "binPath=", binPath, "start=", "auto", "DisplayName=", "Modbus Gateway (mbgw)")

output, err := cmd.CombinedOutput()
if err != nil {
fmt.Printf("шибка регистрации службы: %v\nывод системы: %s\n", err, string(output))
fmt.Println("\n: ля установки службы командную строку нужно запускать от имени дминистратора!")
return
}

fmt.Println("[OK] Служба 'mbgw_service' успешно создана!")
fmt.Println("[INFO] Теперь вы можете запустить ее в 'Службах' Windows (services.msc) или командой: sc start mbgw_service")
}
