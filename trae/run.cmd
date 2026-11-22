@echo off
REM Trae provider launcher (vendored, relocatable via %~dp0).
REM NOTE: keep this file pure ASCII. cmd.exe parses .cmd as ANSI (GBK here),
REM so UTF-8 Chinese comments get shredded and fragments run as commands.
REM
REM Everything else (UPSTREAM_MODE, log redirection, no console) lives in
REM run.py. pythonw.exe has no console, and deliberately avoiding an inner
REM "cmd /c" is what stops a window from lingering: cmd would wait for the
REM resident uvicorn process and keep its console alive.
cd /d "%~dp0"
start "" /b ".venv\Scripts\pythonw.exe" run.py
exit /b 0
