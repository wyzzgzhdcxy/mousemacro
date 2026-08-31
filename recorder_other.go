//go:build !windows

// recorder_other.go — 非 Windows 平台上的录制器占位实现。

package main

import (
	"context"
	"errors"
)

func RecorderStart(ctx context.Context, onState, onLog func(string), onComplete func([]Step)) error {
	return errors.New("宏录制仅在 Windows 上可用")
}

func RecorderStop() {}

func RecorderState() string { return "idle" }
