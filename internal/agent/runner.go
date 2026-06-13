package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"dns-latency-router/internal/checker"
	"dns-latency-router/internal/config"
)

func Run(cfg *config.Config) {
	client := &http.Client{Timeout: 30 * time.Second}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	log.Printf("[agent] starting %s (%s) -> %s", cfg.Agent.ID, agentCarrierLabel(cfg), cfg.Agent.ControllerURL)

	for {
		started := time.Now()
		job, err := fetchJob(client, cfg)
		if err != nil {
			log.Printf("[agent] fetch job failed: %v", err)
			if waitOrStop(sigCh, 30*time.Second) {
				return
			}
			continue
		}

		report := runJob(cfg, job)
		if err := postReport(client, cfg, report); err != nil {
			log.Printf("[agent] report failed: %v", err)
		} else {
			log.Printf("[agent] report sent, profiles=%d", len(report.Profiles))
		}

		interval := time.Duration(cfg.Agent.ReportIntervalSec) * time.Second
		if interval <= 0 && job.CheckInterval > 0 {
			interval = time.Duration(job.CheckInterval) * time.Second
		}
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		elapsed := time.Since(started)
		if elapsed < interval {
			if waitOrStop(sigCh, interval-elapsed) {
				return
			}
		}
	}
}

func waitOrStop(sigCh <-chan os.Signal, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case sig := <-sigCh:
		log.Printf("[agent] received %v, exiting", sig)
		return true
	case <-timer.C:
		return false
	}
}

func fetchJob(client *http.Client, cfg *config.Config) (*JobResponse, error) {
	req, err := http.NewRequest("GET", controllerEndpoint(cfg.Agent.ControllerURL, "/api/agent/jobs"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Agent.Token)
	req.Header.Set("X-Agent-ID", cfg.Agent.ID)
	req.Header.Set("X-Agent-Name", agentName(cfg))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("controller returned %s", resp.Status)
	}
	var job JobResponse
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, err
	}
	return &job, nil
}

func postReport(client *http.Client, cfg *config.Config, report Report) error {
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", controllerEndpoint(cfg.Agent.ControllerURL, "/api/agent/reports"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("controller returned %s", resp.Status)
	}
	return nil
}

func runJob(cfg *config.Config, job *JobResponse) Report {
	started := time.Now()
	source := firstString(job.AgentProbeSource, agentProbeSource(cfg))
	carrier := config.EffectiveCarrierFor(firstString(job.AgentCarrier, cfg.Agent.Carrier), source)
	name := firstString(job.AgentName, agentName(cfg))
	report := Report{
		AgentID:      cfg.Agent.ID,
		AgentName:    name,
		Carrier:      carrier,
		CarrierLabel: config.CarrierLabel(carrier),
		ProbeSource:  source,
		StartedAt:    started,
	}

	for _, profileJob := range job.Profiles {
		report.Profiles = append(report.Profiles, runProfileJob(cfg, job, profileJob, carrier, source))
	}
	report.FinishedAt = time.Now()
	return report
}

