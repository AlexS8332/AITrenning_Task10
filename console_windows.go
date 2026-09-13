//go:build windows

package main

import "syscall"

// enableUTF8Console переводит консоль в UTF-8. PowerShell 5.1 по умолчанию
// работает в cp866, и без этого кириллица в логах сервера превращается
// в мусор.
func enableUTF8Console() {
	const codePageUTF8 = 65001

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	kernel32.NewProc("SetConsoleOutputCP").Call(uintptr(codePageUTF8))
	kernel32.NewProc("SetConsoleCP").Call(uintptr(codePageUTF8))
}
