//go:build !windows

package main

import "fmt"

func installService() {
fmt.Println("Installing Unix service (systemd)...")
// TODO: implement systemd installation
}
