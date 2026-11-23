# doubao-login.ps1 — 用独立 Edge profile 登录豆包，供网关读取含 msToken 的 Cookie
#
# 为什么需要它：豆包签到要求 Cookie 里带 msToken，而 msToken 是网页 JS 在页面
# 加载时生成的反爬 token，纯 HTTP 客户端拿不到、也续不了。唯一可靠的办法是让
# 浏览器走一遍，网关再从该 profile 读取。
#
# 用独立 --user-data-dir：不碰日常 Edge 配置；登录完关掉窗口，网关即可读取。
#
# 用法：.\tools\doubao-login.ps1
#      在弹出的窗口里登录豆包，确认右上角已是目标账号后关闭窗口。

$ErrorActionPreference = 'Stop'

$profileDir = Join-Path $env:LOCALAPPDATA 'unified-api-v2\doubao-profile'
New-Item -ItemType Directory -Path $profileDir -Force | Out-Null

$candidates = @(
    (Join-Path $env:ProgramFiles 'Microsoft\Edge\Application\msedge.exe'),
    (Join-Path ${env:ProgramFiles(x86)} 'Microsoft\Edge\Application\msedge.exe'),
    (Join-Path $env:LOCALAPPDATA 'Microsoft\Edge\Application\msedge.exe')
)
$edge = $candidates | Where-Object { $_ -and (Test-Path $_) } | Select-Object -First 1
if (-not $edge) {
    throw '未找到 msedge.exe。请安装 Edge，或改用 Chrome 并把路径告诉网关。'
}

Write-Host '正在打开独立 Edge 窗口…' -ForegroundColor Cyan
Write-Host "  专用 profile: $profileDir"
Write-Host '  请在该窗口中登录豆包（扫码或手机号均可）。'
Write-Host '  确认右上角显示的是目标账号后，关闭窗口即可（网关会自动读取 Cookie）。'

& $edge --user-data-dir="$profileDir" --no-first-run --no-default-browser-check `
    "https://www.doubao.com/chat/"
