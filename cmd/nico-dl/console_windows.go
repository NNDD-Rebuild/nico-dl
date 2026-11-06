//go:build windows

package main

import "syscall"

// initConsole はコンソールの出力コードページを UTF-8 にする (日本語ログの文字化け防止)。
func initConsole() {
	k := syscall.NewLazyDLL("kernel32.dll")
	k.NewProc("SetConsoleOutputCP").Call(65001)
}
