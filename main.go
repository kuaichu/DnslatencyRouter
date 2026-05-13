package main

import (
	"log"
	"os"
	"os/signal"
	"time"

	"dns-latency-router/internal/checker"
	"dns-latency-router/internal/cloudflare"
	"dns-latency-router/internal/config"
	"dns-latency-router/internal/web"
)

func runOnce(cfg *config.Config, cf *cloudflare.Client, ws *web.Server) *checker.Result {
	// 1. Resolve target domain from multiple DNS servers to discover all IPs
	log.Printf("[check] resolving %s from %d DNS servers ...", cfg.TargetDomain, len(cfg.DNSServers))
	ips, err := checker.ResolveFromAllDNS(cfg.TargetDomain, cfg.DNSServers)
	if err != nil {
		log.Printf("[error] dns resolve failed: %v", err)
		return nil
	}
	log.Printf("[check] discovered %d unique IP(s): %v", len(ips), ips)

	// Update web: discovered count
	if ws != nil {
		st := ws.GetStatus()
		st.DiscoveredCount = len(ips)
		ws.UpdateStatus(st)
	}

	// 2. TCP ping all IPs locally
	log.Printf("[check] pinging %d IP(s) on port %d ...", len(ips), cfg.PingPort)
	results := checker.PingAll(ips, cfg.PingPort, cfg.PingTimeout)

	// 3. Find the best (skip suspiciously low latencies below threshold)
	var best *checker.Result
	for i := range results {
		r := &results[i]
		if r.Err != nil {
			log.Printf("[ping] %s: FAILED (%v)", r.IP, r.Err)
			continue
		}
		ms := float64(r.Latency.Microseconds()) / 1000.0
		if r.Latency < cfg.PingMinThreshold {
			log.Printf("[ping] %s: %.2f ms (skipped, below threshold %.0f ms)", r.IP, ms, cfg.PingMinThreshold.Seconds()*1000)
			continue
		}
		log.Printf("[ping] %s: %.2f ms", r.IP, ms)
		if best == nil {
			best = r
		}
	}

	if best == nil {
		log.Printf("[error] all %d IP(s) failed to respond", len(ips))
		return nil
	}

	log.Printf("[check] best IP: %s (%.2f ms)", best.IP, float64(best.Latency.Microseconds())/1000.0)

	// 4. Update Cloudflare DNS record
	log.Printf("[update] setting %s A record -> %s ...", cfg.CustomDomain, best.IP)
	if err := cf.UpdateRecord(best.IP); err != nil {
		log.Printf("[error] cloudflare update failed: %v", err)
		// Still return best for web status even if CF fails
	}

	log.Printf("[update] done")

	return best
}

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("")

	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Start web dashboard if configured
	var ws *web.Server
	if cfg.WebPort > 0 {
		// Clone cfg pointer for the callback closure
		cfgPtr := cfg
		ws = web.New(cfg.WebPort, configPath, func(targetDomain, customDomain string) {
			cfgPtr.TargetDomain = targetDomain
			cfgPtr.CustomDomain = customDomain
			log.Printf("[config] applied: target_domain=%q custom_domain=%q", targetDomain, customDomain)
		})
		ws.Start()
		ws.WaitReady()
		log.SetOutput(ws.LogWriter())
	}

	cf := cloudflare.New(cfg.Cloudflare.APIToken, cfg.Cloudflare.ZoneID, cfg.Cloudflare.RecordID)

	// Verify Cloudflare API access on startup
	if _, err := cf.GetRecord(); err != nil {
		log.Fatalf("failed to verify Cloudflare API access: %v", err)
	}
	log.Printf("cloudflare API access verified for record %s", cfg.Cloudflare.RecordID)

	// Set initial web status
	if ws != nil {
		ws.UpdateStatus(&web.Status{
			TargetDomain:     cfg.TargetDomain,
			CustomDomain:     cfg.CustomDomain,
			CheckIntervalSec: cfg.CheckIntervalSec,
			IsRunning:        true,
		})
	}

	// Trap SIGINT/SIGTERM for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	// Run immediately, then on ticker
	nextCheck := time.Now()
	best := runOnce(cfg, cf, ws)
	if best != nil && ws != nil {
		ws.UpdateStatus(&web.Status{
			TargetDomain:     cfg.TargetDomain,
			CustomDomain:     cfg.CustomDomain,
			CurrentIP:        best.IP,
			Latency:          float64(best.Latency.Microseconds()) / 1000.0,
			LastCheck:        time.Now().Format(time.RFC3339),
			NextCheck:        time.Now().Add(cfg.CheckInterval).Format(time.RFC3339),
			IsRunning:        true,
			CheckIntervalSec: cfg.CheckIntervalSec,
		})
		ws.AddHistory(web.CheckRecord{
			Time:    time.Now(),
			IP:      best.IP,
			Latency: float64(best.Latency.Microseconds()) / 1000.0,
			Success: true,
		})
	}

	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	log.Printf("next check in %s", cfg.CheckInterval)
	log.Printf("press Ctrl+C to stop")

	for {
		select {
		case <-ticker.C:
			nextCheck = time.Now().Add(cfg.CheckInterval)
			best := runOnce(cfg, cf, ws)
			if ws != nil {
				st := &web.Status{
					TargetDomain:     cfg.TargetDomain,
					CustomDomain:     cfg.CustomDomain,
					CheckIntervalSec: cfg.CheckIntervalSec,
					IsRunning:        true,
					NextCheck:        nextCheck.Format(time.RFC3339),
					LastCheck:        time.Now().Format(time.RFC3339),
				}
				if best != nil {
					st.CurrentIP = best.IP
					st.Latency = float64(best.Latency.Microseconds()) / 1000.0
				}
				ws.UpdateStatus(st)

				if best != nil {
					ws.AddHistory(web.CheckRecord{
						Time:    time.Now(),
						IP:      best.IP,
						Latency: float64(best.Latency.Microseconds()) / 1000.0,
						Success: true,
					})
				} else {
					ws.AddHistory(web.CheckRecord{
						Time:    time.Now(),
						Success: false,
						Error:   "all IPs failed",
					})
				}
			}
			log.Printf("next check in %s", cfg.CheckInterval)

		case sig := <-sigCh:
			log.Printf("received %v, exiting", sig)
			os.Exit(0)
		}
	}
}
