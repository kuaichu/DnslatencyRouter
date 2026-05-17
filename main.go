package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"dns-latency-router/internal/checker"
	"dns-latency-router/internal/cloudflare"
	"dns-latency-router/internal/config"
	"dns-latency-router/internal/web"
)

type switchController struct {
	candidateIP    string
	candidateSince time.Time
	outageActive   bool
	lastAlertAt    time.Time
	orgCache       map[string]string
}

type cycleOutcome struct {
	Best         *checker.Result
	ActiveIP     string
	ActiveResult *checker.Result
}

func (c *switchController) reset() {
	c.candidateIP = ""
	c.candidateSince = time.Time{}
}

func (c *switchController) clearOutage() {
	c.outageActive = false
}

func (c *switchController) orgForIP(ip string) string {
	if c.orgCache == nil {
		c.orgCache = make(map[string]string)
	}
	return c.orgCache[ip]
}

func (c *switchController) storeOrg(ip, org string) {
	if c.orgCache == nil {
		c.orgCache = make(map[string]string)
	}
	if ip == "" || org == "" {
		return
	}
	c.orgCache[ip] = org
}

func (c *switchController) shouldSendAlert(now time.Time) bool {
	if !c.outageActive {
		c.outageActive = true
		c.lastAlertAt = now
		return true
	}
	if now.Sub(c.lastAlertAt) >= 30*time.Minute {
		c.lastAlertAt = now
		return true
	}
	return false
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

func summarizeFailures(results []checker.Result) []string {
	lines := make([]string, 0, len(results))
	for _, r := range results {
		if r.Err == nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %v (loss %.0f%%)", r.IP, r.Err, r.LossRate))
	}
	return lines
}

func sendFallbackAlert(cfg *config.Config, currentIP string, resolved []string, results []checker.Result, reason string, usingFallback bool) {
	if cfg.AlertWebhookURL == "" {
		return
	}
	payload := map[string]interface{}{
		"event":                "dns_latency_router_no_candidate",
		"time":                 time.Now().Format(time.RFC3339),
		"target_domain":        cfg.TargetDomain,
		"custom_domain":        cfg.CustomDomain,
		"carrier":              cfg.EffectiveCarrierLabel(),
		"current_ip":           currentIP,
		"fallback_baseline_ip": cfg.FallbackBaselineIP,
		"using_fallback":       usingFallback,
		"reason":               reason,
		"resolved_ips":         resolved,
		"failures":             summarizeFailures(results),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[error] alert webhook marshal failed: %v", err)
		return
	}
	req, err := http.NewRequest("POST", cfg.AlertWebhookURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[error] alert webhook request build failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[error] alert webhook failed: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("[error] alert webhook returned status %d", resp.StatusCode)
		return
	}
	log.Printf("[alert] webhook sent for no-candidate event")
}

func fetchOrgForIP(ip string) string {
	if net.ParseIP(ip) == nil {
		return ""
	}
	client := &http.Client{Timeout: 4 * time.Second}
	url := fmt.Sprintf("http://ip-api.com/json/%s?fields=status,isp", ip)
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var payload struct {
		Status string `json:"status"`
		ISP    string `json:"isp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}
	if payload.Status != "success" {
		return ""
	}
	return strings.TrimSpace(payload.ISP)
}

func timePenaltyForResult(cfg *config.Config, now time.Time, org string) (float64, string) {
	if cfg.TimePenaltyScore <= 0 || org == "" || !cfg.TimePenaltyActiveAt(now) {
		return 0, ""
	}
	orgLower := strings.ToLower(org)
	for _, keyword := range cfg.TimePenaltyKeywords() {
		if strings.Contains(orgLower, keyword) {
			return cfg.TimePenaltyScore, keyword
		}
	}
	return 0, ""
}

func applyTimePenalty(cfg *config.Config, sc *switchController, results []checker.Result, now time.Time) {
	if cfg.TimePenaltyScore <= 0 || !cfg.TimePenaltyActiveAt(now) {
		return
	}
	for i := range results {
		r := &results[i]
		if r.Err != nil {
			continue
		}
		org := sc.orgForIP(r.IP)
		if org == "" {
			org = fetchOrgForIP(r.IP)
			sc.storeOrg(r.IP, org)
		}
		penalty, matchedKeyword := timePenaltyForResult(cfg, now, org)
		if penalty <= 0 {
			continue
		}
		r.Score += penalty
		log.Printf("[score] %s: +%.2f time-window penalty (org=%s, keyword=%s, hour=%02d)", r.IP, penalty, org, matchedKeyword, now.Hour())
	}
	sort.Slice(results, func(i, j int) bool {
		ri, rj := results[i], results[j]
		if ri.Err != nil && rj.Err != nil {
			return ri.IP < rj.IP
		}
		if ri.Err != nil {
			return false
		}
		if rj.Err != nil {
			return true
		}
		if ri.Score != rj.Score {
			return ri.Score < rj.Score
		}
		if ri.LossRate != rj.LossRate {
			return ri.LossRate < rj.LossRate
		}
		return ri.Latency < rj.Latency
	})
}

func applyCycleOutcome(st *web.Status, outcome *cycleOutcome) bool {
	if outcome == nil {
		st.Latency = 0
		return false
	}

	if outcome.ActiveIP != "" {
		st.CurrentIP = outcome.ActiveIP
	}

	if outcome.ActiveResult != nil && outcome.ActiveResult.Err == nil {
		st.Latency = float64(outcome.ActiveResult.Latency.Microseconds()) / 1000.0
		return true
	}

	st.Latency = 0
	return false
}

func runCheckCycle(cfg *config.Config, cf *cloudflare.Client, ws *web.Server, nextCheck *time.Time, sc *switchController) {
	*nextCheck = time.Now().Add(cfg.CheckInterval)
	outcome := runOnce(cfg, cf, ws, sc)

	now := time.Now()
	if ws != nil {
		st := ws.GetStatus()
		st.TargetDomain = cfg.TargetDomain
		st.CustomDomain = cfg.CustomDomain
		st.ProbeSource = cfg.ProbeSource
		st.Carrier = cfg.Carrier
		st.CarrierLabel = cfg.EffectiveCarrierLabel()
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
		if applyCycleOutcome(st, outcome) {
			ws.AddHistory(web.CheckRecord{
				Time:    now,
				IP:      st.CurrentIP,
				Latency: st.Latency,
				Success: true,
			})
		} else {
			ws.AddHistory(web.CheckRecord{
				Time:    now,
				Success: false,
				Error:   "active route latency unavailable",
			})
		}
		ws.UpdateStatus(st)
	}
	log.Printf("next check in %s", cfg.CheckInterval)
}

func runOnce(cfg *config.Config, cf *cloudflare.Client, ws *web.Server, sc *switchController) *cycleOutcome {
	// 1. Resolve target domain from multiple DNS servers to discover all IPs
	dnsServers := cfg.EffectiveDNSServers()
	log.Printf("[check] carrier policy: %s, using %d DNS servers", cfg.EffectiveCarrierLabel(), len(dnsServers))
	log.Printf("[check] resolving %s from %d DNS servers ...", cfg.TargetDomain, len(dnsServers))
	ips, err := checker.ResolveFromAllDNS(cfg.TargetDomain, dnsServers)
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
	applyTimePenalty(cfg, sc, results, time.Now())

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
		sc.reset()
		log.Printf("[error] all %d IP(s) failed to respond", len(ips))
		currentIP, err := cf.CurrentIP()
		if err != nil {
			log.Printf("[error] read current Cloudflare record failed: %v", err)
			if sc.shouldSendAlert(time.Now()) {
				sendFallbackAlert(cfg, "", ips, results, "all candidates failed and current record could not be read", false)
			}
			return &cycleOutcome{}
		}
		usingFallback := false
		if cfg.FallbackBaselineIP != "" {
			usingFallback = true
			if currentIP == cfg.FallbackBaselineIP {
				log.Printf("[fallback] no healthy candidates, baseline %s is already active", cfg.FallbackBaselineIP)
			} else {
				log.Printf("[fallback] no healthy candidates, switching to baseline %s", cfg.FallbackBaselineIP)
				if err := cf.UpdateRecord(cfg.FallbackBaselineIP); err != nil {
					log.Printf("[error] fallback baseline update failed: %v", err)
				} else {
					currentIP = cfg.FallbackBaselineIP
					log.Printf("[fallback] baseline route applied")
				}
			}
		}
		if sc.shouldSendAlert(time.Now()) {
			reason := "all candidates failed; keeping current route"
			if usingFallback {
				reason = "all candidates failed; fallback baseline engaged"
			}
			sendFallbackAlert(cfg, currentIP, ips, results, reason, usingFallback)
		}
		return &cycleOutcome{
			ActiveIP:     currentIP,
			ActiveResult: findResultByIP(results, currentIP),
		}
	}
	sc.clearOutage()

	log.Printf("[check] best candidate: %s (%s)", best.IP, describeResult(best))

	currentIP, err := cf.CurrentIP()
	if err != nil {
		log.Printf("[error] read current Cloudflare record failed: %v", err)
		return &cycleOutcome{Best: best}
	}

	current := findResultByIP(results, currentIP)
	if currentIP == best.IP {
		sc.reset()
		log.Printf("[update] current record already points to %s, no switch needed", currentIP)
		return &cycleOutcome{
			Best:         best,
			ActiveIP:     currentIP,
			ActiveResult: best,
		}
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
		return &cycleOutcome{
			Best:         best,
			ActiveIP:     currentIP,
			ActiveResult: current,
		}
	}

	now := time.Now()
	if sc.candidateIP != best.IP {
		sc.candidateIP = best.IP
		sc.candidateSince = now
		log.Printf("[update] candidate %s is better than current %s; observing stability for %d cycles (~%ds)",
			best.IP, currentIP, stableCycles(cfg), cfg.SwitchStableSec)
		return &cycleOutcome{
			Best:         best,
			ActiveIP:     currentIP,
			ActiveResult: current,
		}
	}

	if cfg.SwitchStableSec > 0 && now.Sub(sc.candidateSince) < time.Duration(cfg.SwitchStableSec)*time.Second {
		remaining := time.Duration(cfg.SwitchStableSec)*time.Second - now.Sub(sc.candidateSince)
		log.Printf("[update] candidate %s still stabilizing for %s before switch", best.IP, remaining.Round(time.Second))
		return &cycleOutcome{
			Best:         best,
			ActiveIP:     currentIP,
			ActiveResult: current,
		}
	}

	log.Printf("[update] switching %s A record -> %s (current %s, candidate %s)",
		cfg.CustomDomain, best.IP, describeResult(current), describeResult(best))
	if err := cf.UpdateRecord(best.IP); err != nil {
		log.Printf("[error] cloudflare update failed: %v", err)
		return &cycleOutcome{
			Best:         best,
			ActiveIP:     currentIP,
			ActiveResult: current,
		}
	}
	sc.reset()
	log.Printf("[update] switch applied")

	return &cycleOutcome{
		Best:         best,
		ActiveIP:     best.IP,
		ActiveResult: best,
	}
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
		ws = web.New(cfg.WebPort, configPath, triggerCh, func(targetDomain, customDomain, probeSource, carrier, pingMode string, pingPort, checkInterval, pingAttempts, switchStableSec int, latencyWeight, jitterWeight, lossWeight, switchImprovement float64, failedOrphanTTLHours int, fallbackBaselineIP, alertWebhookURL string, timePenaltyStartHour, timePenaltyEndHour int, timePenaltyScore float64, timePenaltyOrgKeywords string) {
			cfgPtr.TargetDomain = targetDomain
			cfgPtr.CustomDomain = customDomain
			cfgPtr.ProbeSource = probeSource
			cfgPtr.Carrier = config.NormalizeCarrier(carrier)
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
			if failedOrphanTTLHours >= 0 {
				cfgPtr.FailedOrphanTTLHours = failedOrphanTTLHours
				cfgPtr.FailedOrphanTTL = time.Duration(failedOrphanTTLHours) * time.Hour
			}
			cfgPtr.FallbackBaselineIP = fallbackBaselineIP
			cfgPtr.AlertWebhookURL = alertWebhookURL
			if timePenaltyStartHour >= 0 {
				cfgPtr.TimePenaltyStartHour = timePenaltyStartHour
			}
			if timePenaltyEndHour >= 0 {
				cfgPtr.TimePenaltyEndHour = timePenaltyEndHour
			}
			if timePenaltyScore >= 0 {
				cfgPtr.TimePenaltyScore = timePenaltyScore
			}
			cfgPtr.TimePenaltyOrgKeywords = timePenaltyOrgKeywords
			ws.SetSafeguards(cfgPtr.FailedOrphanTTLHours, cfgPtr.FallbackBaselineIP, cfgPtr.AlertWebhookURL)
			ws.SetTimePenaltyConfig(cfgPtr.TimePenaltyStartHour, cfgPtr.TimePenaltyEndHour, cfgPtr.TimePenaltyScore, cfgPtr.TimePenaltyOrgKeywords)
			log.Printf("[config] applied: target_domain=%q custom_domain=%q probe_source=%q carrier=%q effective_carrier=%q ping_mode=%q ping_port=%d check_interval=%d ping_attempts=%d latency_weight=%.2f jitter_weight=%.2f loss_weight=%.2f switch_improvement=%.2f switch_stable_seconds=%d failed_orphan_ttl_hours=%d fallback_baseline_ip=%q alert_webhook_url_set=%t time_penalty=%02d-%02d/+%.2f keywords=%q",
				targetDomain, customDomain, probeSource, cfgPtr.Carrier, cfgPtr.EffectiveCarrierLabel(), pingMode, pingPort, checkInterval, pingAttempts, latencyWeight, jitterWeight, lossWeight, switchImprovement, switchStableSec, cfgPtr.FailedOrphanTTLHours, cfgPtr.FallbackBaselineIP, cfgPtr.AlertWebhookURL != "", cfgPtr.TimePenaltyStartHour, cfgPtr.TimePenaltyEndHour, cfgPtr.TimePenaltyScore, cfgPtr.TimePenaltyOrgKeywords)
		})
		ws.SetSafeguards(cfg.FailedOrphanTTLHours, cfg.FallbackBaselineIP, cfg.AlertWebhookURL)
		ws.SetTimePenaltyConfig(cfg.TimePenaltyStartHour, cfg.TimePenaltyEndHour, cfg.TimePenaltyScore, cfg.TimePenaltyOrgKeywords)
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
			TargetDomain:      cfg.TargetDomain,
			CustomDomain:      cfg.CustomDomain,
			ProbeSource:       cfg.ProbeSource,
			Carrier:           cfg.Carrier,
			CarrierLabel:      cfg.EffectiveCarrierLabel(),
			CheckIntervalSec:  cfg.CheckIntervalSec,
			PingMode:          cfg.PingMode,
			PingPort:          cfg.PingPort,
			PingAttempts:      cfg.PingAttempts,
			LatencyWeight:     cfg.LatencyWeight,
			JitterWeight:      cfg.JitterWeight,
			LossWeight:        cfg.LossWeight,
			SwitchImprovement: cfg.SwitchImprovement,
			SwitchStableSec:   cfg.SwitchStableSec,
			IsRunning:         true,
		})
	}

	// Trap SIGINT/SIGTERM for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	// Run immediately, then on timer
	firstOutcome := runOnce(cfg, cf, ws, sc)
	now := time.Now()
	nextCheck := now.Add(cfg.CheckInterval)
	if ws != nil {
		st := ws.GetStatus()
		st.TargetDomain = cfg.TargetDomain
		st.CustomDomain = cfg.CustomDomain
		st.ProbeSource = cfg.ProbeSource
		st.Carrier = cfg.Carrier
		st.CarrierLabel = cfg.EffectiveCarrierLabel()
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
		if applyCycleOutcome(st, firstOutcome) {
			ws.AddHistory(web.CheckRecord{
				Time:    now,
				IP:      st.CurrentIP,
				Latency: st.Latency,
				Success: true,
			})
		} else {
			ws.AddHistory(web.CheckRecord{
				Time:    now,
				Success: false,
				Error:   "active route latency unavailable",
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
