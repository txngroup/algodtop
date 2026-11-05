//go:build windows

package tui

import (
	"os"
	"syscall"
)

// Windows has no SIGWINCH, so the UI checks the size before each redraw.
func watchResize() (<-chan struct{}, func()) { return nil, func() {} }

const enableVirtualTerminalProcessing = 0x0004

// enableVT turns on ANSI escape handling for the older console host.
// Windows Terminal handles escapes natively.
func enableVT(f *os.File) {
	h := syscall.Handle(f.Fd())
	var mode uint32
	if syscall.GetConsoleMode(h, &mode) != nil {
		return
	}
	k := syscall.NewLazyDLL("kernel32.dll")
	k.NewProc("SetConsoleMode").Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
}
