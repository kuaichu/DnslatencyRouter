$ErrorActionPreference = "Stop"

$projectRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $projectRoot

Write-Host "Restoring DNS Latency Router on Windows..."

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go is not installed or not in PATH. Install Go 1.21+ first."
}

if (-not (Test-Path -LiteralPath ".\\config.yaml")) {
    throw "config.yaml is missing. Restore or create it before building."
}

go version
go mod tidy
go build -o dns-latency-router.exe .

Write-Host ""
Write-Host "Build complete: dns-latency-router.exe"
Write-Host "Next step: edit config.yaml if needed, then run .\\dns-latency-router.exe"
