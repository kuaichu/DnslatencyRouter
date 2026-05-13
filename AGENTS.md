# DNS Latency Router — AI Context

## Project Overview

A Go tool that periodically resolves a target domain from multiple DNS servers (one per Chinese ISP), TCP-pings every discovered IP, and updates a Cloudflare DNS A record to point to the fastest one. Includes a web dashboard with real-time status, latency chart, and live config editing.

**Executable**: `dns-latency-router` (Linux) / `dns-latency-router.exe` (Windows)
**Language**: Go 1.21
**Dependencies**: `gopkg.in/yaml.v3` (only external dep)

---

## Directory Structure

```
dns-latency-router/
├── main.go                           # Entry point, loop logic
├── config.yaml                       # User configuration
├── go.mod                            # Go module file
├── README.md                         # Human-readable docs
├── AGENTS.md                         # AI context (this file)
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
2. If `cfg.WebPort > 0`: starts web server, redirects log output to SSE broadcaster
3. Creates Cloudflare client, verifies API access with `GetRecord()`
4. Runs `runOnce()` immediately, then on `cfg.CheckInterval` ticker
5. After each cycle: updates web status + appends history record
6. Traps `os.Interrupt` for graceful shutdown

`runOnce(cfg, cf, ws)` — returns `*checker.Result` or nil on failure:
1. **Resolve**: calls `checker.ResolveFromAllDNS()` using multi-DNS → deduplicated IP list
2. **Ping**: calls `checker.PingAll()` concurrent TCP ping on `cfg.PingPort` with `cfg.PingTimeout`
3. **Select**: picks the first `Result` with no error (sorted by latency, fastest first)
4. **Update**: calls `cf.UpdateRecord(best.IP)` — skips API call if IP unchanged
5. Returns best result (nil if all failed); caller logs errors and continues loop

Web config callback (closure over `cfg` pointer):
- When user saves via `/api/config`, callback updates `cfgPtr.TargetDomain` and `cfgPtr.CustomDomain`
- Changes take effect on next `runOnce` cycle (no restart needed)

### Web Server (`internal/web/server.go`)

**`New(port int, cfgPath string, onConfig func(targetDomain, customDomain string)) *Server`**
- Creates server with config path (for persistence) and callback (for runtime update)

**Embedded HTML**: Uses `//go:embed dashboard.html` to bundle the entire frontend (inline CSS/JS, no external deps)

**SSE** (`/api/events`): 
- `status` event — full Status struct JSON (sent on connect + on every update)
- `history` event — CheckRecord array (sent on connect + on new record)
- `log` event — raw log line string
- Keepalive ping every 30s

**API endpoints**:
| Method | Path | Description |
|--------|------|-------------|
| GET | `/` | Dashboard HTML |
| GET | `/api/status` | Current status JSON |
| GET | `/api/history` | Check history JSON |
| GET | `/api/config` | Current domain config |
| POST | `/api/config` | Update target/custom domain |
| GET | `/api/events` | SSE stream |

**Config update flow** (`handleAPIConfig`):
1. Parse JSON body `{target_domain?, custom_domain?}`
2. Call `config.UpdateYAMLField()` for each changed field (line-based replacement preserves YAML comments)
3. Call `onConfig` callback to update in-memory config
4. Update web status + broadcast via SSE
5. Log the change

**`UpdateYAMLField(path, key, value)`** (in config.go):
- Reads config.yaml as lines
- Finds line matching `key:` prefix
- Replaces with `key: "value"` (preserving indentation)
- Writes back to file

### Config (`internal/config/config.go`)

```go
type Config struct {
    Cloudflare       CloudflareConfig  // api_token, zone_id, record_id
    TargetDomain     string
    CustomDomain     string
    CheckIntervalSec int               // default: 300
    PingPort         int               // default: 443
    PingTimeoutSec   int               // default: 5
    DNSServers       []string          // default: [114.114.114.114, 223.5.5.5, ...]
    WebPort          int               // default: 0 (disabled)

    PingTimeout   time.Duration        // derived: PingTimeoutSec * Second
    CheckInterval time.Duration        // derived: CheckIntervalSec * Second
}
```

Validation rules:
- `cloudflare.api_token`, `zone_id`, `record_id` — all required (fail fast)
- `target_domain` — required
- `custom_domain` — NOT validated (can be empty)
- YAML defaults applied before Unmarshal, so config fields are optional

### Checker (`internal/checker/checker.go`)

**`ResolveFromAllDNS(domain string, dnsServers []string) ([]string, error)`**
- Goroutine per DNS server, 5s timeout, custom dial to `dnsServer:53`
- Deduplicates across all servers, returns unique IPs
- Error only if ALL servers fail

**`PingAll(ips []string, port int, timeout time.Duration) []Result`**
- Goroutine per IP, TCP `net.DialTimeout`, closes immediately
- Sorted: successful first (ascending latency), failed last (sorted by IP)
- `Result`: `IP string`, `Latency time.Duration`, `Err error`

### Cloudflare Client (`internal/cloudflare/cloudflare.go`)

- Base URL: `https://api.cloudflare.com/client/v4`
- **`GetRecord()`** — GET current record state (Type, Name, Content, TTL, Proxied)
- **`UpdateRecord(ip)`** — PATCH with preserved Type/Name/TTL/Proxied, skip if Content unchanged
- Error format: `[code] message` (10000 = Auth error, usually missing DNS:Edit permission)

### Web UI (`internal/web/dashboard.html`)

Self-contained HTML with inline CSS + JS (zero external dependencies):

- **Dark theme** (GitHub-dark inspired)
- **4 status cards**: target domain, current IP, latency (color-coded), countdown timer
- **Latency chart**: Canvas-based, Catmull-Rom to cubic bezier smooth curves, gradient fill, time X-axis
- **Hover tooltip**: mousemove hit detection, shows time + latency + IP in bordered box with dashed guide line
- **Settings modal**: gear button in header, edit target/custom domain, POST /api/config, toast notification
- **Real-time log**: SSE stream, color-coded tags `[check]/[ping]/[update]/[error]`, auto-scroll
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

check_interval: 300
ping_port: 443
ping_timeout_seconds: 5
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
| All DNS servers fail | Error logged, skipped cycle, ticker continues |
| All pings fail | Nil result, skipped cycle, ticker continues |
| Cloudflare API fails | Error logged, ticker continues |
| IP unchanged | `UpdateRecord` skips PATCH (compares Content) |
| Config missing required fields | `log.Fatalf` — immediate exit |
| Invalid YAML | Parse error — `log.Fatalf` |
| Config changed via web UI | Written to config.yaml + in-memory update, next cycle picks up changes |
| Web dashboard disabled | Set `web_port: 0`, no HTTP server started |
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
3. **Web UI not reachable**: Check server firewall + cloud security group for `web_port`
4. **Config changes not persisting**: Ensure write permission for config.yaml directory
5. **PM2 shows 0b memory**: Binary likely missing execute permission (`chmod +x`)
