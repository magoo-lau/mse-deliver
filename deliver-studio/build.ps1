# ============================================================================
# deliver-studio 完整构建（Windows 版，与 build.sh 等价）
#   前端 → 工作台单文件二进制（内嵌前端与预设；安装由 deliver-console 承担）
# 产物：deliver-studio.exe / deliver-studio-linux（本目录，gitignore）
# 前置：node/npm + go；首次需网络拉 npm 与 go 模块（modernc.org/sqlite）
# 用法：powershell -ExecutionPolicy Bypass -File build.ps1（或双击 build.cmd）
# ============================================================================
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $MyInvocation.MyCommand.Path)

foreach ($tool in @("npm", "go")) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "缺少 $tool（请先安装并加入 PATH）" }
}

# 交叉编译一步：设 GOOS/GOARCH → go build → 无论成败恢复环境（防污染本机构建）
function Invoke-GoBuild([string]$goos, [string]$out) {
    $env:GOOS = $goos
    $env:GOARCH = "amd64"
    try {
        go build -ldflags="-s -w" -o $out .
        if ($LASTEXITCODE -ne 0) { throw "go build 失败（GOOS=$goos → $out）" }
    } finally {
        Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
    }
}

Write-Host "==> [1/2] 前端构建（ui → ui/dist）"
Push-Location ui
npm install --no-audit --no-fund
if ($LASTEXITCODE -ne 0) { throw "npm install 失败" }
npm run build
if ($LASTEXITCODE -ne 0) { throw "npm run build 失败" }
Pop-Location

Write-Host "==> [2/2] 工作台构建（内嵌前端 dist）"
Invoke-GoBuild "windows" "deliver-studio.exe"
Invoke-GoBuild "linux"   "deliver-studio-linux"

Write-Host "构建完成 ✔ deliver-studio.exe / deliver-studio-linux（双击或运行即起，数据目录 deliver-data\）"
