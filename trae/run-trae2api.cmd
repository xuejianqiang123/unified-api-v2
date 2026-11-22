@echo off
REM Trae2api-cn 启动脚本（供计划任务调用）。
REM UPSTREAM_MODE=ide：本机 Trae CN 账号在 remote 通道下无模型权限（实测
REM 「Trae remote model is not available for the bound account」），IDE Agent 通道可用。
REM 如需改回 remote/work/solo，直接改下面这行。
cd /d "D:\AI-Workspaces\2026-11-17-17-27-46\trae2api"
if not exist data mkdir data
set UPSTREAM_MODE=ide
".venv\Scripts\python.exe" -m uvicorn src.main:app --host 127.0.0.1 --port 8000 --log-level warning >> data\server.out.log 2>> data\server.err.log
