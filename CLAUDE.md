# DNS Latency Router — AI Context

## Project Overview

A Go tool that periodically resolves a target domain from multiple DNS servers (one per Chinese ISP), pings every discovered IP via ICMP or TCP, and updates a Cloudflare DNS A record to point to the fastest one. Includes a web dashboard with real-time status, latency chart, live config editing, and proxy support for Cloudflare API.

**Executable**: `dns-latency-router` (Linux) / `dns-latency-router.exe` (Windows)
**Language**: Go 1.21+
**Dependencies**: `gopkg.in/yaml.v3`, `golang.org/x/net` (SOCKS5 proxy)

---

## Directory Structure

```
dns-latency-router/
├── main.go                           # Entry point, loop logic
├── config.yaml                       # User configuration
├── go.mod                            # Go module file
├── README.md                         # Human-readable docs
├── CLAUDE.md                         # AI context (this file)
├── ecosystem.config.js               # PM2 process manager config (Linux)
├── build.sh                          # Cross-platform build script
├── .gitignore
├── dns-latency-router.exe            # Compiled Windows binary
├── dns-latency-router                # Compiled Linux binary (git-ignored)
├── logs/                             # PM2 log output (git-ignored)
└── internal/
    ├── config/config.go              # Config parsing, validation, YAML field update
    ├── checker/checker.go            # Multi-DNS resolution + TCP ping
    ├── cloudflare/cloudflare.go      # Cloudflare API client (GET/PATCH DNS records)
    └── web/
        ├── server.go                 # HTTP server, SSE, config API, embed template
        └── dashboard.html            # Web UI template (embedded via go:embed)
```

---

## Code Architecture

### Entry Point (`main.go`)

`main()`:
1. Loads config via `config.Load()` — validates required fields on startup
2. Creates `triggerCh` channel for manual check triggering
3. If `cfg.WebPort > 0`: starts web server, redirects log output to SSE broadcaster
4. Creates Cloudflare client with proxy URL, verifies API access (non-fatal, logs warning on failure)
5. Sets initial web status (including PingMode, PingPort)
6. Runs `runOnce()` immediately, then on `cfg.CheckInterval` timer (Timer, not Ticker — allows dynamic interval changes)
7. After each cycle: updates web status + appends history record (even on failure, timestamps always set)
8. Listens on `triggerCh` for manual check requests from web UI
9. Traps `os.Interrupt` for graceful shutdown

`runOnce(cfg, cf, ws)` — returns `*checker.Result` or nil on failure:
1. **Resolve**: calls `checker.ResolveFromAllDNS()` using multi-DNS → deduplicated IP list
2. **Ping**: calls `checker.PingAll(ips, mode, port, timeout)` — mode "icmp" or "tcp" from `cfg.PingMode`
3. **Select**: picks the first `Result` with no error and latency ≥ `cfg.PingMinThreshold`
4. **Update**: calls `cf.UpdateRecord(best.IP)` — skips API call if IP unchanged
5. Returns best result (nil if all failed); caller logs errors and continues loop

`runCheckCycle(cfg, cf, ws, nextCheck)` — wraps one check cycle:
- Calls `runOnce()`, then updates web Status and History with full timestamps regardless of outcome
- Preserves `DiscoveredCount` from the mid-cycle update in `runOnce()`

Web config callback (closure over `cfg` pointer):
- When user saves via `/api/config`, callback updates `cfgPtr.TargetDomain`, `cfgPtr.CustomDomain`, `cfgPtr.PingMode`, `cfgPtr.PingPort`, `cfgPtr.CheckIntervalSec` (and derived `cfgPtr.CheckInterval`)
- Changes take effect on next cycle (no restart needed); Timer reset picks up new interval

### Web Server (`internal/web/server.go`)

