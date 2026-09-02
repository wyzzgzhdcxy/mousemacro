//go:build windows

// wininput_windows.go — Windows 平台下的鼠标键盘模拟实现。
//
// 设计要点：
//   - INPUT 联合体在 Go 中用 28 字节定长 buffer 表示，避免 MOUSEINPUT / KEYBDINPUT
//     大小不同导致 unsafe.Sizeof 失配（Windows 64-bit 上 INPUT 总长 36 字节，
//     type(4) + pad(4) + union(28)）。
//   - 执行过程中通过 GetAsyncKeyState 轮询 Esc，按下则立即中断循环。
//   - 高 DPI 感知:PER_MONITOR_AWARE_V2,让 SetCursorPos / GetCursorPos 用同一套物理像素。

package main

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ===== Windows 进程级初始化 =====

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	shcore           = syscall.NewLazyDLL("shcore.dll")
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procSetCursorPos = user32.NewProc("SetCursorPos")
	procGetCursorPos = user32.NewProc("GetCursorPos")
	procSendInput    = user32.NewProc("SendInput")
	procGetAsyncKeyState = user32.NewProc("GetAsyncKeyState")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDpiAwareness        = shcore.NewProc("SetProcessDpiAwareness")

	// 钩子相关(recorder_windows.go 需要)
	procSetWindowsHookExW = user32.NewProc("SetWindowsHookExW")
	procGetMessageW       = user32.NewProc("GetMessageW")
	procCallNextHookEx    = user32.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procPostThreadMessageW  = user32.NewProc("PostThreadMessageW")
	procGetCurrentThreadId  = kernel32.NewProc("GetCurrentThreadId")

	// 鼠标 / 键盘模拟:在 Wails GUI 进程下,SendInput 会被 UIPI 拦
	// (要求调用进程是前台进程,而用户已经把焦点切到目标窗口了)。
	// mouse_event / keybd_event 是更老的 API,在「非前台进程」下仍可工作。
	procMouseEvent = user32.NewProc("mouse_event")
	procKeybdEvent = user32.NewProc("keybd_event")

	// 剪贴板 API(给 winTypeText 用,SendInput(KEYEVENTF_UNICODE) 在非前台进程下也卡)
	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procGetClipboardData = user32.NewProc("GetClipboardData")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
)

// 钩子 / 消息相关常量
const (
	whKeyboardLl = 13
	whMouseLl    = 14

	wmKeydown    = 0x0100
	wmKeyup      = 0x0101
	wmSyskeydown = 0x0104
	wmSyskeyup   = 0x0105
)

// 对应 winuser.h 中的 KBDLLHOOKSTRUCT(低级键盘钩子回调的 lParam)
type kbdllhookstruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

// 对应 winuser.h 中的 MSG(钩子线程消息循环用)
type winmsg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

// ===== INPUT 联合体 =====

// INPUT 内存布局(DWORD type + 4 字节 pad + 28 字节 union),Go 端用定长 buffer 表达以避免
// 不同分支大小不一致导致 unsafe.Sizeof 失配。
type winInput struct {
	Type uint32
	_    uint32
	Data [28]byte
}

const (
	winInputMouse    = 0
	winInputKeyboard = 1

	mouseeventfMove       = 0x0001
	mouseeventfLeftdown   = 0x0002
	mouseeventfLeftup     = 0x0004
	mouseeventfRightdown  = 0x0008
	mouseeventfRightup    = 0x0010
	mouseeventfMiddledown = 0x0020
	mouseeventfMiddleup   = 0x0040

	keyeventfExtendedKey = 0x0001
	keyeventfKeyUp       = 0x0002
	keyeventfUnicode     = 0x0004

	vkEscape = 0x1B
)

// 点结构(对应 Win32 POINT)
type winPoint struct{ X, Y int32 }

// ===== 高 DPI =====

func setProcessDpiAware() {
	// 优先用 SetProcessDpiAwarenessContext (Win10 1703+),DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
	dpiAwarenessPerMonitorAwareV2 := int64(-4)
	procSetProcessDpiAwarenessContext.Call(uintptr(dpiAwarenessPerMonitorAwareV2))
	// 兜底:旧版 shcore PER_MONITOR_AWARE = 2
	procSetProcessDpiAwareness.Call(2)
}

// ===== 鼠标 =====

func winSetCursorPos(x, y int) error {
	r1, _, _ := procSetCursorPos.Call(uintptr(x), uintptr(y))
	if r1 == 0 {
		return fmt.Errorf("SetCursorPos(%d, %d) 失败", x, y)
	}
	return nil
}

