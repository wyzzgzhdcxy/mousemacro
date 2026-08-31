//go:build windows

// recorder_windows.go — 宏录制器。
//
// 设计:
//   - 用 WH_MOUSE_LL + WH_KEYBOARD_LL 两个低级钩子捕获全屏输入。
//   - 钩子必须装在「持续泵消息」的 OS 线程上,所以在专用 goroutine 里 LockOSThread
//     并循环 GetMessageW,按 F8 切换 armed <-> recording 状态。
//   - F8-stop 时,回调里 PostThreadMessage(WM_QUIT) 让消息循环退出,
//     然后在 defer 里 UnhookWindowsHookEx + 汇总事件并通过事件发回前端。
//   - 录到的事件:鼠标左/右键 DOWN 视为 click,DBLCLK 视为 doubleclick;
//     键盘非修饰键 keydown 视为 keypress(带上当前按住的 shift/ctrl/alt 修饰)。
//     鼠标移动 / 滚轮不录(可由用户在脚本里手动加 move 步骤)。

package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// ===== Windows 钩子相关常量 =====

const (
	wmMouseMove     = 0x0200
	wmLButtonDown   = 0x0201
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonDown   = 0x0204
	wmRButtonUp     = 0x0205
	wmRButtonDblClk = 0x0206
	wmMButtonDown   = 0x0207
	wmMButtonUp     = 0x0208
	wmMouseWheel    = 0x020A

	wmQuit = 0x0012

	vkF8    = 0x77
	vkShift = 0x10
	vkCtrl  = 0x11
	vkAlt   = 0x12
	vkLWin  = 0x5B
	vkRWin  = 0x5C
)

// MSLLHOOKSTRUCT — 鼠标低级钩子回调的 lParam 指向此结构
type msllhookstruct struct {
	Pt          winPoint
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

// ===== 录制数据 =====

// recordedEvent 是录制阶段收集的原始事件,最终会被转换为 Step 推给前端。
type recordedEvent struct {
	Kind   string // click | doubleclick | keypress
	X, Y   int
	Button string // left | right
	Key    string // 形如 "a" / "ctrl+s" / "shift+F5"
	When   time.Time
}

// Recorder 状态机:
//   idle    → Start() → armed(等 F8) → F8 → recording → F8 → done(发回 steps) → idle
//   recording/armed 任意阶段 → Stop() → idle(丢弃已录事件)
type Recorder struct {
	mu        sync.Mutex
	events    []recordedEvent

	armed     atomicBool // 钩子已装,等 F8
	recording atomicBool // F8 已按下,正在录

	// 钩子线程退出信号
	stopCh   chan struct{}
	stopOnce sync.Once

	// 钩子线程 id(供 PostThreadMessage 用)
	threadId atomic.Uint32

	// 录制结果需要回到前端(在退出 goroutine 前通过事件发送)
	ctx        context.Context
	onComplete func([]Step)
	onLog      func(string)
	onState    func(state string) // "armed" | "recording"
}

var recorder = &Recorder{}

// isModifierKey 判断是否为修饰键(这些键不产生 keypress 步骤,只更新状态)
func isModifierKey(vk uint16) bool {
	switch vk {
	case vkShift, vkCtrl, vkAlt, vkLWin, vkRWin:
		return true
	}
	return false
}

// isToggleKey 判断是否为 toggle 类键(回放会反复切换,容易出问题,录制时直接跳过)
func isToggleKey(vk uint16) bool {
	switch vk {
	case 0x14, // VK_CAPITAL  CapsLock
		0x90, // VK_NUMLOCK
		0x91: // VK_SCROLL
		return true
	}
	return false
}

// modifierName 把修饰键 VK 翻译成可读名
func modifierName(vk uint16) string {
	switch vk {
	case vkShift:
		return "shift"
	case vkCtrl:
		return "ctrl"
	case vkAlt:
		return "alt"
	case vkLWin, vkRWin:
		return "win"
	}
	return ""
}

// vkToReadableKey 把 VK code 转成按键表达式(不包含修饰)
// 字母 a-z 用小写;其他用 namedVK 的反向查表;查不到再补符号
func vkToReadableKey(vk uint16) string {
	if (vk >= 'A' && vk <= 'Z') {
		// A-Z(0x41-0x5A);shift 状态由调用方决定是否加 "shift+" 前缀
		return string(rune(vk + 32)) // 转小写
	}
	if (vk >= '0' && vk <= '9') {
		return string(rune(vk))
	}
	// 反向查 namedVK
	for name, code := range namedVK {
		if code == vk {
			return name
		}
	}
	// 已知符号 VK(部分 OEM 键,因多键盘布局不同,给一个可读名)
	switch vk {
	case 0xBA:
		return ";"
	case 0xBB:
		return "="
	case 0xBC:
		return ","
	case 0xBD:
		return "-"
	case 0xBE:
		return "."
	case 0xBF:
		return "/"
	case 0xC0:
		return "`"
	case 0xDB:
		return "["
	case 0xDC:
		return "\\"
	case 0xDD:
		return "]"
	case 0xDE:
		return "'"
	}
	// 真没识别出来,降级为"vkXX",运行时会把它当作未知的按键报错(用户能看到)
	return fmt.Sprintf("vk%02X", vk)
}

// ===== 钩子回调 =====

// runHookThread 钩子线程入口:安装两个钩子、进入消息循环、退出时清理。
// 之所以独立线程,是因 LL 钩子回调必须运行在安装它的线程(且该线程要持续泵消息)。
func (r *Recorder) runHookThread() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 记下本线程 id,供 F8-stop 时 PostThreadMessage
	tid, _, _ := procGetCurrentThreadId.Call()
	r.threadId.Store(uint32(tid))

	mouseCb := syscall.NewCallback(func(nCode, wparam, lparam uintptr) uintptr {
		return r.handleMouse(nCode, wparam, lparam)
	})
	kbCb := syscall.NewCallback(func(nCode, wparam, lparam uintptr) uintptr {
		return r.handleKeyboard(nCode, wparam, lparam)
	})

	mouseHook, _, _ := procSetWindowsHookExW.Call(whMouseLl, mouseCb, 0, 0)
	if mouseHook == 0 {
		r.onLog("✗ 安装鼠标钩子失败(GetLastError 可能需要管理员权限)")
		return
	}
	defer procUnhookWindowsHookEx.Call(mouseHook)

	kbHook, _, _ := procSetWindowsHookExW.Call(whKeyboardLl, kbCb, 0, 0)
	if kbHook == 0 {
		r.onLog("✗ 安装键盘钩子失败(GetLastError 可能需要管理员权限)")
		return
	}
	defer procUnhookWindowsHookEx.Call(kbHook)

	r.armed.Set(true)
	r.onState("armed")
	r.onLog("🎯 钩子已就绪,按 F8 开始录制,再按 F8 结束")

	// 消息循环
	var msg winmsg
	for {
		r1, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if r1 == 0 || r1 == ^uintptr(0) {
			break // WM_QUIT 或错误
		}
		// 检查外部强制停止
		select {
		case <-r.stopCh:
			goto done
		default:
		}
	}
done:
	r.armed.Set(false)
	r.recording.Set(false)

	// 把已收集到的事件转成 Step 列表
	steps := r.collectToSteps()
	if r.stopRequested() {
		r.onLog("⏹ 录制被强制停止,已丢弃事件")
		r.onComplete(nil)
	} else {
		r.onLog(fmt.Sprintf("✔ 录制完成,共 %d 步", len(steps)))
		r.onComplete(steps)
	}
	r.onState("idle")
}

