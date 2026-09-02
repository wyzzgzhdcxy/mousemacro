package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ===== 共享类型 =====

// Step 表示一个可被执行的鼠标/键盘步骤。
// 通过 JSON 序列化从 Vue 前端传过来;字段在 Wails 端会按名字绑定。
type Step struct {
	Type    string `json:"type"`     // move | click | doubleclick | keypress | keydown | keyup | type | movewindow
	X       int    `json:"x"`        // 鼠标步骤的 X 坐标
	Y       int    `json:"y"`        // 鼠标步骤的 Y 坐标
	Button  string `json:"button"`   // left | right(click / doubleclick)
	Key     string `json:"key"`      // keypress / keydown / keyup 的按键表达式
	Text    string `json:"text"`     // type 步骤的待输入文本
	Title   string `json:"title"`    // movewindow 步骤的目标窗口标题关键字
	DelayMs int    `json:"delayMs"`  // 执行该步骤前的等待毫秒数
}

// Pos 表示屏幕上的一个坐标点(物理像素)。
// 用 struct 而不是 (int, int) 多返回值,避免 Wails 在多返回值编码上的歧义。
type Pos struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// 事件名(常量,前后端都引用,方便对齐)
const (
	EvtRunStarted   = "run:started"
	EvtRunProgress  = "run:progress"
	EvtRunCompleted = "run:completed"
	EvtRunStopped   = "run:stopped"
	EvtRunLog       = "run:log"

	EvtRecordState    = "record:state"     // "idle" | "armed" | "recording"
	EvtRecordLog      = "record:log"
	EvtRecordComplete = "record:complete"  // payload: []Step
)

// 一些进程级单例
var (
	errAborted = errors.New("用户中止")

	// 日志文件句柄,所有 emit(EvtRunLog / EvtRecordLog) 时同时落盘
	logFileMu    sync.Mutex
	logFile      *os.File
	logFilePath  string

	appRunning atomic.Bool
	appMu      sync.Mutex // 保护 start/stop 不会重入
)

// atomicBool 是 atomic.Bool 的薄封装,wininput_windows.go 引用了 *atomicBool
// 类型,所以 App 里的字段必须能拿到指针。sync/atomic.Bool 没有暴露指针的途径,
// 这里包一层,内部用 uint32 实现,便于跨包传 *atomicBool。
type atomicBool struct {
	v uint32
}

func (a *atomicBool) Set(b bool) {
	if b {
		atomic.StoreUint32(&a.v, 1)
	} else {
		atomic.StoreUint32(&a.v, 0)
	}
}
func (a *atomicBool) IsSet() bool { return atomic.LoadUint32(&a.v) == 1 }

// 包装供 runStepOnWindows 使用的 *atomicBool 指针。
// 这个 abort 在 stop 时由 Stop() 翻转。
var runAbort = &atomicBool{}

// sleepInterruptible 在等待时检查中止标记,被中断立即返回 errAborted。
// 同时轮询 Esc 键作为兜底。
func sleepInterruptible(d time.Duration, abort *atomicBool) error {
	deadline := time.Now().Add(d)
	for {
		if abort.IsSet() {
			return errAborted
		}
		if winIsEscPressed() {
			abort.Set(true)
			return errAborted
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		step := 20 * time.Millisecond
		if remaining < step {
			step = remaining
		}
		time.Sleep(step)
	}
}

// ===== 日志文件 =====

// initLogFile 启动时打开日志文件,追加写。
// 路径: %APPDATA%/mouseassistant/log.txt (Windows)
func initLogFile() {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir, _ = os.Getwd() // 兜底:用当前目录
	}
	logDir := filepath.Join(dir, "mouseassistant")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return
	}
	logFilePath = filepath.Join(logDir, "log.txt")
	f, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	logFileMu.Lock()
	logFile = f
	logFileMu.Unlock()
}

