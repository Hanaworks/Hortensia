package service

import (
	"golang.org/x/sys/windows"
	"os"
)

func disableEcho(file *os.File) (func() error, error) {
	h := windows.Handle(file.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return nil, err
	}
	if err := windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		return nil, err
	}
	return func() error { return windows.SetConsoleMode(h, mode) }, nil
}
