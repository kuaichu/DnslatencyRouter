#!/bin/bash
set -e

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$PROJECT_DIR"

echo "=== Building DNS Latency Router ==="

# Linux (amd64) — controller for PM2/systemd deployment
echo "[1/4] Building controller for linux/amd64 ..."
# Use conservative Linux codegen for deployment stability on older VPS CPUs / kernels.
GOOS=linux GOARCH=amd64 go build -gcflags="all=-N -l" -o dns-latency-router .
echo "  -> dns-latency-router"

# Windows (amd64) — controller
echo "[2/4] Building controller for windows/amd64 ..."
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dns-latency-router.exe .
echo "  -> dns-latency-router.exe"

# Linux (amd64) — agent
echo "[3/4] Building agent for linux/amd64 ..."
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dns-latency-router-agent ./cmd/dlr-agent
echo "  -> dns-latency-router-agent"

# Windows (amd64) — agent
echo "[4/4] Building agent for windows/amd64 ..."
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dns-latency-router-agent.exe ./cmd/dlr-agent
echo "  -> dns-latency-router-agent.exe"

echo "=== Done ==="
echo ""
echo "Controller Linux:   ./dns-latency-router"
echo "Controller Windows: dns-latency-router.exe"
echo "Agent Linux:        ./dns-latency-router-agent"
echo "Agent Windows:      dns-latency-router-agent.exe"
echo ""
echo "PM2 deploy: pm2 start ecosystem.config.js"