func winGetCursorPos() (int, int) {
	var pt winPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	return int(pt.X), int(pt.Y)
}

func winSendMouse(flags uint32) error {
	// 走 mouse_event 而不是 SendInput:SendInput 要求调用进程是前台进程,
	// Wails GUI 进程下用户切走焦点就会被 UIPI 拦(同 integrity 也会卡)。
	// mouse_event 没有这个限制,适合"非前台进程模拟输入"的场景。
	// mouse_event(dwFlags, dx, dy, dwData, dwExtraInfo);dx/dy 给 0 表示用当前光标位置。
	procMouseEvent.Call(uintptr(flags), 0, 0, 0, 0)
	return nil
}

func winLeftClick() error {
	if err := winSendMouse(mouseeventfLeftdown); err != nil {
		return err
	}
	time.Sleep(20 * time.Millisecond)
	return winSendMouse(mouseeventfLeftup)
}

func winRightClick() error {
	if err := winSendMouse(mouseeventfRightdown); err != nil {
		return err
	}
	time.Sleep(20 * time.Millisecond)
	return winSendMouse(mouseeventfRightup)
}

func winLeftDoubleClick() error {
	if err := winLeftClick(); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)
	return winLeftClick()
}

func winRightDoubleClick() error {
	if err := winRightClick(); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)
	return winRightClick()
}

// ===== 键盘 =====

// 常见命名键 → virtual-key code
var namedVK = map[string]uint16{
	"backspace": 0x08, "tab": 0x09, "clear": 0x0C, "enter": 0x0D, "return": 0x0D,
	"shift": 0x10, "ctrl": 0x11, "control": 0x11, "alt": 0x12, "menu": 0x12,
	"pause": 0x13, "capslock": 0x14, "esc": 0x1B, "escape": 0x1B,
	"space": 0x20, "pageup": 0x21, "pagedown": 0x22, "end": 0x23, "home": 0x24,
	"left": 0x25, "up": 0x26, "right": 0x27, "down": 0x28,
	"select": 0x29, "print": 0x2A, "execute": 0x2B, "snapshot": 0x2C, "printscreen": 0x2C,
	"insert": 0x2D, "delete": 0x2E, "del": 0x2E,
	"help": 0x2F,

	"win": 0x5B, "lwin": 0x5B, "rwin": 0x5C, "apps": 0x5D, "sleep": 0x5F,

	"numpad0": 0x60, "numpad1": 0x61, "numpad2": 0x62, "numpad3": 0x63,
	"numpad4": 0x64, "numpad5": 0x65, "numpad6": 0x66, "numpad7": 0x67,
	"numpad8": 0x68, "numpad9": 0x69,
	"numpad_multiply": 0x6A, "numpad_add": 0x6B, "numpad_separator": 0x6C,
	"numpad_subtract": 0x6D, "numpad_decimal": 0x6E, "numpad_divide": 0x6F,

	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74, "f6": 0x75,
	"f7": 0x76, "f8": 0x77, "f9": 0x78, "f10": 0x79, "f11": 0x7A, "f12": 0x7B,
	"f13": 0x7C, "f14": 0x7D, "f15": 0x7E, "f16": 0x7F, "f17": 0x80, "f18": 0x81,
	"f19": 0x82, "f20": 0x83, "f21": 0x84, "f22": 0x85, "f23": 0x86, "f24": 0x87,

	"numlock": 0x90, "scrolllock": 0x91,

	"browser_back": 0xA6, "browser_forward": 0xA7, "browser_refresh": 0xA8,
	"browser_stop": 0xA9, "browser_search": 0xAA, "browser_favorites": 0xAB,
	"browser_home": 0xAC,
	"volume_mute": 0xAD, "volume_down": 0xAE, "volume_up": 0xAF,
	"media_next": 0xB0, "media_prev": 0xB1, "media_stop": 0xB2,
	"media_play_pause": 0xB3,
	"launch_mail": 0xB4, "launch_media_select": 0xB5, "launch_app1": 0xB6,
	"launch_app2": 0xB7,
}