**`New(port int, cfgPath string, triggerCh chan<- struct{}, onConfig func(...)) *Server`**
- Creates server with config path, manual-check trigger channel, and callback for runtime config updates
- `onConfig` signature: `func(targetDomain, customDomain, pingMode string, pingPort, checkInterval int)`

**Embedded HTML**: Uses `//go:embed dashboard.html` to bundle the entire frontend (inline CSS/JS, no external deps)

**SSE** (`/api/events`): 
- `reset` event — signals client to clear log buffer (sent on connect, before buffered logs)
- `status` event — full Status struct JSON (sent on connect + on every update)
- `history` event — CheckRecord array (sent on connect + on new record)
- `log` event — raw log line string (sent on connect: all buffered logs; then live)
- Keepalive ping every 30s

**Log Buffer**: Server stores last 200 log lines in memory (`logBuf`). On SSE connect, sends `reset` event then replays all buffered logs so page refresh doesn't lose log history. Incoming `reset` event triggers `logBody.innerHTML = ''` on frontend.

**API endpoints**:
| Method | Path | Description |
|--------|------|-------------|
| GET | `/` | Dashboard HTML |
| GET | `/api/status` | Current status JSON |
| GET | `/api/history` | Check history JSON |
| GET | `/api/config` | Current config (domains, ping_mode, ping_port, check_interval) |
| POST | `/api/config` | Update config fields (partial update, only changed fields) |
| POST | `/api/check` | Trigger immediate check cycle (non-blocking, returns 200 or "already in progress") |
| GET | `/api/events` | SSE stream |

**Status struct fields**: TargetDomain, CustomDomain, CurrentIP, Latency, LastCheck, NextCheck, IsRunning, DiscoveredCount, CheckIntervalSec, PingMode, PingPort

**Config update flow** (`handleAPIConfig`):
1. Parse JSON body `{target_domain?, custom_domain?, ping_mode?, ping_port?, check_interval?}`
2. Validate: ping_mode must be "icmp" or "tcp", ping_port > 0, check_interval >= 10
3. Call `config.UpdateYAMLField()` for each changed field (line-based replacement preserves YAML comments; `quoted: true` for strings, `quoted: false` for ints)
4. Call `onConfig` callback to update in-memory config
5. Update web status + broadcast via SSE
6. Log the change

**`UpdateYAMLField(path, key, value, quoted)`** (in config.go):
- Reads config.yaml as lines
- Finds line matching `key:` prefix
- If `quoted`: replaces with `key: "value"` (string fields); otherwise `key: value` (int fields)
- Preserves indentation, writes back to file

### Config (`internal/config/config.go`)

```go
type Config struct {
    Cloudflare         CloudflareConfig  // api_token, zone_id, record_id
    TargetDomain       string
    CustomDomain       string
    ProxyURL           string            // SOCKS5/HTTP proxy for Cloudflare API (e.g. socks5://x.x.x.x:port or http://x.x.x.x:port)
    CheckIntervalSec   int               // default: 300
    PingMode           string            // "icmp" (default) or "tcp"
    PingPort           int               // default: 443 (only used in TCP mode)
    PingTimeoutSec     int               // default: 5
    PingMinThresholdMs float64           // default: 1 (skip results with latency below this)
    DNSServers         []string          // default: [114.114.114.114, 223.5.5.5, ...]
    WebPort            int               // default: 0 (disabled)

    PingTimeout      time.Duration      // derived: PingTimeoutSec * Second
    CheckInterval    time.Duration      // derived: CheckIntervalSec * Second
    PingMinThreshold time.Duration      // derived: PingMinThresholdMs * Millisecond
}
```

Validation rules:
- `cloudflare.api_token`, `zone_id`, `record_id` — all required (fail fast)
- `target_domain` — required
- `custom_domain` — NOT validated (can be empty)
- `proxy_url` — optional; supports `http://`, `https://`, `socks5://` schemes (HTTP uses CONNECT tunnel)
- `ping_mode` — optional, defaults to "icmp"; valid values: "icmp", "tcp"
- YAML defaults applied before Unmarshal, so config fields are optional

