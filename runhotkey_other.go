//go:build !windows

// runhotkey_other.go — 非 Windows 平台上的 F12 热键占位。

package main

func runHotkeyStart()                 {}
func runHotkeyStop()                  {}
func runHotkeySetToggleFunc(fn func()) {}
