package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"time"

	"dns-latency-router/internal/checker"
	"dns-latency-router/internal/cloudflare"
	"dns-latency-router/internal/config"
	"dns-latency-router/internal/web"
)

type switchController struct {
	candidateIP    string
	candidateSince time.Time
}

func (c *switchController) reset() {
	c.candidateIP = ""
	c.candidateSince = time.Time{}
}

func findResultByIP(results []checker.Result, ip string) *checker.Result {
	for i := range results {
		if results[i].IP == ip {
			return &results[i]
		}
	}
	return nil
}

func stableCycles(cfg *config.Config) int {
	if cfg.SwitchStableSec <= 0 || cfg.CheckIntervalSec <= 0 {
		return 1
	}
	return int(math.Ceil(float64(cfg.SwitchStableSec) / float64(cfg.CheckIntervalSec)))
}

func shouldReplaceCurrent(current, candidate *checker.Result, cfg *config.Config) bool {
	if candidate == nil || candidate.Err != nil {
		return false
	}
	if current == nil || current.Err != nil || current.Score <= 0 {
		return true
	}
	improvement := (current.Score - candidate.Score) / current.Score * 100
	return improvement >= cfg.SwitchImprovement
}

func describeResult(r *checker.Result) string {
	if r == nil {
		return "n/a"
	}
	return fmt.Sprintf("lat=%.2fms jitter=%.2fms loss=%.0f%% score=%.2f",
		float64(r.Latency.Microseconds())/1000.0,
		float64(r.Jitter.Microseconds())/1000.0,
		r.LossRate,
		r.Score,
	)
}

func runCheckCycle(cfg *config.Config, cf *cloudflare.Client, ws *web.Server, nextCheck *time.Time, sc *switchController) {
	*nextCheck = time.Now().Add(cfg.CheckInterval)
	best := runOnce(cfg, cf, ws, sc)

	now := time.Now()
	if ws != nil {
		st := ws.GetStatus()
		st.TargetDomain = cfg.TargetDomain
		st.CustomDomain = cfg.CustomDomain
		st.ProbeSource = cfg.ProbeSource
		st.CheckIntervalSec = cfg.CheckIntervalSec
		st.PingMode = cfg.PingMode
		st.PingPort = cfg.PingPort
		st.PingAttempts = cfg.PingAttempts
		st.LatencyWeight = cfg.LatencyWeight
		st.JitterWeight = cfg.JitterWeight
		st.LossWeight = cfg.LossWeight
		st.SwitchImprovement = cfg.SwitchImprovement
		st.SwitchStableSec = cfg.SwitchStableSec
		st.IsRunning = true
		st.LastCheck = now.Format(time.RFC3339)
		st.NextCheck = nextCheck.Format(time.RFC3339)
		if best != nil {
			st.CurrentIP = best.IP
			st.Latency = float64(best.Latency.Microseconds()) / 1000.0
			ws.AddHistory(web.CheckRecord{
				Time:    now,
				IP:      best.IP,
				Latency: float64(best.Latency.Microseconds()) / 1000.0,
				Success: true,
			})
		} else {
			st.CurrentIP = ""
			st.Latency = 0
			ws.AddHistory(web.CheckRecord{
				Time:    now,
				Success: false,
				Error:   "all IPs failed",
			})
		}
		ws.UpdateStatus(st)
	}
	log.Printf("next check in %s", cfg.CheckInterval)
}

