# deliver-console 完整构建（Windows 原生入口）：① 前端 ② 交叉编译 deliver-agent ③ 交叉编译配置中心插件 ④ go build 内嵌
# 用法：powershell -ExecutionPolicy Bypass -File build.ps1（或双击 build.cmd）
# 本文件必须保存为「UTF-8 带 BOM」：Windows PowerShell 5.1 对无 BOM 文件按 ANSI/GBK 解析，中文输出即乱码
$ErrorActionPreference = "Stop"
Set-Location (Split-Path $MyInvocation.MyCommand.Path -Parent)
# 控制台切 UTF-8（等价 chcp 65001）：npm/go 等原生命令的 UTF-8 输出在默认 GBK 控制台才不乱码；重定向等无控制台宿主下静默跳过
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch {}

Write-Host "==> [1/4] 前端构建"
Push-Location ui
if (-not (Test-Path node_modules)) { npm install --no-audit --no-fund }
npm run build
Pop-Location

Write-Host "==> [2/4] 交叉编译 deliver-agent（内嵌用）"
New-Item -ItemType Directory -Force -Path agent-bin | Out-Null
Remove-Item agent-bin/deliver-agent-linux, agent-bin/deliver-agent-windows.exe -ErrorAction SilentlyContinue
Push-Location ../deliver-agent
$agentVer = Get-Date -Format yyyyMMddHHmm
$env:GOOS = "linux";   $env:GOARCH = "amd64"; go build -ldflags="-s -w -X main.agentVersion=$agentVer" -o ../deliver-console/agent-bin/deliver-agent-linux .
$env:GOOS = "windows"; $env:GOARCH = "amd64"; go build -ldflags="-s -w -X main.agentVersion=$agentVer" -o ../deliver-console/agent-bin/deliver-agent-windows.exe .
Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
Pop-Location

Write-Host "==> [3/4] 交叉编译配置中心插件（内嵌用）"
New-Item -ItemType Directory -Force -Path plugin-bin | Out-Null
Remove-Item plugin-bin/nacos-config-linux, plugin-bin/nacos-config-windows.exe -ErrorAction SilentlyContinue
Push-Location ../deliver-plugins/nacos-config
$env:GOOS = "linux";   $env:GOARCH = "amd64"; go build -ldflags="-s -w" -o ../../deliver-console/plugin-bin/nacos-config-linux .
$env:GOOS = "windows"; $env:GOARCH = "amd64"; go build -ldflags="-s -w" -o ../../deliver-console/plugin-bin/nacos-config-windows.exe .
Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
Pop-Location

Write-Host "==> [4/4] 控制台二进制（内嵌前端 + agent + 插件；agentStamp=内嵌 agent 版本戳，旧 agent 预警用）"
go build -ldflags="-s -w -X main.agentStamp=$agentVer" -o deliver-console.exe .
Write-Host "完成：.\deliver-console.exe（默认 http://0.0.0.0:7870）"
