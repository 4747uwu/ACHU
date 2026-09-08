@echo off
setlocal enabledelayedexpansion

rem tools\mtufix.bat - clamp every IPv4 interface's MTU.
rem
rem RUN AS ADMINISTRATOR (right-click cmd.exe -> "Run as administrator"):
rem
rem     mtufix.bat list      show what it WOULD do, change nothing
rem     mtufix.bat           clamp everything to 1400
rem     mtufix.bat 1500      put it back
rem
rem Why this instead of typing netsh by hand: the interface is NOT called
rem "Wi-Fi" on every machine. On the site box the live adapter is "Wireless
rem Network Connection 2", and netsh answers a name it does not know with
rem "The filename, directory name, or volume label syntax is incorrect",
rem which reads like a quoting mistake rather than a wrong name. This reads
rem the real names out of netsh and clamps all of them, so no adapter gets
rem missed because the traffic was not on the one you assumed.

set "TARGET=%~1"
set "DRYRUN="
if /i "%TARGET%"=="list" set "DRYRUN=1" & set "TARGET=1400"
if "%TARGET%"=="" set "TARGET=1400"

if not defined DRYRUN (
  fltmc >nul 2>&1
  if errorlevel 1 (
    echo.
    echo   ERROR: this must run as Administrator.
    echo   Right-click cmd.exe, choose "Run as administrator", then run it again.
    echo.
    exit /b 1
  )
)

echo.
echo === BEFORE ===
netsh interface ipv4 show subinterfaces
echo.
if defined DRYRUN (
  echo === DRY RUN - nothing will be changed ===
) else (
  echo === setting MTU=%TARGET% on every non-loopback interface ===
)
echo.

for /f "tokens=1,2,3,4,* delims= " %%A in ('netsh interface ipv4 show subinterfaces') do (
  set "MTU=%%A"
  set "NAME=%%E"

  rem Numeric test: strip digits as delimiters. A row whose first column is a
  rem number leaves nothing behind, so the loop body never runs and NOTNUM
  rem stays unset. Headers and the dashed rule fail this and are skipped.
  set "NOTNUM="
  for /f "delims=0123456789" %%Z in ("!MTU!") do set "NOTNUM=1"

  if not defined NOTNUM if defined NAME (
    echo !NAME!| findstr /i /c:"Loopback" >nul
    if errorlevel 1 (
      if defined DRYRUN (
        echo   would set "!NAME!"  ^(currently !MTU!^)
      ) else (
        echo   !NAME!  ^(was !MTU!^)
        netsh interface ipv4 set subinterface "!NAME!" mtu=%TARGET% store=persistent >nul
        if errorlevel 1 (echo       FAILED) else (echo       ok)
      )
    )
  )
)

echo.
echo === AFTER ===
netsh interface ipv4 show subinterfaces
echo.
if not defined DRYRUN (
  echo Now restart Achyu PACS 2 and watch the log.
  echo   fixed  -^> "peer reachable - resuming uploads"
  echo   same   -^> still "TLS handshake timeout", so MTU was not the cause
  echo.
  echo To undo:  mtufix.bat 1500
)
echo.
endlocal