### Checker (`internal/checker/checker.go`)

**`ResolveFromAllDNS(domain string, dnsServers []string) ([]string, error)`**
- Goroutine per DNS server, 5s timeout, custom dial to `dnsServer:53`
- Deduplicates across all servers, returns unique IPs
- Error only if ALL servers fail

**`PingAll(ips []string, mode string, port int, timeout time.Duration) []Result`**
- `mode`: "icmp" or "tcp"
- **ICMP mode**: sends ICMP Echo Request via raw socket (`ip4:icmp`), matches reply by ID+sequence number, measures RTT. Requires root / `CAP_NET_RAW`
- **TCP mode**: concurrent `net.DialTimeout` to `ip:port`, closes immediately
- Sorted: successful first (ascending latency), failed last (sorted by IP)
- `Result`: `IP string`, `Latency time.Duration`, `Err error`

**`pingICMP(ip string, timeout time.Duration) Result`**
- Opens `net.ListenPacket("ip4:icmp", ...)`, sends 8-byte ICMP Echo Request with random ID + atomic sequence number
- Builds ICMP checksum manually (RFC 792)
- Filters received packets by source IP, ICMP type (0 = Echo Reply), ID, and sequence number
- Uses per-goroutine listener to avoid cross-contamination in concurrent pings

### Cloudflare Client (`internal/cloudflare/cloudflare.go`)

- Base URL: `https://api.cloudflare.com/client/v4`
- **`New(apiToken, zoneID, recordID, proxyURL string)`** — creates client with optional proxy
  - `proxyURL`: supports `http://`, `https://` (HTTP CONNECT proxy), and `socks5://` (SOCKS5 proxy)
  - Uses `golang.org/x/net/proxy` for SOCKS5, `http.ProxyURL` for HTTP
- **`GetRecord()`** — GET current record state (Type, Name, Content, TTL, Proxied)
- **`UpdateRecord(ip)`** — PATCH with preserved Type/Name/TTL/Proxied, skip if Content unchanged
- Error format: `[code] message` (10000 = Auth error, usually missing DNS:Edit permission)

### Web UI (`internal/web/dashboard.html`)

Self-contained HTML with inline CSS + JS (zero external dependencies):

- **Dark theme** (GitHub-dark inspired)
- **Header**: "Check Now" button (green, with 2s cooldown), gear settings button, status badge (running/error/disconnected)
- **4 status cards**: target domain (with discovered IP count), current IP (with custom domain), latency (color-coded: green <150ms, yellow <300ms, red >300ms), countdown timer (live seconds)
- **Latency chart**: Canvas-based, Catmull-Rom to cubic bezier smooth curves, gradient fill, time X-axis, Y-axis labels in `#8b949e`, grid lines semi-transparent
- **Hover tooltip**: mousemove hit detection (10px radius), shows time + latency + IP in bordered box with dashed guide line
- **Settings modal**: gear button in header
  - Target domain / Custom domain (text inputs)
  - Ping mode (dropdown: ICMP / TCP) — **linked**: selecting ICMP auto-disables port input (greyed out, no pointer)
  - Ping port (number, only for TCP mode, hidden/disabled for ICMP)
  - Check interval (number, min 10s)
  - Save button with loading state, POST to `/api/config`, toast notification
- **Real-time log**: SSE stream with 200-line server buffer (refresh-safe)
  - Color-coded tags: `[check]` blue, `[ping]` green, `[update]` yellow, `[error]` red
  - **Error lines**: entire line gets red text + left red border + faint red background for high visibility
  - Auto-scroll to bottom
  - Reset on SSE reconnect (clear DOM before replay)
- **Responsive**: 4-column → 2-column → 1-column grid

---

## Configuration File (`config.yaml`)

