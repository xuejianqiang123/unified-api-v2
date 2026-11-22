# loomy-login.ps1 — 用一个独立的 Edge profile 登录 Loomy，供网关读取 cookie
#
# 为什么这么做：Loomy 只认一个 loomy_web_session cookie，且不签发 refresh token。
# 桌面客户端不把 session 落到本地（实测 Cookies 库为空），所以只能自己起一个
# 浏览器去登录——这也是 loomy2api 的做法（它用 Playwright 起有头 Chromium）。
#
# 这里用 Edge 的独立 --user-data-dir：不碰你日常的 Edge 配置，
# 登录完关掉这个窗口，网关就能从该 profile 读出 cookie。
#
# 用法：.\tools\loomy-login.ps1
#      然后在弹出的窗口里用手机号 + 验证码登录，登录成功后关闭窗口。

$ErrorActionPreference = 'Stop'

$profileDir = Join-Path $env:LOCALAPPDATA 'unified-api-v2\loomy-profile'
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
Write-Host '  请在该窗口中用手机号 + 验证码登录 Loomy。'
Write-Host '  登录成功后，关闭该窗口（整个窗口关掉，不是最小化），然后回到面板点「导入本机 Loomy」。'
Write-Host ''

# --user-data-dir 指向专用目录，与日常 Edge 完全隔离；
# --no-first-run 跳过引导页，--new-window 保证打开新窗口而不是复用已有实例。
Start-Process -FilePath $edge -ArgumentList @(
    "--user-data-dir=`"$profileDir`"",
    '--no-first-run',
    '--no-default-browser-check',
    '--new-window',
    'https://loomy.xunfei.cn/web'
)

Write-Host '窗口已打开。登录完成并关闭后，回到面板执行导入。' -ForegroundColor Green
