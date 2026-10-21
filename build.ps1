<#
.SYNOPSIS
    WebSearch 构建脚本（Windows 原生，无需 make）。

.DESCRIPTION
    与 Makefile 目标一一对应：build / test / vet / fmt / clean / run /
    quota / config / doctor / all。

.EXAMPLE
    .\build.ps1 build
    .\build.ps1 test
    .\build.ps1 all
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet('all', 'build', 'test', 'vet', 'fmt', 'clean', 'run', 'quota', 'config', 'doctor', 'build-linux')]
    [string]$Target = 'build',

    [switch]$Help,

    # 强制结束本仓库正在运行的 websearch.exe 后再构建。
    # 注意：当前 Codex 会话的 MCP 服务也会被结束，需重启客户端恢复。
    [switch]$StopRunning
)

if ($Help) {
    Get-Help $PSCommandPath -Detailed
    exit 0
}

$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

$Binary = 'bin\websearch.exe'
$Package = './cmd/websearch'

function Write-Step([string]$Message) {
    Write-Host "==> $Message" -ForegroundColor Cyan
}

function Invoke-Go {
    param([string[]]$Arguments)
    & go @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "go $($Arguments -join ' ') 执行失败（退出码 $LASTEXITCODE）"
    }
}

function Build {
    Write-Step "构建 $Binary"
    $binDir = Join-Path $PSScriptRoot 'bin'
    New-Item -ItemType Directory -Force -Path $binDir | Out-Null

    $target = Join-Path $PSScriptRoot $Binary
    $staging = Join-Path $binDir 'websearch.build.exe'

    if ($StopRunning) {
        Stop-WebSearchProcesses $target
    }

    Invoke-Go @('build', '-trimpath', '-o', $staging, $Package)

    if (-not (Test-Path -LiteralPath $target)) {
        Move-Item -LiteralPath $staging -Destination $target
        Write-Host "完成: $target" -ForegroundColor Green
        return
    }

    # 运行中的 exe 不能被覆盖，但可以改名。先把旧文件归档，
    # 再把新构建放到原路径，正在运行的进程继续使用归档文件。
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $archive = Join-Path $binDir "websearch.$stamp.old.exe"
    try {
        Move-Item -LiteralPath $target -Destination $archive
        Move-Item -LiteralPath $staging -Destination $target
        Write-Host "完成: $target" -ForegroundColor Green
        Write-Host "旧版本已归档: $archive（重启 Codex 后新版本生效）" -ForegroundColor Yellow
        Remove-OldArchives $binDir
    }
    catch {
        # 改名也失败时把旧文件放回，避免目标路径缺失。
        if ((Test-Path -LiteralPath $archive) -and -not (Test-Path -LiteralPath $target)) {
            Move-Item -LiteralPath $archive -Destination $target -ErrorAction SilentlyContinue
        }
        $fallback = Join-Path $binDir "websearch.$stamp.exe"
        if (Test-Path -LiteralPath $staging) {
            Move-Item -LiteralPath $staging -Destination $fallback -ErrorAction SilentlyContinue
        }
        throw ("无法替换 $target：文件正被进程占用。`n" +
            "已尝试改名归档仍失败，新构建保留在 $fallback。`n" +
            "可运行 .\build.ps1 build -StopRunning 强制结束本仓库的 websearch 进程后重试。`n" +
            "原始错误: $($_.Exception.Message)")
    }
}

# Remove-OldArchives 只保留最近 3 份旧版本归档，避免 bin 无限膨胀。
function Remove-OldArchives([string]$BinDir) {
    $archives = Get-ChildItem -LiteralPath $BinDir -Filter 'websearch.*.old.exe' -File -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTime -Descending
    foreach ($old in ($archives | Select-Object -Skip 3)) {
        Remove-Item -LiteralPath $old.FullName -Force -ErrorAction SilentlyContinue
    }
}

function Stop-WebSearchProcesses([string]$TargetPath) {
    $running = Get-CimInstance Win32_Process -Filter "Name='websearch.exe'" |
        Where-Object { $_.ExecutablePath -eq $TargetPath }
    if (-not $running) {
        Write-Host "没有正在运行的 websearch.exe" -ForegroundColor Yellow
        return
    }
    foreach ($p in $running) {
        Write-Host "结束进程 $($p.ProcessId)" -ForegroundColor Yellow
        Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
    }
    Start-Sleep -Milliseconds 300
}

function Test-All {
    Write-Step "运行测试"
    # -p 1 串行执行：Windows 下并行构建测试二进制会偶发文件锁冲突。
    Invoke-Go @('test', './...', '-count=1', '-p', '1')
}

function Vet {
    Write-Step "静态检查"
    Invoke-Go @('vet', './...')
}

function Fmt {
    Write-Step "格式化"
    Invoke-Go @('fmt', './...')
}

function Clean {
    Write-Step "清理产物"
    foreach ($dir in @('bin', 'data')) {
        if (Test-Path $dir) {
            Remove-Item -Recurse -Force $dir
            Write-Host "已删除 $dir"
        }
    }
}

function Build-Linux {
    Write-Step "交叉编译到 Linux"
    New-Item -ItemType Directory -Force -Path 'bin' | Out-Null
    foreach ($arch in @('amd64', 'arm64')) {
        $out = "bin/websearch-linux-$arch"
        $env:GOOS = 'linux'
        $env:GOARCH = $arch
        Invoke-Go @('build', '-trimpath', '-o', $out, $Package)
        Write-Host "完成: $out"
    }
    Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
}

function Run-Tool {
    param([string]$Sub, [string[]]$Rest)
    if (-not (Test-Path $Binary)) {
        Write-Step "先构建"
        Build
    }
    & $Binary $Sub @Rest
    exit $LASTEXITCODE
}

switch ($Target) {
    'all'       { Vet; Test-All; Build }
    'build'     { Build }
    'test'      { Test-All }
    'vet'       { Vet }
    'fmt'       { Fmt }
    'clean'     { Clean }
    'build-linux' { Build-Linux }
    'run'       { Run-Tool 'serve' @('--config', 'config.yaml') }
    'quota'     { Run-Tool 'quota' @('--config', 'config.yaml') }
    'config'    { Run-Tool 'config' @('--config', 'config.yaml') }
    'doctor'    { Run-Tool 'doctor' @('--config', 'config.yaml') }
}
