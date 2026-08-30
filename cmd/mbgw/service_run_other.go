//go:build !windows

package main

// runServerAsWindowsServiceIfApplicable — заглушка для сборок не под
// Windows (сам проект развёрнут только под Windows, но main.go не
// должен переставать собираться на других системах просто из-за этой
// функции). Всегда возвращает nil — обычный ручной запуск (Ctrl+C)
// работает без единого изменения поведения.
func runServerAsWindowsServiceIfApplicable() <-chan struct{} {
	return nil
}