func closeLogFile() {
	logFileMu.Lock()
	defer logFileMu.Unlock()
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}

func writeLogLine(line string) {
	logFileMu.Lock()
	defer logFileMu.Unlock()
	if logFile == nil {
		return
	}
	ts := time.Now().Format("2006-01-02 15:04:05")
	logFile.WriteString("[" + ts + "] " + line + "\n")
}

func getLogFilePath() string {
	if logFilePath != "" {
		return logFilePath
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "mouseassistant", "log.txt")
}

func openLogFolder() error {
	path := getLogFilePath()
	if path == "" {
		return errors.New("无法获取日志路径")
	}
	dir := filepath.Dir(path)
	// explorer.exe 打开文件夹最稳(Windows 原生)
	cmd := exec.Command("explorer.exe", dir)
	return cmd.Start()
}

// ===== App =====

// App struct
type App struct {
	ctx context.Context

	// 最近一次的运行配置(由 RunSteps 内部更新),供 F12 热键复用
	lastRunMu         sync.Mutex
	lastSteps         []Step
	lastLoopCount     int
	lastIntervalMs    int
	lastStartDelayMs  int
	lastOnceTypes     []string
}

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. The context is saved
// so we can call the runtime methods
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	initLogFile()
	// 挂载 F12 常驻热键(开始/停止执行切换)
	runHotkeySetToggleFunc(func() { a.ToggleRunFromHotkey() })
	runHotkeyStart()
}

// domReady 在前端 DOM 准备好后被调用,可用于在 UI 加载完成后做一次性初始化。
func (a *App) domReady(ctx context.Context) {}

// shutdown 在应用退出前调用。
func (a *App) shutdown(ctx context.Context) {
	runAbort.Set(true)
	appRunning.Store(false)
	runHotkeyStop()
	closeLogFile()
}

// GetMousePos 返回当前鼠标在屏幕上的物理像素坐标。
// 用于前端"取当前位置"按钮,直接把当前坐标填入表单。
func (a *App) GetMousePos() Pos {
	x, y := winGetCursorPos()
	return Pos{X: x, Y: y}
}

// MoveWindowByTitle 将标题包含 titleKeyword 的所有可见顶层窗口移动到屏幕坐标 (x, y)。
// 返回被移动窗口的标题列表;没有匹配窗口时返回错误。
func (a *App) MoveWindowByTitle(titleKeyword string, x int, y int) ([]string, error) {
	return winMoveWindowsByTitle(titleKeyword, x, y)
}

// ListWindowTitles 枚举当前桌面上所有可见的顶层窗口,返回它们的标题列表(去重)。
// 用于前端"移动窗口"步骤的下拉选择器。
func (a *App) ListWindowTitles() ([]string, error) {
	return winListWindowTitles()
}

