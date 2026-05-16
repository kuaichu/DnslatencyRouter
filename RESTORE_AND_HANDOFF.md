# DNS Latency Router Restore And Handoff

## GitHub Repository

- Repository: `https://github.com/kuaichu/DnslatencyRouter`
- Default branch: `main`

## What This Backup Contains

- Go source code
- `config.yaml`
- build and PM2 files
- project documentation

This backup intentionally excludes:

- `.git/`
- `.claude/`
- compiled binaries
- runtime logs

## Quick Restore Options

### Option 1: Restore From This Backup Only

1. Extract this archive to a working directory.
2. Edit `config.yaml` and fill in:
   - `cloudflare.api_token`
   - `cloudflare.zone_id`
   - `cloudflare.record_id`
   - `target_domain`
   - `custom_domain`
3. Use one of the included scripts:
   - Windows: `restore-windows.ps1`
   - Linux: `restore-linux.sh`

### Option 2: Restore From GitHub

1. Clone the repository:

```bash
git clone https://github.com/kuaichu/DnslatencyRouter.git
cd DnslatencyRouter
```

2. Copy in a valid `config.yaml`.
3. Run the same platform-specific restore script or build manually.

## Windows Deployment

Requirements:

- Go 1.21 or newer

Steps:

```powershell
powershell -ExecutionPolicy Bypass -File .\restore-windows.ps1
```

What it does:

- verifies Go is installed
- ensures `config.yaml` exists
- runs `go mod tidy`
- builds `dns-latency-router.exe`

Start the app:

```powershell
.\dns-latency-router.exe
```

## Linux Deployment

Requirements:

- Go 1.21 or newer
- optional: PM2 for long-running service management

Steps:

```bash
chmod +x restore-linux.sh
./restore-linux.sh
```

What it does:

- verifies Go is installed
- ensures `config.yaml` exists
- runs `go mod tidy`
- builds `dns-latency-router`
- creates `logs/`

Run directly:

```bash
./dns-latency-router
```

Run with PM2:

```bash
pm2 start ecosystem.config.js
pm2 save
```

## Config Notes

Important fields in `config.yaml`:

- `cloudflare.api_token`: must have `Zone:DNS:Edit`
- `cloudflare.zone_id`
- `cloudflare.record_id`
- `target_domain`
- `custom_domain`
- `web_port`: set to `0` to disable the dashboard

If Cloudflare GET works but update fails with code `10000`, the token usually has read permission but not edit permission.

## Project Purpose

This tool:

1. resolves a target domain through multiple DNS servers
2. collects all discovered IPs
3. TCP-pings each IP
4. picks the fastest responding IP
5. updates a Cloudflare DNS A record to that IP

Typical use case:

- client connects to `custom_domain`
- TLS SNI still uses `target_domain`
- Cloudflare DNS is kept pointed at the best current IP

## Suggested Files To Keep Safe

- `config.yaml`
- this backup archive
- the GitHub repository URL
- any Cloudflare token and zone metadata stored in your password manager

## Handoff Notes For Future AI Agents

- Entry point is `main.go`
- core logic lives in `internal/checker`, `internal/cloudflare`, `internal/config`, and `internal/web`
- web dashboard is embedded from `internal/web/dashboard.html`
- the app can run without the dashboard by setting `web_port: 0`
- the app can run without PM2; PM2 is only for Linux process management

## Last Known Backup Context

- Backup created on: `2026-05-13 20:55:08`
- Source folder: `E:\Project\DeepSeek-TUI\dns-latency-router`
- GitHub remote used: `https://github.com/kuaichu/DnslatencyRouter.git`
