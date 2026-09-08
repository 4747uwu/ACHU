#Requires -Version 5.1
<#
.SYNOPSIS
    Full build script for Achyu PACS  -  compiles the Go binary and
    packages the Electron installer in one shot.

.DESCRIPTION
    Steps:
      1. Verify prerequisites (Go, Node/npm)
      2. Build tarang-sender.exe  (go build with version stamp)
      3. npm install              (install/update Electron dependencies)
      4. electron-builder --win   (produce NSIS installer in ./dist/)

.PARAMETER Version
    Semantic version string injected into the binary and installer.
    Defaults to the value in package.json, or "0.1.0-dev" if unreadable.

.PARAMETER SkipGo
    Skip the Go build step (useful when only the UI changed).

.PARAMETER SkipElectron
    Skip the Electron packaging step (useful when only the Go code changed).

.PARAMETER Clean
    Remove dist/ and any previous tarang-sender.exe before building.

.EXAMPLE
    .\build.ps1
    .\build.ps1 -Version 1.2.0
    .\build.ps1 -SkipElectron          # re-build Go binary only
    .\build.ps1 -SkipGo                # re-package installer only
    .\build.ps1 -Clean -Version 1.0.0  # full clean build
#>

param(
    [string]$Version    = "",
    [switch]$SkipGo,
    [switch]$SkipElectron,
    [switch]$Clean
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# ── Helpers ────────────────────────────────────────────────────────────────

function Write-Step { param([string]$msg) Write-Host "`n==> $msg" -ForegroundColor Cyan }
function Write-OK   { param([string]$msg) Write-Host "    OK  $msg" -ForegroundColor Green }
function Write-Fail { param([string]$msg) Write-Host "    ERR $msg" -ForegroundColor Red; exit 1 }

function Assert-Command {
    param([string]$Name, [string]$Hint = "")
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        $extra = if ($Hint) { "  -  $Hint" } else { "" }
        Write-Fail "$Name not found in PATH$extra"
    }
}

# ── Resolve version ────────────────────────────────────────────────────────

$ScriptDir = $PSScriptRoot

if (-not $Version) {
    try {
        $pkg = Get-Content "$ScriptDir\package.json" -Raw | ConvertFrom-Json
        $Version = $pkg.version
    } catch {
        $Version = "0.1.0-dev"
    }
}

Write-Host ""
Write-Host "  Achyu PACS  -  build" -ForegroundColor Magenta
Write-Host "  Version : $Version"
Write-Host "  Root    : $ScriptDir"

# ── Clean ──────────────────────────────────────────────────────────────────

if ($Clean) {
    Write-Step "Cleaning previous build artefacts"
    $toRemove = @("$ScriptDir\dist", "$ScriptDir\tarang-sender.exe")
    foreach ($p in $toRemove) {
        if (Test-Path $p) {
            Remove-Item $p -Recurse -Force
            Write-OK "Removed $p"
        }
    }
}

# ── Prerequisite checks ────────────────────────────────────────────────────

Write-Step "Checking prerequisites"

if (-not $SkipGo) {
    Assert-Command "go" "Install Go from https://go.dev/dl/"
    $goVer = (go version) -replace "go version go", "" -replace " .*", ""
    Write-OK "Go $goVer"
}

if (-not $SkipElectron) {
    Assert-Command "node" "Install Node.js from https://nodejs.org/"
    Assert-Command "npm"  "npm is bundled with Node.js"
    $nodeVer = node --version
    $npmVer  = npm --version
    Write-OK "Node $nodeVer  /  npm $npmVer"
}

# ── Step 1  -  Build Go binary ───────────────────────────────────────────────

if (-not $SkipGo) {
    Write-Step "Building tarang-sender.exe  (version $Version)"

    $ldflags = "-X main.version=$Version -s -w"
    $outPath  = "$ScriptDir\tarang-sender.exe"

    # Windows 7 compatibility: Go 1.21+ dropped Win7 support, so force the last
    # Win7-capable toolchain (auto-downloaded by the installed Go). Build 32-bit
    # (386) to match the ia32 Electron installer and run on 32- and 64-bit Win7.
    $env:GOTOOLCHAIN  = "go1.20.14"
    $env:GOARCH       = "386"
    $env:CGO_ENABLED  = "0"

    Push-Location $ScriptDir
    try {
        go build -ldflags $ldflags -o $outPath ./cmd/tarang-sender/
        if ($LASTEXITCODE -ne 0) { Write-Fail "go build exited with code $LASTEXITCODE" }
    } finally {
        Pop-Location
    }

    $sizeMB = [math]::Round((Get-Item $outPath).Length / 1MB, 2)
    Write-OK "tarang-sender.exe  ($sizeMB MB)"
}

# ── Step 2  -  npm install ───────────────────────────────────────────────────

if (-not $SkipElectron) {
    Write-Step "Installing Node dependencies"

    Push-Location $ScriptDir
    try {
        npm install --prefer-offline 2>&1 | ForEach-Object { "    $_" } | Write-Host
        if ($LASTEXITCODE -ne 0) { Write-Fail "npm install failed" }
    } finally {
        Pop-Location
    }
    Write-OK "node_modules up to date"

    # ── Step 3  -  Electron Builder ──────────────────────────────────────────

    Write-Step "Packaging Electron app with electron-builder"

    # Stamp the version into package.json temporarily if it differs, so
    # electron-builder picks it up for the installer filename / About screen.
    $pkgPath    = "$ScriptDir\package.json"
    $pkgContent = Get-Content $pkgPath -Raw
    $pkgObj     = $pkgContent | ConvertFrom-Json
    $origVer    = $pkgObj.version
    $versionChanged = $false

    if ($pkgObj.version -ne $Version) {
        $pkgContent = $pkgContent -replace '"version"\s*:\s*"[^"]*"', "`"version`": `"$Version`""
        Set-Content $pkgPath $pkgContent -Encoding utf8
        $versionChanged = $true
        Write-OK "Stamped package.json version → $Version"
    }

    Push-Location $ScriptDir
    try {
        npx electron-builder --win --ia32 2>&1 | ForEach-Object { "    $_" } | Write-Host
        if ($LASTEXITCODE -ne 0) { Write-Fail "electron-builder failed" }
    } finally {
        # Restore original version in package.json.
        if ($versionChanged) {
            $pkgContent = $pkgContent -replace "`"version`": `"$Version`"", "`"version`": `"$origVer`""
            Set-Content $pkgPath $pkgContent -Encoding utf8
        }
        Pop-Location
    }

    # Report output artefacts.
    Write-Step "Build output"
    $distDir = "$ScriptDir\dist"
    if (Test-Path $distDir) {
        Get-ChildItem $distDir -File | ForEach-Object {
            $mb = [math]::Round($_.Length / 1MB, 1)
            Write-OK "$($_.Name)  ($mb MB)"
        }
    }
}

Write-Host ""
Write-Host "  Build complete." -ForegroundColor Green
Write-Host ""
