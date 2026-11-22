@echo off
REM Trae provider launcher (vendored, relocatable via %~dp0).
REM UPSTREAM_MODE=ide is REQUIRED: the bound Trae CN account has no model
REM permission on the remote channel; the IDE Agent channel works.
REM NOTE: keep this file pure ASCII. cmd.exe parses .cmd as ANSI (GBK here),
REM so UTF-8 Chinese comments get shredded and fragments run as commands.
REM
REM Two details that matter:
REM  1) pythonw.exe (not python.exe) - it has no console, so no black window.
REM  2) the inner cmd /c carries the log redirection; without it "start"
REM     would apply >> to itself instead of the python process.
set UPSTREAM_MODE=ide
cd /d "%~dp0"
if not exist data mkdir data
start "" /b cmd /c """.venv\Scripts\pythonw.exe" -m uvicorn src.main:app --host 127.0.0.1 --port 8000 --log-level warning >> data\server.out.log 2>> data\server.err.log"
exit /b 0
