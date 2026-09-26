//go:build darwin

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func detectStdinInteractive() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TIOCGETA)
	return err == nil
}