// RunSteps 异步执行一组步骤循环 N 次。
// 启动后立即返回,通过 run:* 事件向前端推送进度。
//
// 参数:
//   - steps:      步骤列表
//   - loopCount:  循环次数,>=1
//   - intervalMs: 两次循环之间的间隔毫秒数
//   - startDelayMs: 启动前的倒计时毫秒数(给用户切窗口留时间)
//   - onceTypes:  仅首次执行的操作类型列表(如 ["movewindow"])。这些类型只在第 1 次循环执行,
//     后续循环直接跳过;传空列表表示不做任何跳过。
//
// 返回值:调用结果状态(已启动 / 失败原因),真正的执行结果通过事件传达。
func (a *App) RunSteps(steps []Step, loopCount int, intervalMs int, startDelayMs int, onceTypes []string) (string, error) {
	// 缓存最近一次的运行配置(深拷贝 steps,避免后续 JS 端的修改影响)
	a.lastRunMu.Lock()
	a.lastSteps = append([]Step(nil), steps...)
	a.lastLoopCount = loopCount
	a.lastIntervalMs = intervalMs
	a.lastStartDelayMs = startDelayMs
	a.lastOnceTypes = append([]string(nil), onceTypes...)
	a.lastRunMu.Unlock()

	appMu.Lock()
	if appRunning.Load() {
		appMu.Unlock()
		return "", errors.New("已有任务正在执行,请先停止")
	}
	if len(steps) == 0 {
		appMu.Unlock()
		return "", errors.New("步骤列表为空")
	}
	if loopCount < 1 {
		appMu.Unlock()
		return "", errors.New("循环次数必须 >= 1")
	}
	runAbort.Set(false)
	appRunning.Store(true)
	appMu.Unlock()

	go func() {
		defer func() {
			appRunning.Store(false)
			runAbort.Set(false)
		}()

		emit := func(evt string, payload ...interface{}) {
			if a.ctx != nil {
				wruntime.EventsEmit(a.ctx, evt, payload...)
			}
			// 同步落盘到日志文件
			if evt == EvtRunLog {
				if s, ok := payload[0].(string); ok {
					writeLogLine(s)
				}
			}
		}

		emit(EvtRunStarted, map[string]any{
			"steps":        len(steps),
			"loopCount":    loopCount,
			"startDelayMs": startDelayMs,
		})
		emit(EvtRunLog, fmt.Sprintf("▶ 准备执行: %d 步 × %d 次,启动倒计时 %.1f 秒",
			len(steps), loopCount, float64(startDelayMs)/1000))

		// 启动倒计时
		if startDelayMs > 0 {
			emit(EvtRunLog, fmt.Sprintf("⏳ 倒计时 %d ms(按 Esc 取消)...", startDelayMs))
			if err := sleepInterruptible(time.Duration(startDelayMs)*time.Millisecond, runAbort); err != nil {
				emit(EvtRunStopped, map[string]any{"reason": "aborted", "when": "startDelay"})
				emit(EvtRunLog, "✖ 已取消(倒计时期间)")
				return
			}
		}

		// 主体循环
		for i := 1; i <= loopCount; i++ {
			emit(EvtRunProgress, map[string]any{
				"loop":      i,
				"totalLoop": loopCount,
				"step":      0,
				"totalStep": len(steps),
			})
			for j, s := range steps {
				if runAbort.IsSet() {
					emit(EvtRunStopped, map[string]any{
						"reason": "aborted",
						"loop":   i,
						"step":   j + 1,
					})
					emit(EvtRunLog, fmt.Sprintf("✖ 已中止 @ 第 %d 次 / 第 %d 步", i, j+1))
					return
				}
				// 仅首次执行的操作:第 1 次循环正常执行,后续循环直接跳过
				if i > 1 && stepIsOnceType(s.Type, onceTypes) {
					emit(EvtRunLog, fmt.Sprintf("  ↷ 第 %d/%d 步:%s(仅首次执行,跳过)", j+1, len(steps), formatStepForLog(s)))
					continue
				}
				emit(EvtRunProgress, map[string]any{
					"loop":      i,
					"totalLoop": loopCount,
					"step":      j + 1,
					"totalStep": len(steps),
				})
				emit(EvtRunLog, fmt.Sprintf("  → 第 %d/%d 步:%s", j+1, len(steps), formatStepForLog(s)))
				if err := runStepOnWindows(s, runAbort); err != nil {
					emit(EvtRunLog, fmt.Sprintf("  ✗ 第 %d/%d 步执行错误:%v", j+1, len(steps), err))
					emit(EvtRunStopped, map[string]any{
						"reason":   "error",
						"err":      err.Error(),
						"loop":     i,
						"step":     j + 1,
					})
					emit(EvtRunLog, fmt.Sprintf("✖ 步骤执行错误 @ %d/%d 步(第 %d 次): %v", j+1, len(steps), i, err))
					return
				}
				emit(EvtRunLog, fmt.Sprintf("  ✓ 第 %d/%d 步完成", j+1, len(steps)))
			}
			emit(EvtRunLog, fmt.Sprintf("✔ 第 %d/%d 次完成", i, loopCount))
			if i < loopCount && intervalMs > 0 {
				if err := sleepInterruptible(time.Duration(intervalMs)*time.Millisecond, runAbort); err != nil {
					emit(EvtRunStopped, map[string]any{"reason": "aborted", "when": "interval", "loop": i})
					emit(EvtRunLog, "✖ 已中止(循环间隔期间)")
					return
				}
			}
		}

		emit(EvtRunCompleted, map[string]any{
			"totalLoop": loopCount,
		})
		emit(EvtRunLog, "✅ 全部完成")
	}()

	return "started", nil
}