// stopRequested 判断当前停止是用户主动按 F8 还是外部强制 Stop()
func (r *Recorder) stopRequested() bool {
	select {
	case <-r.stopCh:
		return true
	default:
		return false
	}
}

// handleMouse 鼠标钩子回调:仅在 recording 状态下记录事件
func (r *Recorder) handleMouse(nCode, wparam, lparam uintptr) uintptr {
	if nCode >= 0 && r.recording.IsSet() {
		ms := (*msllhookstruct)(unsafe.Pointer(lparam))
		x, y := int(ms.Pt.X), int(ms.Pt.Y)
		switch wparam {
		case wmLButtonDown:
			r.appendEvent(recordedEvent{Kind: "click", X: x, Y: y, Button: "left", When: time.Now()})
		case wmRButtonDown:
			r.appendEvent(recordedEvent{Kind: "click", X: x, Y: y, Button: "right", When: time.Now()})
		case wmLButtonDblClk:
			r.appendEvent(recordedEvent{Kind: "doubleclick", X: x, Y: y, Button: "left", When: time.Now()})
		case wmRButtonDblClk:
			r.appendEvent(recordedEvent{Kind: "doubleclick", X: x, Y: y, Button: "right", When: time.Now()})
		}
	}
	r1, _, _ := procCallNextHookEx.Call(0, nCode, wparam, lparam)
	return r1
}

