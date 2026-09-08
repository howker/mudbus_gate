//go:build !windows

package main

import "context"

type serverCoreFunc func(ctx context.Context, onReady func(), runningAsService bool) error

func runServerAsWindowsServiceIfApplicable(core serverCoreFunc) (bool, error) {
	return false, nil
}

type windowsServiceStatus struct {
	Installed bool
	State     string
}

func queryWindowsServiceStatus() (windowsServiceStatus, error) {
	return windowsServiceStatus{Installed: false}, nil
}
func stopSelfAsWindowsService() error { return nil }
func isRunningAsWindowsService() bool { return false }
