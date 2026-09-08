@echo off
setlocal EnableDelayedExpansion
title Achyu PACS - Build

:: ===========================================================================
::  THE build script. It always builds BOTH halves together.
::
::  Building only one half is how a release ships an installer whose bundled
::  engine predates the UI talking to it — the failure looks like a missing
::  API endpoint, days later, on a site you cannot reach.
:: ===========================================================================

echo.
echo  ============================================
echo   Achyu PACS - Full Build
echo  ============================================
echo.

cd /d "%~dp0"

:: -- 1. Prerequisites -------------------------------------------------------
where go >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Go not found. Install from https://go.dev/dl/ and re-run.
    pause & exit /b 1
)
for /f "tokens=3" %%v in ('go version') do set GO_VER=%%v
echo [1/6] Go %GO_VER% found

where npm >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Node/npm not found. Install from https://nodejs.org/ and re-run.
    pause & exit /b 1
)

:: Version comes from package.json so the installer, the About screen and the
:: binary can never disagree about what this build is.
for /f "usebackq tokens=2 delims=:, " %%a in (`findstr /r /c:"\"version\"" package.json`) do (
    if not defined APP_VER set APP_VER=%%~a
)
if not defined APP_VER set APP_VER=0.0.0-dev
echo       Version %APP_VER%

:: -- 2. Clean ---------------------------------------------------------------
:: Guarantees no stale artifact can ship: if a later step fails, there is
:: nothing left behind for electron-builder to pick up by accident.
echo [2/6] Cleaning previous artifacts ...
if exist dist rmdir /s /q dist
if exist tarang-sender.exe del /q tarang-sender.exe

:: -- 3. Build the Go engine -------------------------------------------------
:: Windows 7 compatibility: Go 1.21+ dropped Win7 support, so force the last
:: Win7-capable toolchain (auto-downloaded by the installed Go on first use).
:: Build 32-bit (386) to match the ia32 Electron installer and to run on both
:: 32- and 64-bit Windows.
set GOTOOLCHAIN=go1.20.14
set GOARCH=386
set CGO_ENABLED=0
echo [3/6] Building tarang-sender.exe (go1.20.14, 386) ...
go build -ldflags "-X main.version=%APP_VER% -s -w" -o tarang-sender.exe ./cmd/tarang-sender/
if errorlevel 1 (
    echo [ERROR] Go build failed.
    pause & exit /b 1
)

:: -- 4. Verify the toolchain actually used ----------------------------------
:: Editing go.mod to "go 1.20" does NOT produce a Win7 binary — only the
:: toolchain that compiles it does. Losing the GOTOOLCHAIN override silently
:: yields a binary that refuses to start on Win7, which is exactly the kind of
:: failure you find out about from a clinic rather than from the build.
echo [4/6] Verifying binary toolchain ...
go version tarang-sender.exe | findstr /c:"go1.20.14" >nul
if errorlevel 1 (
    echo [ERROR] tarang-sender.exe was NOT built with go1.20.14.
    echo         It will not run on Windows 7. Check GOTOOLCHAIN.
    go version tarang-sender.exe
    pause & exit /b 1
)
echo       toolchain verified: go1.20.14 / 386

:: -- 5. npm install ---------------------------------------------------------
echo [5/6] Installing Node dependencies ...
call npm install --prefer-offline --silent
if errorlevel 1 (
    echo [ERROR] npm install failed.
    pause & exit /b 1
)

:: -- 6. Package -------------------------------------------------------------
echo [6/6] Packaging Electron installer ...
call npx electron-builder --win --ia32
if errorlevel 1 (
    echo [ERROR] electron-builder failed.
    pause & exit /b 1
)

echo.
echo  ============================================
echo   Build complete.  Output is in dist\
echo  ============================================
echo.
for %%f in (dist\*.exe) do echo   Installer: %%f
echo.
pause
