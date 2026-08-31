#requires -Version 5.1
<#
.SYNOPSIS
    提交并打 tag，再推送到远端（GitHub / Gitee 通用）。

.DESCRIPTION
    用法:
        .\scripts\release.ps1                       # 交互式输入版本号
        .\scripts\release.ps1 -Version 1.2.3        # 直接指定版本号
        .\scripts\release.ps1 -AutoBump patch       # 自动递增补丁号并使用
        .\scripts\release.ps1 -Message "fix: xxx"   # 自定义提交信息

    默认会:
        1. git fetch + 检查工作区是否干净
        2. git add -A
        3. git commit -m "release: vX.Y.Z"
        4. git tag -a vX.Y.Z -m "vX.Y.Z"
        5. git push origin <current-branch> --follow-tags
#>

[CmdletBinding()]
param(
    [string]$Version,
    [ValidateSet('major', 'minor', 'patch')]
    [string]$AutoBump = 'patch',
    [string]$Message,
    [string]$Branch,
    [switch]$DryRun,
    [switch]$SkipPush
)

$ErrorActionPreference = 'Stop'

function Write-Step($text) {
    Write-Host "`n==> $text" -ForegroundColor Cyan
}

function Bump-Semver([string]$v, [string]$part) {
    if ($v -notmatch '^(\d+)\.(\d+)\.(\d+)$') {
        throw "无法解析版本号: '$v'，应为 MAJOR.MINOR.PATCH 格式"
    }
    $major = [int]$Matches[1]; $minor = [int]$Matches[2]; $patch = [int]$Matches[3]
    switch ($part) {
        'major' { return "$($major+1).0.0" }
        'minor' { return "$major.$($minor+1).0" }
        'patch' { return "$major.$minor.$($patch+1)" }
    }
}

function Get-CurrentBranch {
    $name = (& git rev-parse --abbrev-ref HEAD).Trim()
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($name)) { throw "无法获取当前分支" }
    return $name
}

function Get-LatestTag {
    $raw = git describe --tags --abbrev=0 2>&1
    if ($LASTEXITCODE -ne 0) { return $null }
    $clean = $raw | Where-Object { $_ -match '^v?\d+(\.\d+){1,2}$' } | Select-Object -First 1
    if (-not $clean) { return $null }
    return $clean.Trim()
}

# --- sanity checks ----------------------------------------------------------
try {
    $null = & git rev-parse --show-toplevel 2>&1
    if ($LASTEXITCODE -ne 0) { throw "不在 git 仓库中" }
} catch { throw "当前目录不是 git 仓库" }

Write-Step "更新远端引用"
& git fetch --tags --prune
if ($LASTEXITCODE -ne 0) { throw "git fetch 失败" }

$status = & git status --porcelain
if ($status) {
    Write-Host "工作区有未提交的修改：" -ForegroundColor Yellow
    & git status --short
    $confirm = Read-Host "继续提交这些修改吗？[y/N]"
    if ($confirm -notin @('y','Y','yes','Yes')) { throw "用户取消" }
}

# --- resolve target branch ---------------------------------------------------
if (-not $Branch) { $Branch = Get-CurrentBranch }
Write-Host "当前分支: $Branch"

# --- resolve version ---------------------------------------------------------
if (-not $Version) {
    $latest = Get-LatestTag
    if ($latest -and $latest -match '^v(\d+\.\d+\.\d+)$') {
        $base = $Matches[1]
        $suggested = Bump-Semver $base $AutoBump
        $prompt = "请输入新版本号（当前最新 $latest，$AutoBump 自动建议 $suggested）"
    } else {
        $suggested = '0.1.0'
        $prompt = "请输入版本号（首次发版建议 $suggested）"
    }
    $Version = Read-Host $prompt
    if ([string]::IsNullOrWhiteSpace($Version)) {
        $Version = $suggested
        Write-Host "使用建议版本号: $Version"
    }
}

if ($Version -notmatch '^\d+\.\d+\.\d+$') {
    throw "版本号必须为 MAJOR.MINOR.PATCH，当前为 '$Version'"
}
$Tag = "v$Version"
Write-Host "目标版本: $Tag"

$exists = $false
$verifyOut = git rev-parse -q --verify "refs/tags/$Tag" 2>&1
if ($LASTEXITCODE -eq 0) { $exists = $true }
if ($exists) {
    throw "tag '$Tag' 已经存在，请先删除或换一个新版本号 (git tag -d $Tag)"
}

# --- resolve commit message --------------------------------------------------
if (-not $Message) { $Message = "release: $Tag" }
Write-Host "提交信息: $Message"

# --- execute -----------------------------------------------------------------
if ($DryRun) {
    Write-Step "[DRY-RUN] 以下是预览，不会真正执行"
    Write-Host "git add -A"
    Write-Host "git commit -m \"$Message\""
    Write-Host "git tag -a $Tag -m \"$Tag\""
    if (-not $SkipPush) {
        Write-Host "git push origin $Branch --follow-tags"
    }
    return
}

Write-Step "添加所有变更"
& git add -A
if ($LASTEXITCODE -ne 0) { throw "git add 失败" }

$hasStaged = (& git diff --cached --name-only)
if ($hasStaged) {
    Write-Step "提交变更"
    & git commit -m "$Message"
    if ($LASTEXITCODE -ne 0) { throw "git commit 失败" }
} else {
    Write-Host "没有需要提交的变更，跳过 commit"
}

Write-Step "创建 tag: $Tag"
& git tag -a $Tag -m $Tag
if ($LASTEXITCODE -ne 0) { throw "git tag 失败" }

if ($SkipPush) {
    Write-Host "`n[--SkipPush] 已跳过推送，必要时手动执行：" -ForegroundColor Yellow
    Write-Host "  git push origin $Branch --follow-tags"
    return
}

Write-Step "推送到 origin/$Branch (含 tag)"
& git push origin $Branch --follow-tags
if ($LASTEXITCODE -ne 0) {
    Write-Host "推送失败。tag 已建好，稍后可手动推送：" -ForegroundColor Red
    Write-Host "  git push origin $Tag"
    throw "git push 失败"
}

Write-Host "`n✔ 已发布 $Tag" -ForegroundColor Green

# 根据远端地址给出仓库发布页链接（支持 Gitee / GitHub）
$repoSlug = $null
try {
    $remoteUrl = (& git remote get-url origin).Trim()
    if ($remoteUrl -match '(?:gitee\.com|github\.com)[:/](.+?)(?:\.git)?$') {
        $repoSlug = $Matches[1]
    }
} catch {}
if ($repoSlug) {
    if ($remoteUrl -match 'gitee\.com') {
        Write-Host "查看/创建发布: https://gitee.com/$repoSlug/releases"
    } else {
        Write-Host "查看发布: https://github.com/$repoSlug/releases/tag/$Tag"
    }
}
