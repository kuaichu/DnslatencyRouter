#!/usr/bin/env bash
set -euo pipefail

REPO="${REPO:-kuaichu/DnslatencyRouter}"
CONTROLLER_URL="${CONTROLLER_URL:-}"
TOKEN="${TOKEN:-}"
INSTALL_DIR="${INSTALL_DIR:-/opt/dns-latency-router-agent}"
CONFIG_FILE="${CONFIG_FILE:-$INSTALL_DIR/agent.yaml}"
SERVICE_NAME="${SERVICE_NAME:-dns-latency-router-agent}"
BIN_NAME="${BIN_NAME:-dns-latency-router-agent}"
LAUNCHD_LABEL="${LAUNCHD_LABEL:-com.dns-latency-router.agent}"
AGENT_ID="${AGENT_ID:-}"
AGENT_NAME="${AGENT_NAME:-}"
PROBE_SOURCE="${PROBE_SOURCE:-}"
CARRIER="${CARRIER:-auto}"
REPORT_INTERVAL="${REPORT_INTERVAL:-300}"
DOWNLOAD_URL="${DOWNLOAD_URL:-}"
DOWNLOAD_PRIORITY="${DOWNLOAD_PRIORITY:-controller}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --id) AGENT_ID="$2"; shift 2 ;;
    --name) AGENT_NAME="$2"; shift 2 ;;
    --probe-source|--region) PROBE_SOURCE="$2"; shift 2 ;;
    --carrier) CARRIER="$2"; shift 2 ;;
    --controller) CONTROLLER_URL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --interval) REPORT_INTERVAL="$2"; shift 2 ;;
    --download-url) DOWNLOAD_URL="$2"; shift 2 ;;
    --controller-first) DOWNLOAD_PRIORITY=controller; shift ;;
    --github-first) DOWNLOAD_PRIORITY=github; shift ;;
    --no-controller-fallback) DOWNLOAD_PRIORITY=github; shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

if [ "$(id -u)" -ne 0 ]; then
  echo "please run as root" >&2
  exit 1
fi

if [ -z "$CONTROLLER_URL" ]; then
  echo "--controller is required" >&2
  exit 2
fi

if [ -z "$TOKEN" ]; then
  echo "--token is required" >&2
  exit 2
fi

CONTROLLER_URL="${CONTROLLER_URL%/}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$os/$arch" in
  linux/x86_64|linux/amd64)
    platform="linux-amd64"
    asset="dns-latency-router-agent-$platform"
    ;;
  linux/aarch64|linux/arm64)
    platform="linux-arm64"
    asset="dns-latency-router-agent-$platform"
    ;;
  darwin/x86_64|darwin/amd64)
    platform="darwin-amd64"
    asset="dns-latency-router-agent-$platform"
    ;;
  darwin/arm64|darwin/aarch64)
    platform="darwin-arm64"
    asset="dns-latency-router-agent-$platform"
    ;;
  *)
    echo "unsupported system: $os/$arch" >&2
    exit 1
    ;;
esac

GITHUB_DOWNLOAD_URL="https://github.com/$REPO/releases/latest/download/$asset"
CONTROLLER_DOWNLOAD_URL="$CONTROLLER_URL/api/agent/download/$platform"

download() {
  url="$1"
  out="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 3 --connect-timeout 15 "$url" -o "$out"
  elif command -v wget >/dev/null 2>&1; then
    wget -O "$out" "$url"
  else
    echo "curl or wget is required" >&2
    exit 1
  fi
}

yaml_quote() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/''/g")"
}

yaml_read_agent_value() {
  key="$1"
  [ -f "$CONFIG_FILE" ] || return 0
  sed -n "s/^[[:space:]]*$key:[[:space:]]*['\"]\\{0,1\\}\\([^'\"]*\\)['\"]\\{0,1\\}[[:space:]]*$/\\1/p" "$CONFIG_FILE" | head -n 1
}

host="$(hostname -s 2>/dev/null || hostname 2>/dev/null || echo agent)"
if [ -z "$AGENT_ID" ]; then AGENT_ID="$(yaml_read_agent_value id || true)"; fi
if [ -z "$AGENT_NAME" ]; then AGENT_NAME="$(yaml_read_agent_value name || true)"; fi
if [ -z "$PROBE_SOURCE" ]; then PROBE_SOURCE="$(yaml_read_agent_value probe_source || true)"; fi
if [ "$CARRIER" = "auto" ]; then
  existing_carrier="$(yaml_read_agent_value carrier || true)"
  if [ -n "$existing_carrier" ]; then CARRIER="$existing_carrier"; fi
