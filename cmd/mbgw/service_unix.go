//go:build !windows

package main

import "fmt"

func installService() {
	fmt.Println("Установка службы Unix (systemd)...")
	// TODO: implement systemd installation
}
