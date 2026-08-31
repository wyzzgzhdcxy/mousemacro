# 鼠标助手 (mouseassistant)

基于 **Wails v2 + Vue 3 + Go** 的鼠标宏录制与自动化执行工具。

通过图形界面录制鼠标 / 键盘操作序列，保存为脚本后可重复执行。

## 功能特性

- 录制鼠标移动、点击、双击、滚轮、键盘按键、文本输入、窗口定位等步骤
- 支持步骤级延迟、循环次数、随机偏移等执行参数
- 录制脚本保存为本地 JSON，方便备份与分享
- 系统级热键触发开始 / 停止 / 运行（仅 Windows）

## 技术栈

| 层级    | 技术                                |
| ------- | ----------------------------------- |
| 前端    | Vue 3.5 + Vite 8                    |
| 后端    | Go 1.27 + Wails v2.15               |
| 系统集成 | Win32 API (Windows) / stub (其它) |

## 环境要求

- Go >= 1.27
- Node.js >= 20.19 (推荐 24，与本机/CI 一致)
- Wails CLI: `go install github.com/wailsapp/wails/v2/cmd/wails@v2.15.0`
- Windows 端构建需 WebView2 Runtime（Win11 自带）

## 项目结构

```
mousemacro/
├── app.go                 # Wails App 结构 & 业务逻辑
├── main.go                # Wails 入口
├── recorder_*.go          # 鼠标/键盘录制 (Windows 实现 + 其它平台 stub)
├── runhotkey_*.go         # 全局热键 (Windows 实现 + 其它平台 stub)
├── wininput_*.go          # 鼠标/键盘注入 (Windows 实现 + 其它平台 stub)
├── frontend/              # Vue 3 前端
│   ├── src/
│   └── package.json
├── scripts/
│   ├── build-local.ps1    # 本地构建 + 部署
│   └── release.ps1        # 打 tag + 推送
├── .github/workflows/
│   └── release.yml        # tag 触发 CI 构建并发布 GitHub Release
├── wails.json             # Wails 项目配置
├── go.mod / go.sum
└── README.md
```

## 本地开发

```powershell
# 安装前端依赖
cd frontend
npm install

# 启动热重载开发模式（前端热更新 + Go 端重启）
cd ..
wails dev
```

浏览器开发面板：<http://localhost:34115>（在 dev 模式下访问可调用 Go 方法）

## 本地构建

默认会构建 `build/bin/mouseassistant.exe`，并自动部署到 `E:\application\我的工具箱`（目标进程在跑时会先停止再覆盖）。

```powershell
# 默认：构建 + 部署
.\scripts\build-local.ps1

# 只构建，不部署
.\scripts\build-local.ps1 -NoDeploy

# 自定义输出目录
.\scripts\build-local.ps1 -OutputDir dist

# 部署到其它目录
.\scripts\build-local.ps1 -DeployDir D:\tools

# 跳过前端构建（仅 Go 端快验）
.\scripts\build-local.ps1 -SkipFrontend

# 仅预览将要执行的命令
.\scripts\build-local.ps1 -DryRun
```

参数说明：

| 参数            | 说明                                        |
| --------------- | ------------------------------------------- |
| `-Clean`        | 构建前清空 `build/bin/`                     |
| `-SkipFrontend` | 跳过前端 `npm install` / `npm run build`    |
| `-Platform`     | Wails 平台，默认 `windows/amd64`            |
| `-OutputDir`    | 输出目录，默认 `build\bin`                  |
| `-DeployDir`    | 部署目录，默认 `E:\application\我的工具箱`  |
| `-NoDeploy`     | 只构建不部署                                |
| `-DryRun`       | 只打印计划执行的步骤                        |

## 发布版本

```powershell
# 交互式输入版本号
.\scripts\release.ps1

# 直接指定
.\scripts\release.ps1 -Version 1.2.3

# 自动递增补丁号
.\scripts\release.ps1 -AutoBump patch

# 自定义提交信息
.\scripts\release.ps1 -Message "feat: 支持循环录制"

# 仅预览
.\scripts\release.ps1 -DryRun

# 打 tag 但不推送（之后手动 push）
.\scripts\release.ps1 -SkipPush
```

推送 tag 后，`.github/workflows/release.yml` 会在 Windows (amd64) 上构建并自动创建 GitHub Release，工件命名格式：

```
mouseassistant-vX.Y.Z-<goos>-<goarch>.<ext>
```

## 跨平台说明

- `recorder_windows.go` / `runhotkey_windows.go` / `wininput_windows.go`：使用 Win32 API 的真实实现
- 对应的 `*_other.go`：Linux / macOS 上的 stub，保证可编译但功能不可用
- 完整功能仅在 Windows 平台可用

## 许可证

MIT