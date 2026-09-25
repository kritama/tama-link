//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func detectStdinInteractive() bool {
	var mode uint32
	err := windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode)
	return err == nil
}