```yaml
cloudflare:
  api_token: ""
  zone_id: ""
  record_id: ""

target_domain: ""
custom_domain: ""

# Optional: HTTP/SOCKS5 proxy for Cloudflare API (use http:// for HTTP CONNECT proxy)
proxy_url: ""

check_interval: 300
ping_mode: "icmp"       # "icmp" (default, no port needed) or "tcp"
ping_port: 443          # only used when ping_mode=tcp
ping_timeout_seconds: 5
ping_min_threshold_ms: 1  # skip results below this latency
web_port: 19198

dns_servers:
  - 114.114.114.114
  - 223.5.5.5
  - 119.29.29.29
  - 180.76.76.76
  - 8.8.8.8
```

Config path overridable via CLI arg: `./dns-latency-router myconfig.yaml`.

---

## Behavior & Edge Cases

| Scenario | Behavior |
|----------|----------|
| Target domain has 1 IP | Resolves, pings, updates (or skips if same IP) |
| Target domain has N IPs | Discovers all via multi-DNS, pings all concurrently, picks fastest |
| All DNS servers fail | Error logged, skipped cycle, timer continues |
| All pings fail (ICMP timeout / TCP refused) | Nil result, status updated with timestamps, history records failure, timer continues |
| Cloudflare API fails on startup | Warning logged (non-fatal), continues to first cycle, retries on each update |
| Cloudflare API fails during cycle | Error logged, best result still returned for web status, timer continues |
| IP unchanged | `UpdateRecord` skips PATCH (compares Content) |
| Proxy configured (proxy_url) | Cloudflare client uses SOCKS5 or HTTP CONNECT proxy for all API calls |
| Config missing required fields | `log.Fatalf` — immediate exit |
| Invalid YAML | Parse error — `log.Fatalf` |
| Config changed via web UI | Written to config.yaml + in-memory update (PingMode/PingPort/CheckInterval), next cycle picks up changes; Timer reset for new interval |
| Manual check triggered (web UI) | Sends on `triggerCh` (non-blocking), resets Timer to full interval |
| Web dashboard disabled | Set `web_port: 0`, no HTTP server started |
| SSE reconnect / page refresh | Server sends buffered status, history, and last 200 log lines; frontend clears log body on `reset` event |
| ICMP mode selected | Port input disabled in UI, pings via raw ICMP socket (no port needed) |
| SIGINT/Ctrl+C | Clean exit via signal handler |

---

## Cloudflare API Notes

- PATCH endpoint preserves unmodified fields
- Rate limit: 1200 requests/5min on free plan (~2/cycle = ~24/hour, well within limits)

---

## Build & Run

```bash
# One-shot both platforms
chmod +x build.sh && ./build.sh

# Windows
go build -o dns-latency-router.exe .
.\dns-latency-router.exe

# Linux + PM2
./build.sh
mkdir -p logs && pm2 start ecosystem.config.js
pm2 save
```

PM2: auto-restart on crash (10 retries, 5s delay), logs in `./logs/`, no watch mode.

---

## Common Issues

1. **Token works for GET but fails for PATCH** ([10000]): Token has DNS:Read not DNS:Edit
2. **All DNS servers fail**: UDP 53 blocked or DNS hijacking
3. **Cloudflare API unreachable (EOF / TLS handshake timeout)**: GFW DPI blocks TLS to Cloudflare IPs. Configure `proxy_url` with a working HTTP or SOCKS5 proxy
4. **ICMP ping fails (permission denied)**: ICMP requires raw sockets. Ensure binary runs as root or has `CAP_NET_RAW` capability: `setcap cap_net_raw+ep dns-latency-router`
5. **Web UI not reachable**: Check server firewall + cloud security group for `web_port`
6. **Config changes not persisting**: Ensure write permission for config.yaml directory
7. **PM2 shows 0b memory**: Binary likely missing execute permission (`chmod +x`)
8. **TCP ping shows "connection refused" on all IPs**: The target service port is not open; switch to ICMP mode or verify the port number
