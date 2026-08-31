//go:build !windows

// wininput_other.go — 非 Windows 平台的占位实现。
// 当前工程仅在 Windows 上提供真正的鼠标键盘模拟。

package main

import "fmt"

func winGetCursorPos() (int, int) { return 0, 0 }

func runStepOnWindows(step Step, abort *atomicBool) error {
	return fmt.Errorf("mouse/keyboard simulation 仅在 Windows 上可用,当前为非 Windows 平台")
}

func winMoveWindowsByTitle(keyword string, x, y int) ([]string, error) {
	return nil, fmt.Errorf("窗口操作仅在 Windows 上可用,当前为非 Windows 平台")
}
