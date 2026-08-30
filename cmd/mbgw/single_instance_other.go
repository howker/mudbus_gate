//go:build !windows

package main

// acquireSingleInstanceLock — заглушка для сборок не под Windows (сам
// проект развёрнут только под Windows, но main.go не должен переставать
// собираться на других системах просто из-за этой функции). Всегда
// возвращает ok=true — защита от двойного запуска реализована только
// для Windows, где она и нужна.
func acquireSingleInstanceLock() (bool, error) {
	return true, nil
}
