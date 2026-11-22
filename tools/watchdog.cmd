@echo off
REM Gateway watchdog: restart if 8900 is not listening.
REM Runs every minute from Task Scheduler. Deliberately does NOT install a
REM Windows service (sc.exe is on this machine's program blacklist).
REM Keep this file pure ASCII: cmd.exe parses .cmd as ANSI.

netstat -ano | findstr /R /C:":8900 .*LISTENING" >nul 2>&1
if %errorlevel%==0 exit /b 0

REM A gateway process may be mid-startup (takes ~6-8s). Starting another one
REM would fight for the port, so skip when the process already exists.
tasklist /fi "IMAGENAME eq unified-api-v2.exe" 2>nul | findstr /i "unified-api-v2.exe" >nul 2>&1
if %errorlevel%==0 exit /b 0

cd /d "%~dp0.."
echo [%date% %time%] watchdog: 8900 not listening and no process, restarting>> gateway.err.log

REM -detach makes the gateway FreeConsole() itself, so no black window survives.
REM Do NOT add >> redirection here: the process writes its own log after detaching.
start "" /b unified-api-v2.exe -config config.json -detach
exit /b 0
