#requires -Version 5.1
<#
.SYNOPSIS
    本地构建 mouseassistant（Wails Vue + Go）单可执行文件，可选部署到目标目录。

.DESCRIPTION
    用法:
        .\scripts\build-local.ps1                  # 默认构建，输出到 build/bin/
        .\scripts\build-local.ps1 -Clean           # 构建前清理 build/bin/
        .\scripts\build-local.ps1 -Platform win    # 指定 wails 平台 (默认 windows/amd64)
        .\scripts\build-local.ps1 -SkipFrontend    # 不重装/重建前端 (开发快速验证用)
        .\scripts\build-local.ps1 -OutputDir dist   # 自定义输出目录
        .\scripts\build-local.ps1 -DeployDir D:\x   # 覆盖部署目录
        .\scripts\build-local.ps1 -NoDeploy        # 只构建不部署
        .\scripts\build-local.ps1 -DryRun          # 只预览不执行

    默认会部署到 D:\app_mgr\my\（目标进程在跑时先杀掉再复制）。

    产出:
        build/bin/mouseassistant.exe   <- 单文件 GUI 程序（隐藏控制台）

    说明:
        - 通过 `wails build` 一次性产出前端+后端的单可执行文件；
        - 构建完成后默认把 mouseassistant.exe 部署到 -DeployDir 指定的目录，
          部署前会先停止同名正在运行的进程（避免文件占用）。
    部署目录（app_output_dir）:
        取值优先级：-DeployDir 参数 > 环境变量 app_output_dir > 内置兜底 D:\app_mgr\my
        查看当前值：$env:app_output_dir
        修改：[Environment]::SetEnvironmentVariable('app_output_dir', 'D:\app_mgr\my', 'User')
#>

[CmdletBinding()]
param(
    [switch]$Clean,
    [switch]$SkipFrontend,
    [string]$Platform = "windows/amd64",
    [string]$OutputDir = "build\bin",
    [string]$DeployDir,       # 部署目录；留空则取环境变量 app_output_dir，兜底 D:\app_mgr\my
    [switch]$NoDeploy,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

# --- resolve deploy dir ------------------------------------------------------
# 部署目录取值优先级：-DeployDir 参数 > 环境变量 app_output_dir > 内置兜底目录
$FallbackDeployDir = "D:\app_mgr\my"
$envDeployDir = [Environment]::GetEnvironmentVariable('app_output_dir')
$envDeployDir = if ($null -ne $envDeployDir) { $envDeployDir.Trim() } else { '' }

if ($DeployDir) {
    $DeployDirSource = '-DeployDir 参数'
} elseif ($envDeployDir) {
    $DeployDir = $envDeployDir
    $DeployDirSource = '环境变量 app_output_dir'
} else {
    $DeployDir = $FallbackDeployDir
    $DeployDirSource = '脚本内置默认值'
}

# 去掉结尾多余的分隔符，避免拼出 "D:\app_mgr\my\\app.exe"；盘符根目录保留
$DeployDir = $DeployDir.Trim().TrimEnd('\', '/')
if ($DeployDir -match '^[A-Za-z]:$') { $DeployDir += '\' }

function Write-Step($t) { Write-Host "`n==> $t" -ForegroundColor Cyan }
function Write-Ok($t)   { Write-Host $t -ForegroundColor Green }
function Write-Warn($t) { Write-Host $t -ForegroundColor Yellow }

# 停止指定名称（不带 .exe 后缀）的进程，不存在时静默跳过
function Stop-ExeProcess([string]$name) {
    $procs = Get-Process -Name $name -ErrorAction SilentlyContinue
    if ($procs) {
        Write-Host "  停止进程: $name (PID: $(($procs.Id) -join ', '))" -ForegroundColor Yellow
        $procs | Stop-Process -Force -ErrorAction SilentlyContinue
        Start-Sleep -Milliseconds 500
    }
}

function Require-Command($cmd) {
    $ok = Get-Command $cmd -ErrorAction SilentlyContinue
    if (-not $ok) { throw "未找到命令: $cmd，请先安装并加入 PATH" }
}

$root        = Split-Path $PSScriptRoot -Parent
$productName = "mouseassistant"   # 与 wails.json#outputfilename / module 保持一致

# --- preflight ---------------------------------------------------------------
Write-Step "检查构建依赖"
Require-Command go
Require-Command wails
Write-Host "  $(& go version).Trim()"
Write-Host "  $(& wails version)"

# --- output dir --------------------------------------------------------------
$binDir = if ([System.IO.Path]::IsPathRooted($OutputDir)) { $OutputDir } else { Join-Path $root $OutputDir }
if ($Clean -and (Test-Path $binDir)) {
    Write-Step "清理输出目录"
    Remove-Item -LiteralPath $binDir -Recurse -Force
}
if (-not (Test-Path $binDir)) {
    New-Item -ItemType Directory -Path $binDir -Force | Out-Null
}
Write-Host "输出目录: $binDir"

# --- 构建 --------------------------------------------------------------------
# wails 默认输出到 <root>/build/bin/<outputfilename>，与 $binDir 一致；不传 -o。
$exeName = "$productName.exe"
$out     = Join-Path $binDir $exeName

if ($DryRun) {
    Write-Step "[DRY-RUN] 将执行: wails build"
    Write-Host "  wails build -platform $Platform -trimpath -clean"
    Write-Host "  预期产物: $out"
    if (-not $SkipFrontend) {
        Write-Host "  (前端: wails 会自动调用 frontend:install + frontend:build)"
    }
} else {
    Write-Step "wails build ($Platform)"
    Push-Location $root
    try {
        $args = @("build", "-platform", $Platform, "-trimpath", "-clean")
        if ($SkipFrontend) { $args += @("-skipbindings") }
        & wails @args
        if ($LASTEXITCODE -ne 0) { throw "wails build 失败 (exit=$LASTEXITCODE)" }
    } finally {
        Pop-Location
    }
    if (-not (Test-Path -LiteralPath $out)) { throw "未找到预期产物: $out" }
    Write-Ok "  ✓ 已生成: $out"
}

# --- deploy ------------------------------------------------------------------
if ($NoDeploy) {
    Write-Warn "已跳过部署 (-NoDeploy)"
} else {
    Write-Step "部署到: $DeployDir  (来源: $DeployDirSource)"
    if (-not (Test-Path -LiteralPath $DeployDir)) {
        New-Item -ItemType Directory -Path $DeployDir -Force | Out-Null
    }
    if ($DryRun) {
        Write-Host "  (DRYRUN) copy: $out -> $DeployDir"
    } else {
        if (-not (Test-Path -LiteralPath $out)) {
            throw "未找到产物 $out，无法部署"
        }
        Stop-ExeProcess $productName
        Copy-Item -LiteralPath $out -Destination (Join-Path $DeployDir $exeName) -Force
        Write-Ok "  ✓ 已部署: $(Join-Path $DeployDir $exeName)"
    }
}

# --- summary -----------------------------------------------------------------
Write-Step "构建完成"
if (Test-Path $binDir) {
    Get-ChildItem -LiteralPath $binDir -File | Sort-Object Name |
        Select-Object Name, @{n='Size(MB)';e={[math]::Round($_.Length/1MB,2)}} |
        Format-Table -AutoSize | Out-String | Write-Host
}

Write-Host "最终输出目录: $binDir" -ForegroundColor Green