// Stop 主动中止当前正在执行的任务。
func (a *App) Stop() {
	runAbort.Set(true)
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, EvtRunLog, "⏹ 用户点击停止")
	}
}

// IsRunning 返回当前是否正在执行任务。
func (a *App) IsRunning() bool {
	return appRunning.Load()
}

// GetLogPath 返回当前日志文件绝对路径。
func (a *App) GetLogPath() string {
	return getLogFilePath()
}

// OpenLogFolder 用资源管理器打开日志所在文件夹。
func (a *App) OpenLogFolder() error {
	return openLogFolder()
}

// ToggleRunFromHotkey F12 热键回调:
//   - 正在跑 → 停止
//   - 没在跑 → 用最近一次的配置启动(启动延迟覆盖为 0,这样按下 F12 立刻执行)
func (a *App) ToggleRunFromHotkey() {
	if appRunning.Load() {
		a.Stop()
		return
	}
	a.lastRunMu.Lock()
	steps := append([]Step(nil), a.lastSteps...)
	loopCount := a.lastLoopCount
	intervalMs := a.lastIntervalMs
	onceTypes := append([]string(nil), a.lastOnceTypes...)
	a.lastRunMu.Unlock()

	if len(steps) == 0 {
		if a.ctx != nil {
			wruntime.EventsEmit(a.ctx, EvtRunLog, "✗ F12 热键:还没有可执行的步骤,请先在界面里 ▶ 开始执行 一次以缓存配置")
		}
		return
	}
	// F12 热键不走启动延迟(用户已经按了热键,不需要再等)
	if _, err := a.RunSteps(steps, loopCount, intervalMs, 0, onceTypes); err != nil {
		if a.ctx != nil {
			wruntime.EventsEmit(a.ctx, EvtRunLog, fmt.Sprintf("✗ F12 热键启动失败: %v", err))
		}
	} else {
		if a.ctx != nil {
			wruntime.EventsEmit(a.ctx, EvtRunLog, "⌨ F12 热键:开始执行")
		}
	}
}

// stepIsOnceType 判断某个步骤类型是否属于"仅首次执行"列表。
// 空列表表示不做任何跳过(返回 false)。
func stepIsOnceType(stepType string, onceTypes []string) bool {
	if len(onceTypes) == 0 {
		return false
	}
	for _, t := range onceTypes {
		if t == stepType {
			return true
		}
	}
	return false
}

// formatStepForLog 把 Step 格式化成一行可读的日志(和前端 formatStep 风格一致)。
func formatStepForLog(s Step) string {
	switch s.Type {
	case "move":
		return fmt.Sprintf("移动 → (%d, %d)", s.X, s.Y)
	case "click":
		if s.Button == "right" {
			return fmt.Sprintf("右键单击 @ (%d, %d)", s.X, s.Y)
		}
		return fmt.Sprintf("左键单击 @ (%d, %d)", s.X, s.Y)
	case "doubleclick":
		if s.Button == "right" {
			return fmt.Sprintf("右键双击 @ (%d, %d)", s.X, s.Y)
		}
		return fmt.Sprintf("左键双击 @ (%d, %d)", s.X, s.Y)
	case "keypress":
		return fmt.Sprintf("按键 %s", s.Key)
	case "keydown":
		return fmt.Sprintf("按下 %s", s.Key)
	case "keyup":
		return fmt.Sprintf("释放 %s", s.Key)
	case "type":
		return fmt.Sprintf("输入文本 %q", s.Text)
	case "movewindow":
		return fmt.Sprintf("移动窗口 %q → (%d, %d)", s.Title, s.X, s.Y)
	default:
		return fmt.Sprintf("%s (x=%d y=%d key=%q)", s.Type, s.X, s.Y, s.Key)
	}
}

