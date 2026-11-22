# deploy.ps1 — unified-api-v2 一键部署（含 vendor 进来的 Trae provider）
#
# 用法：
#   .\deploy.ps1                    # 准备环境并启动（不改计划任务）
#   .\deploy.ps1 -RegisterTasks     # 额外注册/更新两个计划任务（开机自启）
#   .\deploy.ps1 -SkipTrae          # 只编译网关
#   .\deploy.ps1 -MigrateData       # 从旧的 ..\trae2api\data 复制账号与缓存
#
# 设计约束：
#   * 幂等：重复执行不会重复装依赖、不会覆盖已有 data；
#   * 不静默改系统：默认不碰计划任务，必须显式 -RegisterTasks；
#   * 失败即停：$ErrorActionPreference='Stop'，不做"看起来成功"的兜底。

param(
    [switch]$SkipTrae,
    [switch]$RegisterTasks,
    [switch]$MigrateData
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
Set-Location $root

function Step($n, $msg) { Write-Host "[$n] $msg" -ForegroundColor Cyan }
function Info($msg) { Write-Host "    $msg" -ForegroundColor DarkGray }

# ---------- 1. 编译网关 ----------
Step '1/4' '编译 unified-api-v2'
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw '未找到 go，请先安装 Go 1.22+'
}
# 只编译到 .next.exe。真正的替换必须等进程停掉之后，
# 否则 Windows 会因文件被占用拒绝覆盖（早期版本在这里就失败）。
& go build -trimpath -o unified-api-v2.next.exe ./cmd/server
if ($LASTEXITCODE -ne 0) { throw 'go build 失败' }
Info '已生成 unified-api-v2.next.exe（待停服后替换）'

# ---------- 2. 准备 Trae provider ----------
if (-not $SkipTrae) {
    Step '2/4' '准备 Trae provider（Python venv + 依赖）'
    $traeDir = Join-Path $root 'trae'
    if (-not (Test-Path $traeDir)) { throw "未找到 $traeDir，vendor 目录缺失" }

    $venv = Join-Path $traeDir '.venv'
    if (-not (Test-Path $venv)) {
        $py = (Get-Command python -ErrorAction SilentlyContinue)
        if (-not $py) { throw '未找到 python，请先安装 Python 3.11+' }
        & $py.Source -m venv $venv
        Info "已创建 venv: $venv"
    }
    $vpy = Join-Path $venv 'Scripts\python.exe'
    & $vpy -m pip install -q -U pip
    & $vpy -m pip install -q -r (Join-Path $traeDir 'requirements.txt')
    & $vpy -m pip install -q -r (Join-Path $traeDir 'requirements-native.txt')
    Info 'Trae 依赖已就绪'

    # data 目录：账号、缓存、日志都在这里。
    $newData = Join-Path $traeDir 'data'
    $oldData = Join-Path (Split-Path $root -Parent) 'trae2api\data'
    if (-not (Test-Path $newData)) {
        if ($MigrateData -and (Test-Path $oldData)) {
            Copy-Item $oldData $newData -Recurse -Force
            Info "已从 $oldData 迁移账号与缓存"
        } else {
            New-Item -ItemType Directory -Path $newData -Force | Out-Null
            Info '新建空 data 目录（首次需在面板里登录 Trae 账号）'
        }
    } else {
        Info 'data 目录已存在，保留不动'
    }
} else {
    Step '2/4' '跳过 Trae（-SkipTrae）'
}

# ---------- 3. 注册计划任务（可选） ----------
if ($RegisterTasks) {
    Step '3/4' '注册/更新计划任务'
    $trigger = New-ScheduledTaskTrigger -AtLogOn
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable

    # 沿用现有做法：任务执行「命令行 /c run-*.cmd」，而不是直接跑 exe。
    # 包装脚本负责 cd、环境变量（UPSTREAM_MODE）与日志重定向，绕开它会丢这些。
    $sh = Join-Path $env:SystemRoot 'System32\cmd.exe'
    $gwAction = New-ScheduledTaskAction -Execute $sh `
        -Argument ('/c "' + (Join-Path $root 'run-unified-v2.cmd') + '"') -WorkingDirectory $root
    Register-ScheduledTask -TaskName 'UnifiedAPIV2' -Action $gwAction -Trigger $trigger -Settings $settings -Force | Out-Null
    Info 'UnifiedAPIV2 已注册（经 run-unified-v2.cmd 包装）'

    if (-not $SkipTrae) {
        $traeAction = New-ScheduledTaskAction -Execute $sh `
            -Argument ('/c "' + (Join-Path $root 'trae\run.cmd') + '"') -WorkingDirectory (Join-Path $root 'trae')
        Register-ScheduledTask -TaskName 'Trae2API' -Action $traeAction -Trigger $trigger -Settings $settings -Force | Out-Null
        Info 'Trae2API 已注册（经 trae\run.cmd 包装）'
    }
} else {
    Step '3/4' '跳过计划任务（如需开机自启请加 -RegisterTasks）'
}

# ---------- 4. 启动 ----------
Step '4/4' '启动服务'
function Restart-Task($name, $port) {
    Stop-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 1
    # 按端口精确定位并结束旧进程。早期按进程名匹配（'python'）会误杀本机其它
    # Python 服务，这里改为只处理监听目标端口的那个进程。
    $conn = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($conn) {
        Stop-Process -Id $conn.OwningProcess -Force -ErrorAction SilentlyContinue
        Info ("已结束端口 $port 上的旧进程 PID $($conn.OwningProcess)")
    }
    # 进程停掉之后才能安全替换 exe。
    if ($port -eq 8900 -and (Test-Path .\unified-api-v2.next.exe)) {
        if (Test-Path .\unified-api-v2.exe) {
            Copy-Item .\unified-api-v2.exe .\unified-api-v2.prev.exe -Force
            Remove-Item .\unified-api-v2.exe -Force
        }
        Move-Item .\unified-api-v2.next.exe .\unified-api-v2.exe -Force
        Info '已替换 unified-api-v2.exe'
    }
    Start-Sleep -Seconds 1
    Start-ScheduledTask -TaskName $name
}

if (Get-ScheduledTask -TaskName 'UnifiedAPIV2' -ErrorAction SilentlyContinue) {
    Restart-Task 'UnifiedAPIV2' 8900
    Start-Sleep -Seconds 5
    $ok = $false
    try { $ok = (Invoke-WebRequest -Uri 'http://127.0.0.1:8900/healthz' -UseBasicParsing -TimeoutSec 5).StatusCode -eq 200 } catch {}
    Info ("网关 healthz: " + $(if ($ok) { '200' } else { '不可达' }))
} else {
    Info '未找到 UnifiedAPIV2 计划任务，请手动启动 unified-api-v2.exe'
}

if (-not $SkipTrae -and (Get-ScheduledTask -TaskName 'Trae2API' -ErrorAction SilentlyContinue)) {
    Restart-Task 'Trae2API' 8000
    Start-Sleep -Seconds 8
    $okT = $false
    try { $okT = (Invoke-WebRequest -Uri 'http://127.0.0.1:8000/healthz' -UseBasicParsing -TimeoutSec 5).StatusCode -eq 200 } catch {}
    Info ("Trae healthz: " + $(if ($okT) { '200' } else { '不可达（看 trae\data\server.err.log）' }))
}

Write-Host ''
Write-Host '完成。面板: http://127.0.0.1:8900/' -ForegroundColor Green
