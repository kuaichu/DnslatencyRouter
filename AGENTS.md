# DNS Latency Router — AI Context

## Project Overview

A Go service that resolves one or more airport entry domains from carrier-aware DNS pools, probes every discovered IP, scores them by latency/jitter/loss, and updates Cloudflare DNS A records only when candidates are meaningfully better and stable long enough. It supports both legacy single-entry routing and multi-airport / multi-region routing. It includes a self-contained Web dashboard for status, history, logs, IP analytics, live config editing, and local SVG flag display.

**Executable**: `dns-latency-router` / `dns-latency-router.exe`  
**Language**: Go 1.21  
**External dependency**: `gopkg.in/yaml.v3`

## Directory Structure

```text
dns-latency-router/
├── main.go
├── config.yaml
├── go.mod
├── README.md
├── AGENTS.md
├── ecosystem.config.js
├── build.sh
├── internal/
│   ├── checker/
│   │   └── checker.go
│   ├── cloudflare/
│   │   └── cloudflare.go
│   ├── config/
│   │   └── config.go
│   └── web/
│       ├── assets/
│       │   └── flags/
│       ├── dashboard.html
│       ├── persistence.go
│       └── server.go
└── data/
    ├── runtime-history.json
    ├── runtime-logs.jsonl
    └── runtime-samples.json
```

## Runtime Flow

`main()`:

1. Load config and apply defaults
2. Start the web server if `web_port > 0`
3. Build Cloudflare client with optional proxy
4. Run one check immediately
5. Continue on `check_interval` ticker or manual web trigger
6. Persist status/history/samples/logs through `internal/web`

`runOnce()`:

1. Resolve all candidate IPs from the effective carrier DNS pool
2. Mark current active IP set in web state
3. Probe each IP using ICMP or TCP, multiple attempts per IP
4. Compute latency/jitter/loss/score
5. Select the best candidate above `ping_min_threshold_ms`
6. Compare with current Cloudflare record using hysteresis
7. Update Cloudflare only if improvement and stability conditions are met

## Important Config Fields

```yaml
target_domain:
custom_domain:
probe_source:
carrier:
proxy_url:
ping_mode:
ping_port:
ping_timeout_seconds:
ping_attempts:
ping_min_threshold_ms:
selection_latency_weight:
selection_jitter_weight:
selection_loss_weight:
switch_improvement_percent:
switch_stable_seconds:
time_penalty_start_hour:
time_penalty_end_hour:
time_penalty_score:
time_penalty_org_keywords:
check_interval:
web_port:
dns_servers:
```

`carrier` supports `auto`, `unicom`, `telecom`, `mobile`, and `all`. `auto` infers the effective carrier from `probe_source`; `all` uses the configured `dns_servers` list directly. Carrier strategy only changes the resolver pool used to discover candidate IPs. The actual route still uses local latency/jitter/loss scoring and Cloudflare still stores one global A record.

Time-based weighting can penalize specific ISP / IDC labels during configured local-hour windows. This is intended for cases like Google Anycast behaving well during the day but becoming volatile overnight; matching providers receive an additive score penalty so more stable lines can win even when their raw RTT is slightly higher.

## Current Web UI

The dashboard is embedded from `internal/web/dashboard.html`. There is no separate frontend build step.

Main modules:

- Hero card with domain, live latency orb, probe source, effective carrier strategy, current IP, next check, discovered count
- IP-specific latency history chart
- IP performance cards with:
  - geo/ISP badge
  - sample count
  - latency/jitter/loss/success-rate/score
  - orphaned state for IPs removed from the latest DNS result
- Real-time logs with:
  - polling frontend
  - filter tags
  - cycle grouping
  - summaries
  - collapse/expand
  - auto-follow toggle
- Region routing panel with global summary and region cards, using embedded local SVG flags from `internal/web/assets/flags`
- Settings modal with 4 tabs:
  - basic
  - airport entries
  - probe
  - routing
    - includes time-window penalty controls for start hour, end hour, penalty score, and provider keywords

## Persistence

Handled in `internal/web/persistence.go`.

- Logs, history, and samples are retained for a 7-day sliding window
- Each dataset also has a hard cap of 2000 entries
- Data is stored under `data/`
- Old IP analytics naturally age out through sample retention

## Geo / IP Analytics

Implemented in `internal/web/server.go` and `persistence.go`.

- Geo lookups are cached in memory
- Only unseen public IPs trigger geo lookup
- Current endpoint:
  - `http://ip-api.com/json/{ip}?lang=zh-CN&fields=status,country,city,isp`
- `computeIPStats()` enriches each IP with:
  - `geo`
  - `active`
  - `status`
  - sample counts
  - success rate
  - latency/jitter/loss/score metrics

## Lifecycle / Orphaned IP Behavior

- Latest DNS result becomes the active IP set
- Historical IPs not in the latest DNS result become `status: "orphaned"`
- Orphaned IPs are visually demoted in the UI and sorted below active IPs
- They are not actively re-probed because new probe rounds only target the latest resolved IPs
- Their historical samples remain visible until they age out of the 7-day window

## Config Editing

`/api/config` supports live updates for:

- `target_domain`
- `custom_domain`
- `probe_source`
- `carrier`
- `ping_mode`
- `ping_port`
- `check_interval`
- `ping_attempts`
- `selection_latency_weight`
- `selection_jitter_weight`
- `selection_loss_weight`
- `switch_improvement_percent`
- `switch_stable_seconds`
- `failed_orphan_ttl_hours`
- `fallback_baseline_ip`
- `alert_webhook_url`
- `time_penalty_start_hour`
- `time_penalty_end_hour`
- `time_penalty_score`
- `time_penalty_org_keywords`

`/api/airport-profiles` supports live updates for multi-airport routing:

- `base_domain`
- `airport_profiles[].name`
- `airport_profiles[].slug`
- `airport_profiles[].target_domains`
- `airport_profiles[].probe_source`
- `airport_profiles[].carrier`

Changes are:

1. Written back to `config.yaml`
2. Applied to in-memory runtime state
3. Reflected in the dashboard immediately

## Deployment Notes

Common real deployment target used in this project history:

- Host: `10.0.0.231`
- Path: `/opt/dns-latency-router`
- Process manager: `pm2`
- Process name: `dns-latency-router`

Typical deployment flow:

1. `go test ./...`
2. Build Linux binary
3. Upload binary to server
4. Replace executable in `/opt/dns-latency-router`
5. `pm2 restart dns-latency-router`

## Common Pitfalls

- The live dashboard template is `dashboard.html`
- Static dashboard assets under `internal/web/assets/` are embedded by Go; keep flag SVGs local unless an external CDN is explicitly requested
- The frontend currently polls JSON APIs; browser "always loading" issues from SSE are no longer the main UI path
- Not every change needs Cloudflare API verification, but anything touching modern network behavior or deployment should be verified carefully