// handleKeyboard 键盘钩子回调:
//   - F8:切换 armed <-> recording
//   - 其他键:仅在 recording 状态下记录 keypress(带当前修饰键状态)
func (r *Recorder) handleKeyboard(nCode, wparam, lparam uintptr) uintptr {
	if nCode >= 0 && (wparam == wmKeydown || wparam == wmSyskeydown) {
		kb := (*kbdllhookstruct)(unsafe.Pointer(lparam))
		vk := uint16(kb.VkCode)

		if vk == vkF8 {
			// F8:切换
			if r.recording.IsSet() {
				// recording → 结束
				r.recording.Set(false)
				r.armed.Set(false)
				r.onState("idle")
				r.onLog("⏹ F8 结束录制")
				// 投 WM_QUIT 让消息循环退出
				tid := r.threadId.Load()
				procPostThreadMessageW.Call(uintptr(tid), wmQuit, 0, 0)
				return 1 // 吞掉 F8,不让它继续传到前台应用
			}
			if r.armed.IsSet() {
				// armed → recording
				r.mu.Lock()
				r.events = nil
				r.mu.Unlock()
				r.recording.Set(true)
				r.onState("recording")
				r.onLog("▶ F8 开始录制")
				return 1
			}
		}

		// 普通键:仅在 recording 时记录
		if r.recording.IsSet() {
			switch {
			case isModifierKey(vk):
				// 修饰键按下不直接产生 keypress,只影响后续 keypress 的修饰前缀
			case isToggleKey(vk):
				// 过滤 toggle 键(CapsLock / NumLock / ScrollLock),回放会反复切换有副作用
				r.onLog(fmt.Sprintf("  · 跳过 toggle 键: vk=0x%02X", vk))
			default:
				key := composeKeyWithModifiers(vk)
				if key != "" {
					r.appendEvent(recordedEvent{Kind: "keypress", Key: key, When: time.Now()})
				}
			}
		}
	}

	r1, _, _ := procCallNextHookEx.Call(0, nCode, wparam, lparam)
	return r1
}

// composeKeyWithModifiers 把 VK 翻译成"ctrl+shift+a"这种形式
func composeKeyWithModifiers(vk uint16) string {
	// 注意:这里只是按"当前按下的修饰键"前缀,不做去抖 / 弹起跟踪
	mods := []string{}
	// 用 GetAsyncKeyState 读当前按住状态,比跟 keyup 事件更准
	if isKeyDown(vkShift) {
		mods = append(mods, "shift")
	}
	if isKeyDown(vkCtrl) {
		mods = append(mods, "ctrl")
	}
	if isKeyDown(vkAlt) {
		mods = append(mods, "alt")
	}
	if isKeyDown(vkLWin) || isKeyDown(vkRWin) {
		mods = append(mods, "win")
	}
	key := vkToReadableKey(vk)
	if len(mods) == 0 {
		return key
	}
	return joinMods(mods) + "+" + key
}

func joinMods(mods []string) string {
	out := mods[0]
	for i := 1; i < len(mods); i++ {
		out += "+" + mods[i]
	}
	return out
}

func isKeyDown(vk uint16) bool {
	state, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return state&0x8000 != 0
}

// appendEvent 线程安全地追加事件
func (r *Recorder) appendEvent(e recordedEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// collectToSteps 把录制事件转成 Step 列表
func (r *Recorder) collectToSteps() []Step {
	r.mu.Lock()
	evs := make([]recordedEvent, len(r.events))
	copy(evs, r.events)
	r.mu.Unlock()

	steps := make([]Step, 0, len(evs))
	for _, e := range evs {
		s := Step{DelayMs: 0} // DelayMs 由前端 ingestRecordedSteps 统一覆盖为用户配置的"默认延迟"
		switch e.Kind {
		case "click":
			s.Type = "click"
			s.X, s.Y = e.X, e.Y
			s.Button = e.Button
		case "doubleclick":
			s.Type = "doubleclick"
			s.X, s.Y = e.X, e.Y
			s.Button = e.Button
		case "keypress":
			s.Type = "keypress"
			s.Key = e.Key
		default:
			continue
		}
		steps = append(steps, s)
	}
	return steps
}

// ===== 公开 API =====

// RecorderStart 启动录制监听(装钩子 + 进入消息循环)
// 内部用 onComplete/onLog/onState 三个回调把结果推回调用方
func RecorderStart(
	ctx context.Context,
	onState func(string),
	onLog func(string),
	onComplete func([]Step),
) error {
	if recorder.armed.IsSet() || recorder.recording.IsSet() {
		return errors.New("已经在录制监听中")
	}
	recorder.ctx = ctx
	recorder.onState = onState
	recorder.onLog = onLog
	recorder.onComplete = onComplete
	recorder.stopCh = make(chan struct{})
	recorder.stopOnce = sync.Once{}
	recorder.threadId.Store(0)

	go recorder.runHookThread()
	return nil
}

// RecorderStop 强制停止(用户主动取消)
// 已录事件会被丢弃。
func RecorderStop() {
	if !recorder.armed.IsSet() && !recorder.recording.IsSet() {
		return
	}
	recorder.stopOnce.Do(func() {
		close(recorder.stopCh)
	})
	// 通知钩子线程自己 PostThreadMessage(WM_QUIT)兜底
	tid := recorder.threadId.Load()
	if tid != 0 {
		procPostThreadMessageW.Call(uintptr(tid), wmQuit, 0, 0)
	}
}

// RecorderState 返回当前状态:idle / armed / recording
func RecorderState() string {
	if recorder.recording.IsSet() {
		return "recording"
	}
	if recorder.armed.IsSet() {
		return "armed"
	}
	return "idle"
}
