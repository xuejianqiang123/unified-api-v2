@echo off
REM Gateway launcher for Task Scheduler.
REM %~dp0 keeps it relocatable.
REM NOTE: keep this file pure ASCII. cmd.exe parses .cmd as ANSI (GBK here),
REM so UTF-8 Chinese comments get shredded and fragments run as commands.
cd /d "%~dp0"
unified-api-v2.exe -config config.json >> gateway.out.log 2>> gateway.err.log