func runProfileJob(cfg *config.Config, job *JobResponse, profileJob ProfileJob, carrier, source string) ProfileReport {
	started := time.Now()
	profile := config.AirportProfile{
		ID:            profileJob.ID,
		Name:          profileJob.Name,
		Slug:          profileJob.Slug,
		TargetDomains: append([]string(nil), profileJob.TargetDomains...),
		ProbeSource:   source,
		Carrier:       carrier,
	}
	dnsServers := cfg.EffectiveDNSServersFor(carrier, source)
	log.Printf("[agent] [%s] resolving from %d DNS servers as %s", profile.ID, len(dnsServers), config.CarrierLabel(carrier))
	ips, err := resolveProfileIPs(profile, dnsServers)
	report := ProfileReport{
		ProfileID:     profile.ID,
		ProfileName:   profile.Name,
		TargetDomains: append([]string(nil), profile.TargetDomains...),
		StartedAt:     started,
	}
	if err != nil {
		report.Error = err.Error()
		report.FinishedAt = time.Now()
		log.Printf("[agent] [%s] resolve failed: %v", profile.ID, err)
		return report
	}
	report.ResolvedIPs = ips

	timeout := time.Duration(job.PingTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = cfg.PingTimeout
	}
	attempts := job.PingAttempts
	if attempts <= 0 {
		attempts = cfg.PingAttempts
	}
	mode := job.PingMode
	if mode == "" {
		mode = cfg.PingMode
	}
	port := job.PingPort
	if port <= 0 {
		port = cfg.PingPort
	}
	latencyWeight := firstPositive(job.LatencyWeight, cfg.LatencyWeight)
	jitterWeight := firstNonNegative(job.JitterWeight, cfg.JitterWeight)
	lossWeight := firstNonNegative(job.LossWeight, cfg.LossWeight)

	log.Printf("[agent] [%s] probing %d IP(s) via %s", profile.ID, len(ips), mode)
	results := checker.PingAll(ips, mode, port, timeout, attempts, latencyWeight, jitterWeight, lossWeight)
	for _, result := range results {
		report.Results = append(report.Results, resultFromChecker(result))
		if result.Err != nil {
			log.Printf("[agent] [%s] %s failed: %v", profile.ID, result.IP, result.Err)
			continue
		}
		log.Printf("[agent] [%s] %s %.2fms jitter %.2fms loss %.0f%% score %.2f",
			profile.ID, result.IP,
			float64(result.Latency.Microseconds())/1000.0,
			float64(result.Jitter.Microseconds())/1000.0,
			result.LossRate,
			result.Score,
		)
	}
	report.FinishedAt = time.Now()
	return report
}

func resolveProfileIPs(profile config.AirportProfile, dnsServers []string) ([]string, error) {
	targets := profile.TargetDomains
	if len(targets) == 0 && profile.TargetDomain != "" {
		targets = []string{profile.TargetDomain}
	}
	ipSet := make(map[string]struct{})
	var failures []string
	for _, domain := range targets {
		log.Printf("[agent] [%s] resolving %s from %d DNS servers ...", profile.ID, domain, len(dnsServers))
		ips, err := checker.ResolveFromAllDNS(domain, dnsServers)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", domain, err))
			log.Printf("[agent] [%s] dns resolve failed for %s: %v", profile.ID, domain, err)
			continue
		}
		log.Printf("[agent] [%s] %s discovered %d unique IP(s): %v", profile.ID, domain, len(ips), ips)
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

func resultFromChecker(result checker.Result) Result {
	out := Result{
		IP:        result.IP,
		Latency:   float64(result.Latency.Microseconds()) / 1000.0,
		Jitter:    float64(result.Jitter.Microseconds()) / 1000.0,
		LossRate:  result.LossRate,
		Attempts:  result.Attempts,
		Successes: result.Successes,
		Score:     result.Score,
	}
	if result.Err != nil {
		out.Error = result.Err.Error()
	}
	return out
}

func controllerEndpoint(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

func agentProbeSource(cfg *config.Config) string {
	if strings.TrimSpace(cfg.Agent.ProbeSource) != "" {
		return strings.TrimSpace(cfg.Agent.ProbeSource)
	}
	return strings.TrimSpace(cfg.ProbeSource)
}

func agentCarrier(cfg *config.Config) string {
	return config.EffectiveCarrierFor(cfg.Agent.Carrier, agentProbeSource(cfg))
}

func agentCarrierLabel(cfg *config.Config) string {
	return config.CarrierLabel(agentCarrier(cfg))
}

func agentName(cfg *config.Config) string {
	if strings.TrimSpace(cfg.Agent.Name) != "" {
		return strings.TrimSpace(cfg.Agent.Name)
	}
	return cfg.Agent.ID
}

func firstPositive(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 1
}

func firstNonNegative(values ...float64) float64 {
	for _, value := range values {
		if value >= 0 {
			return value
		}
	}
	return 0
}

func firstString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
