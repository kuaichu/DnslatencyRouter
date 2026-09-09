#!/bin/bash
set -e

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$PROJECT_DIR"

echo "=== Building DNS Latency Router ==="

# Linux (amd64) — controller for PM2/systemd deployment
echo "[1/6] Building controller for linux/amd64 ..."
# Use conservative Linux codegen for deployment stability on older VPS CPUs / kernels.
GOOS=linux GOARCH=amd64 go build -gcflags="all=-N -l" -o dns-latency-router .
echo "  -> dns-latency-router"

# Windows (amd64) — controller
echo "[2/6] Building controller for windows/amd64 ..."
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dns-latency-router.exe .
echo "  -> dns-latency-router.exe"

# Linux (amd64) — agent
echo "[3/6] Building agent for linux/amd64 ..."
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dns-latency-router-agent ./cmd/dlr-agent
echo "  -> dns-latency-router-agent"

# Linux (arm64) — agent
echo "[4/6] Building agent for linux/arm64 ..."
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o dns-latency-router-agent-linux-arm64 ./cmd/dlr-agent
echo "  -> dns-latency-router-agent-linux-arm64"

# macOS (arm64) — agent
echo "[5/6] Building agent for darwin/arm64 ..."
GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o dns-latency-router-agent-darwin-arm64 ./cmd/dlr-agent
echo "  -> dns-latency-router-agent-darwin-arm64"

# Windows (amd64) — agent
echo "[6/6] Building agent for windows/amd64 ..."
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dns-latency-router-agent.exe ./cmd/dlr-agent
echo "  -> dns-latency-router-agent.exe"

echo "=== Done ==="
echo ""
echo "Controller Linux:   ./dns-latency-router"
echo "Controller Windows: dns-latency-router.exe"
echo "Agent Linux:        ./dns-latency-router-agent"
echo "Agent Linux arm64:  ./dns-latency-router-agent-linux-arm64"
echo "Agent macOS arm64:  ./dns-latency-router-agent-darwin-arm64"
echo "Agent Windows:      dns-latency-router-agent.exe"
echo ""
echo "PM2 deploy: pm2 start ecosystem.config.js"