// extendedKey:方向键 / Home / End / 编辑键 / 浏览器媒体键 / RWin / Apps / 小键盘 Enter
// 等带 E0 前缀的键需要打上 extended flag
var extendedKey = map[uint16]bool{
	0x0C: true, // numpad center (clear) — extended
	0x21: true, 0x22: true, 0x23: true, 0x24: true,
	0x25: true, 0x26: true, 0x27: true, 0x28: true,
	0x2D: true, 0x2E: true,
	0x5B: true, 0x5C: true, 0x5D: true, 0x5F: true, // win / apps / sleep
	0x6F: true, // numpad divide
	0x90: true, // numlock
	0xA6: true, 0xA7: true, 0xA8: true, 0xA9: true, 0xAA: true, 0xAB: true, 0xAC: true,
	0xAD: true, 0xAE: true, 0xAF: true,
	0xB0: true, 0xB1: true, 0xB2: true, 0xB3: true,
	0xB4: true, 0xB5: true, 0xB6: true, 0xB7: true,
}

func lookupVK(name string) (uint16, bool) {
	low := strings.ToLower(strings.TrimSpace(name))
	if v, ok := namedVK[low]; ok {
		return v, true
	}
	// 单字符:字母 / 数字,直接用大写 ASCII 作为 VK code
	if len(low) == 1 {
		c := low[0]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			return uint16(c), true
		}
		// 常见符号
		switch c {
		case '-':
			return 0xBD, true
		case '=':
			return 0xBB, true
		case '[':
			return 0xDB, true
		case ']':
			return 0xDD, true
		case '\\':
			return 0xDC, true
		case ';':
			return 0xBA, true
		case '\'':
			return 0xDE, true
		case ',':
			return 0xBC, true
		case '.':
			return 0xBE, true
		case '/':
			return 0xBF, true
		case '`':
			return 0xC0, true
		}
	}
	return 0, false
}

func winSendKey(vk uint16, down bool) error {
	// 同 winSendMouse 的原因:用 keybd_event 替代 SendInput。
	// keybd_event(bVk, bScan, dwFlags, dwExtraInfo);bScan 给 0 = 用 VK code 自带的 scan code。
	flags := uintptr(0)
	if !down {
		flags |= uintptr(keyeventfKeyUp)
	}
	if extendedKey[vk] {
		flags |= uintptr(keyeventfExtendedKey)
	}
	procKeybdEvent.Call(uintptr(vk), 0, flags, 0)
	return nil
}

// 解析形如 "ctrl+shift+s" 的组合键,逐个按键 down/up(修饰键先 down,普通键后 down;反之 up)
func winKeyCombo(combo string) error {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(combo)), "+")
	if len(parts) == 0 {
		return fmt.Errorf("空的按键表达式")
	}
	modVKs := make([]uint16, 0, len(parts)-1)
	mainVK, ok := lookupVK(parts[len(parts)-1])
	if !ok {
		return fmt.Errorf("未知的按键: %q", parts[len(parts)-1])
	}
	for _, p := range parts[:len(parts)-1] {
		vk, ok := lookupVK(p)
		if !ok {
			return fmt.Errorf("未知的修饰键: %q", p)
		}
		modVKs = append(modVKs, vk)
	}
	for _, vk := range modVKs {
		if err := winSendKey(vk, true); err != nil {
			return err
		}
	}
	if err := winSendKey(mainVK, true); err != nil {
		return err
	}
	time.Sleep(20 * time.Millisecond)
	if err := winSendKey(mainVK, false); err != nil {
		return err
	}
	for i := len(modVKs) - 1; i >= 0; i-- {
		if err := winSendKey(modVKs[i], false); err != nil {
			return err
		}
	}
	return nil
}

// ===== 剪贴板(给 winTypeText 用) =====

const (
	cfUnicodeText = 13  // CF_UNICODETEXT
	gmemMoveable  = 0x0002
)

