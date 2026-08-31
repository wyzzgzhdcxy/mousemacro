//go:build windows

// runhotkey_windows.go — 常驻的 F12 全局热键,用于切换"开始执行 / 停止执行"。
//
// 与 recorder 的 F8 钩子区别:F12 钩子从 app 启动时安装,一直运行到 app 退出,
// 跟"录制"功能是否启用无关。

package main

import (
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const vkF12 = 0x7B

var (
	runHotkeyAbort      chan struct{}
	runHotkeyAbortOnce  sync.Once
	runHotkeyThreadId   atomic.Uint32
	runHotkeyToggleFunc func() // 由 App.startup 设置为 a.ToggleRunFromHotkey
)

func runHotkeySetToggleFunc(fn func()) { runHotkeyToggleFunc = fn }

func runHotkeyStart() {
	runHotkeyAbort = make(chan struct{})
	runHotkeyAbortOnce = sync.Once{}
	runHotkeyThreadId.Store(0)
	go runHotkeyThread()
}

func runHotkeyThread() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	tid, _, _ := procGetCurrentThreadId.Call()
	runHotkeyThreadId.Store(uint32(tid))

	hookProc := syscall.NewCallback(func(nCode, wparam, lparam uintptr) uintptr {
		if nCode >= 0 && (wparam == wmKeydown || wparam == wmSyskeydown) {
			kb := (*kbdllhookstruct)(unsafe.Pointer(lparam))
			if kb.VkCode == vkF12 {
				// 派发到独立 goroutine,避免阻塞钩子回调
				go func() {
					if runHotkeyToggleFunc != nil {
						runHotkeyToggleFunc()
					}
				}()
				return 1 // 吞掉 F12,不让它继续传到前台应用
			}
		}
		r1, _, _ := procCallNextHookEx.Call(0, nCode, wparam, lparam)
		return r1
	})

	hook, _, _ := procSetWindowsHookExW.Call(whKeyboardLl, hookProc, 0, 0)
	if hook == 0 {
		return
	}
	defer procUnhookWindowsHookEx.Call(hook)

	var msg winmsg
	for {
		r1, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if r1 == 0 || r1 == ^uintptr(0) {
			break // WM_QUIT 或错误
		}
		select {
		case <-runHotkeyAbort:
			goto done
		default:
		}
	}
done:
}

func runHotkeyStop() {
	runHotkeyAbortOnce.Do(func() {
		if runHotkeyAbort != nil {
			close(runHotkeyAbort)
		}
	})
	tid := runHotkeyThreadId.Load()
	if tid != 0 {
		procPostThreadMessageW.Call(uintptr(tid), wmQuit, 0, 0)
	}
}
