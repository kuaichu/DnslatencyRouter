#!/bin/bash
set -e

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$PROJECT_DIR"

echo "=== Building dns-latency-router ==="

# Linux (amd64) — for PM2 deployment
echo "[1/2] Building for linux/amd64 ..."
# Use conservative Linux codegen for deployment stability on older VPS CPUs / kernels.
GOOS=linux GOARCH=amd64 go build -gcflags="all=-N -l" -o dns-latency-router ./main.go
echo "  -> dns-latency-router"

# Windows (amd64)
echo "[2/2] Building for windows/amd64 ..."
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dns-latency-router.exe ./main.go
echo "  -> dns-latency-router.exe"

echo "=== Done ==="
echo ""
echo "Linux:   ./dns-latency-router"
echo "Windows: dns-latency-router.exe"
echo ""
echo "PM2 deploy: pm2 start ecosystem.config.js"