// openClipboardRetry 重试 OpenClipboard(其它进程可能短暂占用剪贴板)。
func openClipboardRetry() bool {
	for i := 0; i < 10; i++ {
		if r1, _, _ := procOpenClipboard.Call(0); r1 != 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func winGetClipboardText() (string, bool) {
	if !openClipboardRetry() {
		return "", false
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(uintptr(cfUnicodeText))
	if h == 0 {
		return "", false
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return "", false
	}
	defer procGlobalUnlock.Call(h)
	return windows.UTF16PtrToString((*uint16)(unsafe.Pointer(p))), true
}

func winSetClipboardText(text string) error {
	if !openClipboardRetry() {
		return errors.New("OpenClipboard 失败(其它进程可能占用)")
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()

	utf16, err := syscall.UTF16FromString(text)
	if err != nil {
		return err
	}
	size := (len(utf16) + 1) * 2
	hMem, _, _ := procGlobalAlloc.Call(uintptr(gmemMoveable), uintptr(size))
	if hMem == 0 {
		return errors.New("GlobalAlloc 失败")
	}
	p, _, _ := procGlobalLock.Call(hMem)
	if p == 0 {
		return errors.New("GlobalLock 失败")
	}
	dst := (*[1 << 28]uint16)(unsafe.Pointer(p))[:len(utf16)+1]
	copy(dst, utf16)
	dst[len(utf16)] = 0
	procGlobalUnlock.Call(hMem)

	if r1, _, _ := procSetClipboardData.Call(uintptr(cfUnicodeText), hMem); r1 == 0 {
		return errors.New("SetClipboardData 失败")
	}
	return nil
}

// winTypeText 通过"剪贴板 + Ctrl+V"输入一段文本(unicode 安全)。
// SendInput + KEYEVENTF_UNICODE 在「非前台进程」下会被 UIPI 拦,
// 走剪贴板 + keybd_event 可以稳定工作(也支持中文/emoji 等 unicode 字符)。
// 副作用:会临时覆盖剪贴板,粘贴完成后 200ms 自动还原(如果原剪贴板有内容)。
func winTypeText(text string) error {
	if text == "" {
		return nil
	}

	saved, _ := winGetClipboardText()

	if err := winSetClipboardText(text); err != nil {
		return fmt.Errorf("type 步骤设置剪贴板失败: %w", err)
	}
	if err := winKeyCombo("ctrl+v"); err != nil {
		if saved != "" {
			_ = winSetClipboardText(saved)
		}
		return fmt.Errorf("type 步骤粘贴失败: %w", err)
	}

	// 200ms 后异步恢复(给目标窗口时间消费粘贴事件)
	if saved != "" {
		go func() {
			time.Sleep(200 * time.Millisecond)
			_ = winSetClipboardText(saved)
		}()
	}
	return nil
}

// ===== Esc 轮询 =====

func winIsEscPressed() bool {
	state, _, _ := procGetAsyncKeyState.Call(uintptr(vkEscape))
	return state&0x8000 != 0
}

// ===== 步骤执行 =====

// runStepOnWindows 在 Windows 平台上执行单个步骤;返回错误或被取消标志。
// 参数 abort 用于在每一步前检查是否需要中断。
func runStepOnWindows(step Step, abort *atomicBool) error {
	if abort.IsSet() {
		return errAborted
	}
	// 步骤自身的延时
	if step.DelayMs > 0 {
		if err := sleepInterruptible(time.Duration(step.DelayMs)*time.Millisecond, abort); err != nil {
			return err
		}
	}
	switch step.Type {
	case "move":
		return winSetCursorPos(step.X, step.Y)
	case "click":
		if err := winSetCursorPos(step.X, step.Y); err != nil {
			return fmt.Errorf("click 移动鼠标到 (%d,%d) 失败: %w", step.X, step.Y, err)
		}
		time.Sleep(20 * time.Millisecond)
		if step.Button == "right" {
			return winRightClick()
		}
		return winLeftClick()
	case "doubleclick":
		if err := winSetCursorPos(step.X, step.Y); err != nil {
			return fmt.Errorf("doubleclick 移动鼠标到 (%d,%d) 失败: %w", step.X, step.Y, err)
		}
		time.Sleep(20 * time.Millisecond)
		if step.Button == "right" {
			return winRightDoubleClick()
		}
		return winLeftDoubleClick()
	case "keypress":
		if step.Key == "" {
			return fmt.Errorf("keypress 步骤的 key 为空")
		}
		// 形如 "ctrl+s" / "enter" / "a"
		return winKeyCombo(step.Key)
	case "keydown":
		vk, ok := lookupVK(step.Key)
		if !ok {
			return fmt.Errorf("keydown 步骤的 key=%q 无法识别为已知按键", step.Key)
		}
		return winSendKey(vk, true)
	case "keyup":
		vk, ok := lookupVK(step.Key)
		if !ok {
			return fmt.Errorf("keyup 步骤的 key=%q 无法识别为已知按键", step.Key)
		}
		return winSendKey(vk, false)
	case "type":
		if step.Text == "" {
			return fmt.Errorf("type 步骤的 text 为空")
		}
		// 把字符串当文本逐字符输入
		return winTypeText(step.Text)
	case "movewindow":
		if step.Title == "" {
			return fmt.Errorf("movewindow 步骤的 title 为空")
		}
		moved, err := winMoveWindowsByTitle(step.Title, step.X, step.Y)
		if err != nil {
			return fmt.Errorf("movewindow 移动窗口(%q)到 (%d,%d) 失败: %w", step.Title, step.X, step.Y, err)
		}
		_ = moved
		return nil
	default:
		return fmt.Errorf("不支持的步骤类型: %q (key=%q, x=%d, y=%d)", step.Type, step.Key, step.X, step.Y)
	}
}

func init() {
	// 鼠标键盘 API 调用需要 UI 线程,SendInput 在工作线程也能用,但 setCursorPos / GetAsyncKeyState
	// 在某些应用下表现不一致。提前 LockOSThread,避免 goroutine 切换造成时序问题。
	runtime.LockOSThread()
	setProcessDpiAware()
}

// ===== 窗口操作(按标题查找并移动) =====

var (
	procEnumWindows     = user32.NewProc("EnumWindows")
	procGetWindowTextW  = user32.NewProc("GetWindowTextW")
	procIsWindowVisible = user32.NewProc("IsWindowVisible")
	procSetWindowPos    = user32.NewProc("SetWindowPos")
)

const (
	swpNoSize     = 0x0001 // 不改变窗口大小
	swpNoZOrder   = 0x0004 // 不改变 Z 序
	swpNoActivate = 0x0010 // 不激活窗口
)

// winIsWindowVisible 判断窗口是否可见。
func winIsWindowVisible(hwnd uintptr) bool {
	r, _, _ := procIsWindowVisible.Call(hwnd)
	return r != 0
}

// winGetWindowTitle 返回窗口标题(UTF-16 → string)。
func winGetWindowTitle(hwnd uintptr) string {
	buf := make([]uint16, 512)
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// winEnumWindows 枚举所有顶层窗口,对每个「可见且有标题」的窗口调用 visit(hwnd, title)。
// visit 返回 false 时提前停止枚举。
func winEnumWindows(visit func(hwnd uintptr, title string) bool) error {
	cb := syscall.NewCallback(func(hwnd, lParam uintptr) uintptr {
		if !winIsWindowVisible(hwnd) {
			return 1 // 继续
		}
		title := winGetWindowTitle(hwnd)
		if title == "" {
			return 1 // 继续(无标题窗口跳过)
		}
		if !visit(hwnd, title) {
			return 0 // 停止
		}
		return 1
	})
	r1, _, callErr := procEnumWindows.Call(cb, 0)
	_ = cb // 调用期间保持回调存活,避免被 GC
	if r1 == 0 {
		return fmt.Errorf("EnumWindows 失败: %v", callErr)
	}
	return nil
}

// winMoveWindow 把窗口移动到屏幕坐标 (x, y),不改变大小 / Z 序 / 激活状态。
func winMoveWindow(hwnd uintptr, x, y int) error {
	flags := uintptr(swpNoSize | swpNoZOrder | swpNoActivate)
	r1, _, callErr := procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), 0, 0, flags)
	if r1 == 0 {
		return fmt.Errorf("SetWindowPos 失败: %v", callErr)
	}
	return nil
}

// winMoveWindowsByTitle 将标题包含 keyword 的所有可见顶层窗口移动到 (x, y)。
// 返回被成功移动的窗口标题列表;没有匹配窗口时返回错误。
func winMoveWindowsByTitle(keyword string, x, y int) ([]string, error) {
	if keyword == "" {
		return nil, errors.New("窗口标题关键字为空")
	}
	var moved []string
	err := winEnumWindows(func(hwnd uintptr, title string) bool {
		if strings.Contains(title, keyword) {
			if err := winMoveWindow(hwnd, x, y); err == nil {
				moved = append(moved, title)
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if len(moved) == 0 {
		return nil, fmt.Errorf("未找到标题包含 %q 的可见窗口", keyword)
	}
	return moved, nil
}

// winListWindowTitles 枚举所有可见顶层窗口的标题,按枚举顺序去重后返回。
// 供前端"移动窗口"步骤的下拉选择器使用。
func winListWindowTitles() ([]string, error) {
	seen := make(map[string]struct{})
	var titles []string
	err := winEnumWindows(func(hwnd uintptr, title string) bool {
		if _, ok := seen[title]; !ok {
			seen[title] = struct{}{}
			titles = append(titles, title)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return titles, nil
}
