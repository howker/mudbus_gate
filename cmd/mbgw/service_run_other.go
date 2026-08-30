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

// windowsServiceStatus — заглушка, повторяющая форму настоящей версии
// (см. service_run_windows.go), чтобы internal/web мог ссылаться на
// один и тот же тип независимо от платформы сборки.
type windowsServiceStatus struct {
	Installed bool
	State     string
}

// queryWindowsServiceStatus — заглушка для не-Windows сборок. Installed
// всегда false — на этой платформе понятия "служба Windows" в принципе
// не существует, вкладка «Служба» в UI покажет это через отдельное
// поле GOOS, а не будет путать оператора несуществующим статусом.
func queryWindowsServiceStatus() (windowsServiceStatus, error) {
	return windowsServiceStatus{Installed: false}, nil
}

// stopSelfAsWindowsService — заглушка для не-Windows сборок, никогда не
// вызывается в реальности (см. api_service.go — выбор между "остановить
// как службу" и "остановить как обычный процесс" делается на основании
// runtime.GOOS/IsWindowsService, до вызова этой функции).
func stopSelfAsWindowsService() error {
	return nil
}

// isRunningAsWindowsService — заглушка для не-Windows сборок, всегда
// false: понятия "служба Windows" на этой платформе не существует.
func isRunningAsWindowsService() bool {
	return false
}
