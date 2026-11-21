$ErrorActionPreference = 'SilentlyContinue'

# 并发保护：两次看门狗间隔至少 110 秒（独立锁，不与阶段1豆包看门狗共享）
$lock = Join-Path $env:TEMP 'unifiedapiv2_watchdog.lock'
if (Test-Path $lock) {
    $age = (Get-Date) - (Get-Item $lock).LastWriteTime
    if ($age.TotalSeconds -lt 110) { exit 0 }
    Remove-Item $lock -Force
}
Set-Content -Path $lock -Value (Get-Date).ToString('o')

$taskName = 'UnifiedAPIV2'
$port = 8900

# 端口已监听 = 服务健康，直接返回
$conn = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue
if ($conn) { exit 0 }

$task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
if (-not $task) { exit 0 }

# 端口未监听但任务在跑：说明进程卡死/异常，精确按进程名终止（绝不用宽 commandline 匹配）
# 历史教训：Stop-ScheduledTask 不会结束子进程，必须先精确清掉 exe 进程再重启
if ($task.State -eq 'Running') {
    $procs = Get-CimInstance Win32_Process -Filter "Name = 'unified-api-v2.exe'"
    foreach ($p in $procs) {
        Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
    }
    Start-Sleep -Seconds 2
}

Start-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