func runOnce(cfg *config.Config, cf *cloudflare.Client, ws *web.Server, sc *switchController) *checker.Result {
	// 1. Resolve target domain from multiple DNS servers to discover all IPs
	log.Printf("[check] resolving %s from %d DNS servers ...", cfg.TargetDomain, len(cfg.DNSServers))
	ips, err := checker.ResolveFromAllDNS(cfg.TargetDomain, cfg.DNSServers)
	if err != nil {
		log.Printf("[error] dns resolve failed: %v", err)
		return nil
	}
	log.Printf("[check] discovered %d unique IP(s): %v", len(ips), ips)
	if ws != nil {
		ws.UpdateResolvedIPs(ips)
	}

	// Update web: discovered count
	if ws != nil {
		st := ws.GetStatus()
		st.DiscoveredCount = len(ips)
		ws.UpdateStatus(st)
	}

	// 2. Ping all IPs (ICMP or TCP)
	mode := cfg.PingMode
	if mode == "" {
		mode = "icmp"
	}
	if mode == "icmp" {
		log.Printf("[check] pinging %d IP(s) via ICMP ...", len(ips))
	} else {
		log.Printf("[check] pinging %d IP(s) on port %d ...", len(ips), cfg.PingPort)
	}
	results := checker.PingAll(
		ips,
		mode,
		cfg.PingPort,
		cfg.PingTimeout,
		cfg.PingAttempts,
		cfg.LatencyWeight,
		cfg.JitterWeight,
		cfg.LossWeight,
	)

	if ws != nil {
		now := time.Now()
		samples := make([]web.IPSample, 0, len(results))
		for _, r := range results {
			sample := web.IPSample{
				Time: now,
				IP:   r.IP,
			}
			if r.Err != nil {
				sample.Error = r.Err.Error()
				sample.LossRate = r.LossRate
			} else {
				sample.Success = true
				sample.Latency = float64(r.Latency.Microseconds()) / 1000.0
				sample.Jitter = float64(r.Jitter.Microseconds()) / 1000.0
				sample.LossRate = r.LossRate
				sample.Score = r.Score
			}
			samples = append(samples, sample)
		}
		ws.AddSamples(samples)
	}

	// 3. Find the best (skip suspiciously low latencies below threshold)
	var best *checker.Result
	for i := range results {
		r := &results[i]
		if r.Err != nil {
			log.Printf("[ping] %s: FAILED (%v, loss %.0f%%)", r.IP, r.Err, r.LossRate)
			continue
		}
		ms := float64(r.Latency.Microseconds()) / 1000.0
		if r.Latency < cfg.PingMinThreshold {
			log.Printf("[ping] %s: %.2f ms (skipped, below threshold %.0f ms)", r.IP, ms, cfg.PingMinThreshold.Seconds()*1000)
			continue
		}
		log.Printf("[ping] %s: avg %.2f ms, jitter %.2f ms, loss %.0f%%, score %.2f",
			r.IP,
			ms,
			float64(r.Jitter.Microseconds())/1000.0,
			r.LossRate,
			r.Score,
		)
		if best == nil {
			best = r
		}
	}

	if best == nil {
		log.Printf("[error] all %d IP(s) failed to respond", len(ips))
		return nil
	}

	log.Printf("[check] best candidate: %s (%s)", best.IP, describeResult(best))

	currentIP, err := cf.CurrentIP()
	if err != nil {
		log.Printf("[error] read current Cloudflare record failed: %v", err)
		return best
	}

	current := findResultByIP(results, currentIP)
	if currentIP == best.IP {
		sc.reset()
		log.Printf("[update] current record already points to %s, no switch needed", currentIP)
		return best
	}

	if !shouldReplaceCurrent(current, best, cfg) {
		sc.reset()
		log.Printf("[update] keeping %s; candidate %s does not beat current enough (need %.0f%%, current %s, candidate %s)",
			currentIP,
			best.IP,
			cfg.SwitchImprovement,
			describeResult(current),
			describeResult(best),
		)
		return best
	}

	now := time.Now()
	if sc.candidateIP != best.IP {
		sc.candidateIP = best.IP
		sc.candidateSince = now
		log.Printf("[update] candidate %s is better than current %s; observing stability for %d cycles (~%ds)",
			best.IP, currentIP, stableCycles(cfg), cfg.SwitchStableSec)
		return best
	}

	if cfg.SwitchStableSec > 0 && now.Sub(sc.candidateSince) < time.Duration(cfg.SwitchStableSec)*time.Second {
		remaining := time.Duration(cfg.SwitchStableSec)*time.Second - now.Sub(sc.candidateSince)
		log.Printf("[update] candidate %s still stabilizing for %s before switch", best.IP, remaining.Round(time.Second))
		return best
	}

	log.Printf("[update] switching %s A record -> %s (current %s, candidate %s)",
		cfg.CustomDomain, best.IP, describeResult(current), describeResult(best))
	if err := cf.UpdateRecord(best.IP); err != nil {
		log.Printf("[error] cloudflare update failed: %v", err)
		return best
	}
	sc.reset()
	log.Printf("[update] switch applied")

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

	// Manual check trigger channel
	triggerCh := make(chan struct{}, 1)
	sc := &switchController{}

	// Start web dashboard if configured
	var ws *web.Server
	if cfg.WebPort > 0 {
		// Clone cfg pointer for the callback closure
		cfgPtr := cfg
		ws = web.New(cfg.WebPort, configPath, triggerCh, func(targetDomain, customDomain, probeSource, pingMode string, pingPort, checkInterval, pingAttempts, switchStableSec int, latencyWeight, jitterWeight, lossWeight, switchImprovement float64) {
			cfgPtr.TargetDomain = targetDomain
			cfgPtr.CustomDomain = customDomain
			cfgPtr.ProbeSource = probeSource
			if pingMode != "" {
				cfgPtr.PingMode = pingMode
			}
			if pingPort > 0 {
				cfgPtr.PingPort = pingPort
			}
			if checkInterval > 0 {
				cfgPtr.CheckIntervalSec = checkInterval
				cfgPtr.CheckInterval = time.Duration(checkInterval) * time.Second
			}
			if pingAttempts > 0 {
				cfgPtr.PingAttempts = pingAttempts
			}
			if latencyWeight > 0 {
				cfgPtr.LatencyWeight = latencyWeight
			}
			if jitterWeight >= 0 {
				cfgPtr.JitterWeight = jitterWeight
			}
			if lossWeight >= 0 {
				cfgPtr.LossWeight = lossWeight
			}
			if switchImprovement >= 0 {
				cfgPtr.SwitchImprovement = switchImprovement
			}
			if switchStableSec >= 0 {
				cfgPtr.SwitchStableSec = switchStableSec
			}
			log.Printf("[config] applied: target_domain=%q custom_domain=%q probe_source=%q ping_mode=%q ping_port=%d check_interval=%d ping_attempts=%d latency_weight=%.2f jitter_weight=%.2f loss_weight=%.2f switch_improvement=%.2f switch_stable_seconds=%d",
				targetDomain, customDomain, probeSource, pingMode, pingPort, checkInterval, pingAttempts, latencyWeight, jitterWeight, lossWeight, switchImprovement, switchStableSec)
		})
		ws.Start()
		ws.WaitReady()
		log.SetOutput(ws.LogWriter())
	}

	cf := cloudflare.New(cfg.Cloudflare.APIToken, cfg.Cloudflare.ZoneID, cfg.Cloudflare.RecordID, cfg.ProxyURL)

	// Verify Cloudflare API access on startup (non-fatal: network may be blocked intermittently)
	if _, err := cf.GetRecord(); err != nil {
		log.Printf("[error] Cloudflare API unreachable: %v (will retry on next cycle)", err)
	} else {
		log.Printf("cloudflare API access verified for record %s", cfg.Cloudflare.RecordID)
	}

	// Set initial web status
	if ws != nil {
		ws.UpdateStatus(&web.Status{
			TargetDomain:     cfg.TargetDomain,
			CustomDomain:     cfg.CustomDomain,
			ProbeSource:      cfg.ProbeSource,
			CheckIntervalSec: cfg.CheckIntervalSec,
			PingMode:         cfg.PingMode,
			PingPort:         cfg.PingPort,
			PingAttempts:     cfg.PingAttempts,
			LatencyWeight:    cfg.LatencyWeight,
			JitterWeight:     cfg.JitterWeight,
			LossWeight:       cfg.LossWeight,
			SwitchImprovement: cfg.SwitchImprovement,
			SwitchStableSec:  cfg.SwitchStableSec,
			IsRunning:        true,
		})
	}

	// Trap SIGINT/SIGTERM for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	// Run immediately, then on timer
	firstBest := runOnce(cfg, cf, ws, sc)
	now := time.Now()
	nextCheck := now.Add(cfg.CheckInterval)
	if ws != nil {
		st := ws.GetStatus()
		st.TargetDomain = cfg.TargetDomain
		st.CustomDomain = cfg.CustomDomain
		st.ProbeSource = cfg.ProbeSource
		st.CheckIntervalSec = cfg.CheckIntervalSec
		st.PingMode = cfg.PingMode
		st.PingPort = cfg.PingPort
		st.PingAttempts = cfg.PingAttempts
		st.LatencyWeight = cfg.LatencyWeight
		st.JitterWeight = cfg.JitterWeight
		st.LossWeight = cfg.LossWeight
		st.SwitchImprovement = cfg.SwitchImprovement
		st.SwitchStableSec = cfg.SwitchStableSec
		st.IsRunning = true
		st.LastCheck = now.Format(time.RFC3339)
		st.NextCheck = nextCheck.Format(time.RFC3339)
		if firstBest != nil {
			st.CurrentIP = firstBest.IP
			st.Latency = float64(firstBest.Latency.Microseconds()) / 1000.0
			ws.AddHistory(web.CheckRecord{
				Time:    now,
				IP:      firstBest.IP,
				Latency: float64(firstBest.Latency.Microseconds()) / 1000.0,
				Success: true,
			})
		} else {
			st.CurrentIP = ""
			st.Latency = 0
			ws.AddHistory(web.CheckRecord{
				Time:    now,
				Success: false,
				Error:   "all IPs failed",
			})
		}
		ws.UpdateStatus(st)
	}

	timer := time.NewTimer(cfg.CheckInterval)
	defer timer.Stop()

	log.Printf("next check in %s", cfg.CheckInterval)
	log.Printf("press Ctrl+C to stop")

	for {
		select {
		case <-timer.C:
			runCheckCycle(cfg, cf, ws, &nextCheck, sc)
			timer.Reset(cfg.CheckInterval)

		case <-triggerCh:
			log.Printf("[check] manual trigger")
			runCheckCycle(cfg, cf, ws, &nextCheck, sc)
			// Reset timer to full interval from now
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(cfg.CheckInterval)

		case sig := <-sigCh:
			log.Printf("received %v, exiting", sig)
			os.Exit(0)
		}
	}
}
