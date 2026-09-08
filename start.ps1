# start.ps1 — Start TARANG Sender (Electron desktop app + Go backend)
# The Electron process manages the Go backend automatically.
# Usage: .\start.ps1

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

if (!(Get-Command node -ErrorAction SilentlyContinue)) {
    Write-Host "Node.js not found. Install from https://nodejs.org" -ForegroundColor Red
    exit 1
}

if (!(Test-Path "node_modules")) {
    Write-Host "Installing dependencies..." -ForegroundColor Cyan
    npm install
}

Write-Host "Starting TARANG Sender..." -ForegroundColor Cyan
npm start
