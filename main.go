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
	routes         map[string]*routeSwitchState
	candidateIP    string
	candidateSince time.Time
	outageActive   bool
	lastAlertAt    time.Time
	orgCache       map[string]string
	geoCache       map[string]web.GeoInfo
}

type routeSwitchState struct {
	candidateIP    string
	candidateSince time.Time
	outageActive   bool
	lastAlertAt    time.Time
}

type cycleOutcome struct {
	Best         *checker.Result
	ActiveIP     string
	ActiveResult *checker.Result
	Profiles     []web.ProfileStatus
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

func (c *switchController) route(key string) *routeSwitchState {
	if key == "" {
		key = "default"
	}
	if c.routes == nil {
		c.routes = make(map[string]*routeSwitchState)
	}
	st := c.routes[key]
	if st == nil {
		st = &routeSwitchState{}
		c.routes[key] = st
	}
	return st
}

func (c *switchController) resetRoute(key string) {
	st := c.route(key)
	st.candidateIP = ""
	st.candidateSince = time.Time{}
}

func (c *switchController) clearRouteOutage(key string) {
	c.route(key).outageActive = false
}

func (c *switchController) shouldSendRouteAlert(key string, now time.Time) bool {
	st := c.route(key)
	if !st.outageActive {
		st.outageActive = true
		st.lastAlertAt = now
		return true
	}
	if now.Sub(st.lastAlertAt) >= 30*time.Minute {
		st.lastAlertAt = now
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

func fetchGeoForIP(ip string) web.GeoInfo {
	info := web.GeoInfo{IP: ip}
	if net.ParseIP(ip) == nil {
		return info
	}
	client := &http.Client{Timeout: 4 * time.Second}
	url := fmt.Sprintf("http://ip-api.com/json/%s?fields=status,countryCode,country,city,isp", ip)
	resp, err := client.Get(url)
	if err != nil {
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info
	}
	var payload struct {
		Status      string `json:"status"`
		CountryCode string `json:"countryCode"`
		Country     string `json:"country"`
		City        string `json:"city"`
		ISP         string `json:"isp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return info
	}
	if payload.Status != "success" {
		return info
	}
	info.CountryCode = strings.TrimSpace(payload.CountryCode)
	info.Country = strings.TrimSpace(payload.Country)
	info.City = strings.TrimSpace(payload.City)
	info.ISP = strings.TrimSpace(payload.ISP)
	return info
}

func (c *switchController) geoForIP(ws *web.Server, ip string) web.GeoInfo {
	if ws != nil {
		info := ws.GeoForIP(ip)
		if info.CountryCode != "" || info.ISP != "" {
			if c.geoCache == nil {
				c.geoCache = make(map[string]web.GeoInfo)
			}
			c.geoCache[ip] = info
		}
		return info
	}
	if c.geoCache == nil {
		c.geoCache = make(map[string]web.GeoInfo)
	}
	if info, ok := c.geoCache[ip]; ok {
		return info
	}
	info := fetchGeoForIP(ip)
	c.geoCache[ip] = info
	return info
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

func sortedRegionKeys[V any](records map[string]V) []string {
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func regionCloudflareClient(cfg *config.Config, rec config.RegionRecord) *cloudflare.Client {
	token := strings.TrimSpace(rec.Cloudflare.APIToken)
	if token == "" {
		token = cfg.Cloudflare.APIToken
	}
	zoneID := strings.TrimSpace(rec.Cloudflare.ZoneID)
	if zoneID == "" {
		zoneID = cfg.Cloudflare.ZoneID
	}
	recordID := strings.TrimSpace(rec.RecordID)
	if recordID == "" {
		recordID = strings.TrimSpace(rec.Cloudflare.RecordID)
	}
	return cloudflare.New(token, zoneID, recordID, cfg.ProxyURL)
}

func resultLatencyMs(r *checker.Result) float64 {
	if r == nil || r.Err != nil {
		return 0
	}
	return float64(r.Latency.Microseconds()) / 1000.0
}

func classifyResultRegion(profile config.AirportProfile, records map[string]config.RegionRecord, geo web.GeoInfo) string {
	if _, ok := records["default"]; ok {
		return "default"
	}
	return config.CountryCodeToRegion(geo.CountryCode)
}

func regionRecordFor(cfg *config.Config, profile config.AirportProfile, region string) config.RegionRecord {
	region = config.NormalizeRegion(region)
	if rec, ok := profile.RegionRecords[region]; ok {
		if rec.Label == "" {
			rec.Label = config.RegionLabel(region)
		}
		if rec.CustomDomain == "" && cfg.BaseDomain != "" {
			rec.CustomDomain = fmt.Sprintf("%s-%s.%s", profile.Slug, region, strings.TrimPrefix(cfg.BaseDomain, "."))
		}
		return rec
	}
	return config.RegionRecord{
		Label:        config.RegionLabel(region),
		CustomDomain: fmt.Sprintf("%s-%s.%s", profile.Slug, region, strings.TrimPrefix(cfg.BaseDomain, ".")),
	}
}

func bestHealthyResult(results []checker.Result, cfg *config.Config) *checker.Result {
	for i := range results {
		r := &results[i]
		if r.Err != nil {
			continue
		}
		if r.Latency < cfg.PingMinThreshold {
			continue
		}
		return r
	}
	return nil
}

func resolveProfileIPs(profile config.AirportProfile, dnsServers []string) ([]string, error) {
	targets := profile.TargetDomains
	if len(targets) == 0 && profile.TargetDomain != "" {
		targets = []string{profile.TargetDomain}
	}
	ipSet := make(map[string]struct{})
	var failures []string
	for _, domain := range targets {
		log.Printf("[check] [%s] resolving %s from %d DNS servers ...", profile.ID, domain, len(dnsServers))
		ips, err := checker.ResolveFromAllDNS(domain, dnsServers)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", domain, err))
			log.Printf("[error] [%s] dns resolve failed for %s: %v", profile.ID, domain, err)
			continue
		}
		log.Printf("[check] [%s] %s discovered %d unique IP(s): %v", profile.ID, domain, len(ips), ips)
		for _, ip := range ips {
			ipSet[ip] = struct{}{}
		}
	}
	if len(ipSet) == 0 {
		return nil, fmt.Errorf("all target domains failed: %s", strings.Join(failures, "; "))
	}
	ips := make([]string, 0, len(ipSet))
	for ip := range ipSet {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	return ips, nil
}

func runAirportProfiles(cfg *config.Config, ws *web.Server, sc *switchController) *cycleOutcome {
	outcome := &cycleOutcome{}
	for _, profile := range cfg.AirportProfiles {
		profileStatus := runAirportProfileOnce(cfg, profile, ws, sc)
		outcome.Profiles = append(outcome.Profiles, profileStatus)
		if outcome.ActiveIP == "" {
			for _, region := range profileStatus.Regions {
				if region.CurrentIP != "" {
					outcome.ActiveIP = region.CurrentIP
					break
				}
			}
		}
	}
	return outcome
}

func baseProfileStatus(profile config.AirportProfile) web.ProfileStatus {
	st := web.ProfileStatus{
		ID: profile.ID, Name: profile.Name, Slug: profile.Slug,
		TargetDomain: profile.TargetDomain, TargetDomains: profile.TargetDomains, ProbeSource: profile.ProbeSource,
		Carrier: profile.Carrier, CarrierLabel: config.EffectiveCarrierLabelFor(profile.Carrier, profile.ProbeSource),
	}
	if profile.EntryRecord.RecordID != "" || profile.EntryRecord.CustomDomain != "" {
		label := profile.EntryRecord.Label
		if label == "" {
			label = "全局最快"
		}
		st.Regions = append(st.Regions, web.RegionStatus{
			Region:       "entry",
			Label:        label,
			CustomDomain: profile.EntryRecord.CustomDomain,
			Status:       "no_candidate",
		})
	}
	return st
}

func runAirportProfileOnce(cfg *config.Config, profile config.AirportProfile, ws *web.Server, sc *switchController) web.ProfileStatus {
	dnsServers := cfg.EffectiveDNSServersFor(profile.Carrier, profile.ProbeSource)
	log.Printf("[check] [%s] carrier policy: %s, using %d DNS servers", profile.ID, config.EffectiveCarrierLabelFor(profile.Carrier, profile.ProbeSource), len(dnsServers))
	ips, err := resolveProfileIPs(profile, dnsServers)
	if err != nil {
		log.Printf("[error] [%s] dns resolve failed: %v", profile.ID, err)
		return baseProfileStatus(profile)
	}
	log.Printf("[check] [%s] discovered %d merged unique IP(s): %v", profile.ID, len(ips), ips)
	if ws != nil {
		ws.UpdateResolvedIPsForProfile(profile.ID, ips)
	}

	mode := cfg.PingMode
	if mode == "" {
		mode = "icmp"
	}
	if mode == "icmp" {
		log.Printf("[check] [%s] pinging %d IP(s) via ICMP ...", profile.ID, len(ips))
	} else {
		log.Printf("[check] [%s] pinging %d IP(s) on port %d ...", profile.ID, len(ips), cfg.PingPort)
	}
	results := checker.PingAll(
		ips, mode, cfg.PingPort, cfg.PingTimeout, cfg.PingAttempts,
		cfg.LatencyWeight, cfg.JitterWeight, cfg.LossWeight,
	)
	applyTimePenalty(cfg, sc, results, time.Now())

	resultsByRegion := make(map[string][]checker.Result)
	now := time.Now()
	samples := make([]web.IPSample, 0, len(results))
	for _, r := range results {
		geo := sc.geoForIP(ws, r.IP)
		region := classifyResultRegion(profile, profile.RegionRecords, geo)
		regionLabel := config.RegionLabel(region)
		if rec, ok := profile.RegionRecords[region]; ok && rec.Label != "" {
			regionLabel = rec.Label
		}
		resultsByRegion[region] = append(resultsByRegion[region], r)

		sample := web.IPSample{
			Time: now, ProfileID: profile.ID, ProfileName: profile.Name,
			Region: region, RegionLabel: regionLabel, IP: r.IP,
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
	if ws != nil {
		ws.AddSamples(samples)
	}

	for _, r := range results {
		if r.Err != nil {
			log.Printf("[ping] [%s] %s: FAILED (%v, loss %.0f%%)", profile.ID, r.IP, r.Err, r.LossRate)
			continue
		}
		ms := float64(r.Latency.Microseconds()) / 1000.0
		region := classifyResultRegion(profile, profile.RegionRecords, sc.geoForIP(ws, r.IP))
		if r.Latency < cfg.PingMinThreshold {
			log.Printf("[ping] [%s] [%s] %s: %.2f ms (skipped, below threshold %.0f ms)", profile.ID, region, r.IP, ms, cfg.PingMinThreshold.Seconds()*1000)
			continue
		}
		log.Printf("[ping] [%s] [%s] %s: avg %.2f ms, jitter %.2f ms, loss %.0f%%, score %.2f",
			profile.ID, region, r.IP, ms, float64(r.Jitter.Microseconds())/1000.0, r.LossRate, r.Score)
	}

	profileStatus := baseProfileStatus(profile)
	profileStatus.Regions = nil
	profileStatus.DiscoveredCount = len(ips)
	if profile.EntryRecord.RecordID != "" || profile.EntryRecord.CustomDomain != "" {
		profileStatus.Regions = append(profileStatus.Regions, runProfileRecordDecision(cfg, profile, "entry", profile.EntryRecord, results, sc, now))
	}
	for _, region := range sortedRegionKeys(resultsByRegion) {
		if region == "unknown" || region == "" {
			continue
		}
		rec := regionRecordFor(cfg, profile, region)
		regionResults := resultsByRegion[region]
		profileStatus.Regions = append(profileStatus.Regions, runProfileRecordDecision(cfg, profile, region, rec, regionResults, sc, now))
	}
	return profileStatus
}

func runProfileRecordDecision(cfg *config.Config, profile config.AirportProfile, region string, rec config.RegionRecord, results []checker.Result, sc *switchController, now time.Time) web.RegionStatus {
	best := bestHealthyResult(results, cfg)
	status := web.RegionStatus{
		Region: region, Label: rec.Label, CustomDomain: rec.CustomDomain,
		CandidateCount: len(results), Status: "no_candidate",
	}
	if status.Label == "" {
		if region == "entry" {
			status.Label = "全局最快"
		} else {
			status.Label = config.RegionLabel(region)
		}
	}
	if best != nil {
		status.BestIP = best.IP
		status.Latency = resultLatencyMs(best)
		status.Score = best.Score
	}

	cf := regionCloudflareClient(cfg, rec)
	currentIP := ""
	current := (*checker.Result)(nil)
	if rec.RecordID != "" {
		var err error
		currentIP, err = cf.CurrentIP()
		if err != nil {
			log.Printf("[error] [%s/%s] read current Cloudflare record failed: %v", profile.ID, region, err)
			status.Status = "read_failed"
			return status
		}
		current = findResultByIP(results, currentIP)
	} else if rec.CustomDomain != "" {
		var err error
		currentIP, err = cf.CurrentIPByName(rec.CustomDomain)
		if err != nil {
			if best == nil {
				log.Printf("[error] [%s/%s] read current Cloudflare record failed: %v", profile.ID, region, err)
				status.Status = "read_failed"
				return status
			}
			log.Printf("[update] [%s/%s] A record %s not found, will create it with best candidate %s", profile.ID, region, rec.CustomDomain, best.IP)
		} else {
			current = findResultByIP(results, currentIP)
		}
	} else {
		log.Printf("[error] [%s/%s] missing custom_domain and record_id", profile.ID, region)
		status.Status = "read_failed"
		return status
	}
	status.CurrentIP = currentIP
	routeKey := profile.ID + "|" + region

	if best == nil {
		sc.resetRoute(routeKey)
		log.Printf("[error] [%s/%s] no healthy candidate for %s", profile.ID, region, rec.CustomDomain)
		return status
	}
	sc.clearRouteOutage(routeKey)
	log.Printf("[check] [%s/%s] best candidate: %s (%s)", profile.ID, region, best.IP, describeResult(best))

	if currentIP == best.IP {
		sc.resetRoute(routeKey)
		status.Status = "synced"
		status.Latency = resultLatencyMs(best)
		log.Printf("[update] [%s/%s] current record already points to %s, no switch needed", profile.ID, region, currentIP)
		return status
	}

	if currentIP != "" && !shouldReplaceCurrent(current, best, cfg) {
		sc.resetRoute(routeKey)
		status.Status = "keeping"
		if current != nil && current.Err == nil {
			status.Latency = resultLatencyMs(current)
		}
		log.Printf("[update] [%s/%s] keeping %s; candidate %s does not beat current enough (need %.0f%%, current %s, candidate %s)",
			profile.ID, region, currentIP, best.IP, cfg.SwitchImprovement, describeResult(current), describeResult(best))
		return status
	}

	route := sc.route(routeKey)
	if route.candidateIP != best.IP {
		route.candidateIP = best.IP
		route.candidateSince = now
		status.Status = "stabilizing"
		log.Printf("[update] [%s/%s] candidate %s is better than current %s; observing stability for %d cycles (~%ds)",
			profile.ID, region, best.IP, currentIP, stableCycles(cfg), cfg.SwitchStableSec)
		return status
	}

	if cfg.SwitchStableSec > 0 && now.Sub(route.candidateSince) < time.Duration(cfg.SwitchStableSec)*time.Second {
		remaining := time.Duration(cfg.SwitchStableSec)*time.Second - now.Sub(route.candidateSince)
		status.Status = "stabilizing"
		log.Printf("[update] [%s/%s] candidate %s still stabilizing for %s before switch", profile.ID, region, best.IP, remaining.Round(time.Second))
		return status
	}

	log.Printf("[update] [%s/%s] switching %s A record -> %s (current %s, candidate %s)",
		profile.ID, region, rec.CustomDomain, best.IP, describeResult(current), describeResult(best))
	var updateErr error
	if rec.RecordID != "" {
		updateErr = cf.UpdateRecord(best.IP)
	} else {
		updateErr = cf.UpdateRecordByName(rec.CustomDomain, best.IP)
	}
	if updateErr != nil {
		status.Status = "update_failed"
		log.Printf("[error] [%s/%s] cloudflare update failed: %v", profile.ID, region, updateErr)
		return status
	}
	sc.resetRoute(routeKey)
	status.CurrentIP = best.IP
	status.Status = "switched"
	status.Latency = resultLatencyMs(best)
	log.Printf("[update] [%s/%s] switch applied", profile.ID, region)
	return status
}

func runCheckCycle(cfg *config.Config, cf *cloudflare.Client, ws *web.Server, nextCheck *time.Time, sc *switchController) {
	*nextCheck = time.Now().Add(cfg.CheckInterval)
	if cfg.HasAirportProfiles() {
		outcome := runAirportProfiles(cfg, ws, sc)
		now := time.Now()
		if ws != nil {
			st := ws.GetStatus()
			applyConfigStatus(st, cfg, nextCheck, now)
			st.Profiles = outcome.Profiles
			applyProfileSummary(st, outcome.Profiles)
			for _, profile := range outcome.Profiles {
				for _, region := range profile.Regions {
					if region.CurrentIP != "" && region.Latency > 0 {
						ws.AddHistory(web.CheckRecord{
							Time: now, ProfileID: profile.ID, Region: region.Region,
							IP: region.CurrentIP, Latency: region.Latency, Success: true,
						})
					}
				}
			}
			ws.UpdateStatus(st)
		}
		log.Printf("next check in %s", cfg.CheckInterval)
		return
	}
	outcome := runOnce(cfg, cf, ws, sc)

	now := time.Now()
	if ws != nil {
		st := ws.GetStatus()
		applyConfigStatus(st, cfg, nextCheck, now)
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

func applyConfigStatus(st *web.Status, cfg *config.Config, nextCheck *time.Time, now time.Time) {
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
}

func applyProfileSummary(st *web.Status, profiles []web.ProfileStatus) {
	if len(profiles) == 0 {
		return
	}
	first := profiles[0]
	st.TargetDomain = first.TargetDomain
	st.ProbeSource = first.ProbeSource
	st.Carrier = first.Carrier
	st.CarrierLabel = first.CarrierLabel
	st.DiscoveredCount = first.DiscoveredCount
	for _, profile := range profiles {
		for _, region := range profile.Regions {
			if st.CustomDomain == "" || profile.ID == first.ID {
				st.CustomDomain = region.CustomDomain
			}
			if region.CurrentIP != "" {
				st.CurrentIP = region.CurrentIP
				st.Latency = region.Latency
				return
			}
		}
	}
}

func verifyProfileRecord(cfg *config.Config, profileID, region string, rec config.RegionRecord) {
	client := regionCloudflareClient(cfg, rec)
	if rec.RecordID != "" {
		if _, err := client.GetRecord(); err != nil {
			log.Printf("[error] Cloudflare API unreachable for %s/%s: %v (will retry on next cycle)", profileID, region, err)
		} else {
			log.Printf("cloudflare API access verified for %s/%s record %s", profileID, region, rec.RecordID)
		}
		return
	}
	if rec.CustomDomain == "" {
		log.Printf("[error] Cloudflare record config missing for %s/%s", profileID, region)
		return
	}
	if _, err := client.GetRecordByName(rec.CustomDomain); err != nil {
		log.Printf("[config] Cloudflare A record %s not found or unreadable for %s/%s; it will be created after a healthy candidate is selected", rec.CustomDomain, profileID, region)
	} else {
		log.Printf("cloudflare API access verified for %s/%s record %s", profileID, region, rec.CustomDomain)
	}
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
		ws.SetProfilesCallback(func(next *config.Config) {
			cfgPtr.BaseDomain = next.BaseDomain
			cfgPtr.AirportProfiles = next.AirportProfiles
			cfgPtr.TargetDomain = next.TargetDomain
			cfgPtr.CustomDomain = next.CustomDomain
			cfgPtr.ProbeSource = next.ProbeSource
			cfgPtr.Carrier = next.Carrier
			log.Printf("[config] airport profiles applied: base_domain=%q profiles=%d", cfgPtr.BaseDomain, len(cfgPtr.AirportProfiles))
		})
		ws.SetSafeguards(cfg.FailedOrphanTTLHours, cfg.FallbackBaselineIP, cfg.AlertWebhookURL)
		ws.SetTimePenaltyConfig(cfg.TimePenaltyStartHour, cfg.TimePenaltyEndHour, cfg.TimePenaltyScore, cfg.TimePenaltyOrgKeywords)
		ws.Start()
		ws.WaitReady()
		log.SetOutput(ws.LogWriter())
	}

	var cf *cloudflare.Client
	if cfg.HasAirportProfiles() {
		for _, profile := range cfg.AirportProfiles {
			if profile.EntryRecord.CustomDomain != "" || profile.EntryRecord.RecordID != "" {
				verifyProfileRecord(cfg, profile.ID, "entry", profile.EntryRecord)
			}
			for region, rec := range profile.RegionRecords {
				verifyProfileRecord(cfg, profile.ID, region, rec)
			}
		}
	} else {
		cf = cloudflare.New(cfg.Cloudflare.APIToken, cfg.Cloudflare.ZoneID, cfg.Cloudflare.RecordID, cfg.ProxyURL)
		// Verify Cloudflare API access on startup (non-fatal: network may be blocked intermittently)
		if _, err := cf.GetRecord(); err != nil {
			log.Printf("[error] Cloudflare API unreachable: %v (will retry on next cycle)", err)
		} else {
			log.Printf("cloudflare API access verified for record %s", cfg.Cloudflare.RecordID)
		}
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
	var firstOutcome *cycleOutcome
	if cfg.HasAirportProfiles() {
		firstOutcome = runAirportProfiles(cfg, ws, sc)
	} else {
		firstOutcome = runOnce(cfg, cf, ws, sc)
	}
	now := time.Now()
	nextCheck := now.Add(cfg.CheckInterval)
	if ws != nil {
		st := ws.GetStatus()
		applyConfigStatus(st, cfg, &nextCheck, now)
		if cfg.HasAirportProfiles() {
			st.Profiles = firstOutcome.Profiles
			applyProfileSummary(st, firstOutcome.Profiles)
			for _, profile := range firstOutcome.Profiles {
				for _, region := range profile.Regions {
					if region.CurrentIP != "" && region.Latency > 0 {
						ws.AddHistory(web.CheckRecord{
							Time: now, ProfileID: profile.ID, Region: region.Region,
							IP: region.CurrentIP, Latency: region.Latency, Success: true,
						})
					}
				}
			}
		} else if applyCycleOutcome(st, firstOutcome) {
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