// ===== 录制 =====

// StartRecord 启动全局键盘鼠标监听,等待 F8 开始 / F8 结束。
// 钩子线程由后端独立维护,录制结果通过 record:complete 事件推回前端。
func (a *App) StartRecord() (string, error) {
	if appRunning.Load() {
		return "", errors.New("宏正在执行中,请先停止")
	}
	if RecorderState() != "idle" {
		return "", errors.New("录制已在进行中")
	}
	if a.ctx == nil {
		return "", errors.New("前端尚未就绪")
	}
	ctx := a.ctx
	err := RecorderStart(ctx,
		func(state string) {
			wruntime.EventsEmit(ctx, EvtRecordState, state)
		},
		func(line string) {
			wruntime.EventsEmit(ctx, EvtRecordLog, line)
			writeLogLine("[record] " + line)
		},
		func(steps []Step) {
			if steps == nil {
				wruntime.EventsEmit(ctx, EvtRecordComplete, []Step{})
				return
			}
			wruntime.EventsEmit(ctx, EvtRecordComplete, steps)
		},
	)
	if err != nil {
		return "", err
	}
	return "armed", nil
}

// StopRecord 强制停止录制监听(已录事件会被丢弃)。
func (a *App) StopRecord() {
	RecorderStop()
}

// RecorderState 返回 idle / armed / recording。
func (a *App) RecorderState() string {
	return RecorderState()
}

// ===== 脚本存读 =====

// SaveSteps 弹出保存对话框,把 steps 写成 JSON 文件。
// 返回写入的文件路径(用户取消时为空字符串)。
func (a *App) SaveSteps(steps []Step) (string, error) {
	if a.ctx == nil {
		return "", errors.New("前端尚未就绪")
	}
	if len(steps) == 0 {
		return "", errors.New("步骤列表为空,无法保存")
	}
	path, err := wruntime.SaveFileDialog(a.ctx, wruntime.SaveDialogOptions{
		Title:           "保存宏脚本",
		DefaultFilename: "macro.json",
		Filters: []wruntime.FileFilter{
			{DisplayName: "JSON 脚本 (*.json)", Pattern: "*.json"},
			{DisplayName: "所有文件 (*.*)", Pattern: "*.*"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("打开保存对话框失败: %w", err)
	}
	if path == "" {
		return "", nil // 用户取消
	}
	data, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("写文件失败: %w", err)
	}
	return path, nil
}

// LoadSteps 弹出打开对话框,读 JSON 文件并返回 steps。
// 用户取消时返回空切片(nil error)。
func (a *App) LoadSteps() ([]Step, error) {
	if a.ctx == nil {
		return nil, errors.New("前端尚未就绪")
	}
	path, err := wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: "导入宏脚本",
		Filters: []wruntime.FileFilter{
			{DisplayName: "JSON 脚本 (*.json)", Pattern: "*.json"},
			{DisplayName: "所有文件 (*.*)", Pattern: "*.*"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("打开对话框失败: %w", err)
	}
	if path == "" {
		return nil, nil // 用户取消
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读文件失败: %w", err)
	}
	var steps []Step
	if err := json.Unmarshal(data, &steps); err != nil {
		return nil, fmt.Errorf("解析 JSON 失败: %w", err)
	}
	return steps, nil
}
