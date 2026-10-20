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

    [switch]$Help
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
    Invoke-Go @('build', '-trimpath', '-o', $Binary, $Package)
    Write-Host "完成: $((Resolve-Path $Binary).Path)" -ForegroundColor Green
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