fi
if [ -z "$AGENT_ID" ]; then
  suffix="$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 6 || true)"
  if [ -z "$suffix" ]; then suffix="$(date +%s)"; fi
  AGENT_ID="dlr-$host-$suffix"
fi

AGENT_ID="$(printf '%s' "$AGENT_ID" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9._-]/-/g')"
if [ -z "$AGENT_NAME" ]; then AGENT_NAME="$host"; fi
if [ -z "$PROBE_SOURCE" ]; then PROBE_SOURCE="$AGENT_NAME"; fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

if [ -n "$DOWNLOAD_URL" ]; then
  echo "Downloading agent from custom URL..."
  download "$DOWNLOAD_URL" "$tmp"
elif [ "$DOWNLOAD_PRIORITY" = "github" ]; then
  echo "Downloading agent from GitHub release..."
  if ! download "$GITHUB_DOWNLOAD_URL" "$tmp"; then
    echo "GitHub release download failed; falling back to controller download..."
    download "$CONTROLLER_DOWNLOAD_URL" "$tmp"
  fi
else
  echo "Downloading agent from controller..."
  if ! download "$CONTROLLER_DOWNLOAD_URL" "$tmp"; then
    echo "Controller download failed; falling back to GitHub release..."
    download "$GITHUB_DOWNLOAD_URL" "$tmp"
  fi
fi

mkdir -p "$INSTALL_DIR"
install -m 0755 "$tmp" "$INSTALL_DIR/$BIN_NAME"

{
  echo "node_role: agent"
  echo "agent:"
  echo "  id: $(yaml_quote "$AGENT_ID")"
  echo "  name: $(yaml_quote "$AGENT_NAME")"
  echo "  controller_url: $(yaml_quote "$CONTROLLER_URL")"
  echo "  token: $(yaml_quote "$TOKEN")"
  echo "  probe_source: $(yaml_quote "$PROBE_SOURCE")"
  echo "  carrier: $(yaml_quote "$CARRIER")"
  echo "  report_interval_seconds: $REPORT_INTERVAL"
  echo "ping_mode: icmp"
  echo "ping_port: 443"
  echo "ping_timeout_seconds: 5"
  echo "ping_attempts: 4"
} > "$CONFIG_FILE"
chmod 0600 "$CONFIG_FILE"

if [ "$os" = "linux" ]; then
  cat > "/etc/systemd/system/$SERVICE_NAME.service" <<UNIT
[Unit]
Description=DNS Latency Router Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/$BIN_NAME $CONFIG_FILE
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
UNIT

  systemctl daemon-reload
  systemctl enable --now "$SERVICE_NAME"
  sleep 2
  systemctl --no-pager --full status "$SERVICE_NAME" || true
elif [ "$os" = "darwin" ]; then
  plist="/Library/LaunchDaemons/$LAUNCHD_LABEL.plist"
  cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$LAUNCHD_LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$INSTALL_DIR/$BIN_NAME</string>
    <string>$CONFIG_FILE</string>
  </array>
  <key>WorkingDirectory</key>
  <string>$INSTALL_DIR</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>$INSTALL_DIR/agent.log</string>
  <key>StandardErrorPath</key>
  <string>$INSTALL_DIR/agent.err</string>
</dict>
</plist>
PLIST
  chown root:wheel "$plist"
  chmod 0644 "$plist"
  launchctl bootout system "$plist" >/dev/null 2>&1 || true
  launchctl bootstrap system "$plist"
  launchctl enable "system/$LAUNCHD_LABEL"
  launchctl kickstart -k "system/$LAUNCHD_LABEL"
  sleep 2
  launchctl print "system/$LAUNCHD_LABEL" || true
else
  echo "service installation is not implemented for $os" >&2
  exit 1
fi

echo
echo "Installed DNS Latency Router agent:"
echo "  id: $AGENT_ID"
echo "  name: $AGENT_NAME"
echo "  controller: $CONTROLLER_URL"
echo "Open the controller dashboard and edit region/carrier in Agent."
