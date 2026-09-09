#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "Restoring DNS Latency Router on Linux..."

if ! command -v go >/dev/null 2>&1; then
  echo "Go is not installed or not in PATH. Install Go 1.21+ first." >&2
  exit 1
fi

if [ ! -f "config.yaml" ]; then
  echo "config.yaml is missing. Restore or create it before building." >&2
  exit 1
fi

go version
go mod tidy
go build -o dns-latency-router .
mkdir -p logs

echo
echo "Build complete: dns-latency-router"
echo "Direct run: ./dns-latency-router"
echo "PM2 run: pm2 start ecosystem.config.js && pm2 save"
