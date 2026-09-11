package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dns-latency-router/internal/checker"
	"gopkg.in/yaml.v3"
)

type CloudflareConfig struct {
	APIToken string `yaml:"api_token"`
	ZoneID   string `yaml:"zone_id"`
	RecordID string `yaml:"record_id"`
}

type RegionRecord struct {
	Label        string           `yaml:"label"`
	CustomDomain string           `yaml:"custom_domain"`
	RecordID     string           `yaml:"record_id"`
	Cloudflare   CloudflareConfig `yaml:"cloudflare"`
}

type AgentConfig struct {
	ID                string `yaml:"id"`
	Name              string `yaml:"name"`
	ControllerURL     string `yaml:"controller_url"`
	Token             string `yaml:"token"`
	ProbeSource       string `yaml:"probe_source"`
	Carrier           string `yaml:"carrier"`
	ReportIntervalSec int    `yaml:"report_interval_seconds"`
	ReportTTLSeconds  int    `yaml:"report_ttl_seconds"`
}

type AgentPeerConfig struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	ProbeSource string `yaml:"probe_source"`
	Carrier     string `yaml:"carrier"`
}

type AirportProfile struct {
	ID             string                  `yaml:"id"`
	Name           string                  `yaml:"name"`
	Slug           string                  `yaml:"slug"`
	TargetDomain   string                  `yaml:"target_domain,omitempty"`
	TargetDomains  []string                `yaml:"target_domains"`
	ProbeSource    string                  `yaml:"probe_source"`
	Carrier        string                  `yaml:"carrier"`
	EntryRecord    RegionRecord            `yaml:"entry_record"`
	RegionRecords  map[string]RegionRecord `yaml:"region_records,omitempty"`
	CarrierRecords map[string]RegionRecord `yaml:"carrier_records,omitempty"`
}

type Config struct {
	NodeRole               string            `yaml:"node_role"`
	Agent                  AgentConfig       `yaml:"agent"`
	Agents                 []AgentPeerConfig `yaml:"agents"`
	Cloudflare             CloudflareConfig  `yaml:"cloudflare"`
	BaseDomain             string            `yaml:"base_domain"`
	AirportProfiles        []AirportProfile  `yaml:"airport_profiles"`
	TargetDomain           string            `yaml:"target_domain"`
	CustomDomain           string            `yaml:"custom_domain"`
	ProbeSource            string            `yaml:"probe_source"`
	Carrier                string            `yaml:"carrier"`
	CheckIntervalSec       int               `yaml:"check_interval"`
	ProxyURL               string            `yaml:"proxy_url"` // SOCKS5/HTTP proxy for Cloudflare API
	PingMode               string            `yaml:"ping_mode"` // "icmp" or "tcp"
	PingPort               int               `yaml:"ping_port"`
	PingTimeoutSec         int               `yaml:"ping_timeout_seconds"`
	PingAttempts           int               `yaml:"ping_attempts"`
	PingMinThresholdMs     float64           `yaml:"ping_min_threshold_ms"`
	LatencyWeight          float64           `yaml:"selection_latency_weight"`
	JitterWeight           float64           `yaml:"selection_jitter_weight"`
	LossWeight             float64           `yaml:"selection_loss_weight"`
	SwitchImprovement      float64           `yaml:"switch_improvement_percent"`
	SwitchStableSec        int               `yaml:"switch_stable_seconds"`
	FailureConfirmCycles   int               `yaml:"failure_confirm_cycles"`
	FailedOrphanTTLHours   int               `yaml:"failed_orphan_ttl_hours"`
	FallbackBaselineIP     string            `yaml:"fallback_baseline_ip"`
	AlertWebhookURL        string            `yaml:"alert_webhook_url"`
	TimePenaltyStartHour   int               `yaml:"time_penalty_start_hour"`
	TimePenaltyEndHour     int               `yaml:"time_penalty_end_hour"`
	TimePenaltyScore       float64           `yaml:"time_penalty_score"`
	TimePenaltyOrgKeywords string            `yaml:"time_penalty_org_keywords"`
	DNSServers             []string          `yaml:"dns_servers"`
	WebPort                int               `yaml:"web_port"`

	// derived
	PingTimeout      time.Duration `yaml:"-"`
	CheckInterval    time.Duration `yaml:"-"`
	PingMinThreshold time.Duration `yaml:"-"`
	FailedOrphanTTL  time.Duration `yaml:"-"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		NodeRole:               "standalone",
		CheckIntervalSec:       300,
		ProbeSource:            "本机网络",
		Carrier:                "auto",
		PingMode:               "icmp",
		PingPort:               443,
		PingTimeoutSec:         5,
		PingAttempts:           4,
		PingMinThresholdMs:     1,
		LatencyWeight:          1.0,
		JitterWeight:           0.35,
		LossWeight:             4.0,
		SwitchImprovement:      15,
		SwitchStableSec:        120,
		FailedOrphanTTLHours:   24,
		FailureConfirmCycles:   3,
		TimePenaltyStartHour:   0,
		TimePenaltyEndHour:     5,
		TimePenaltyScore:       60,
		TimePenaltyOrgKeywords: "Google LLC",
		WebPort:                0, // 0 = disabled
		Agent: AgentConfig{
			ReportIntervalSec: 300,
			ReportTTLSeconds:  900,
		},
		DNSServers: []string{
			"114.114.114.114", // China Telecom
			"223.5.5.5",       // Alibaba (Aliyun)
			"119.29.29.29",    // DNSPod (Tencent)
			"180.76.76.76",    // Baidu
			"8.8.8.8",         // Google (fallback)
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.Normalize(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (cfg *Config) Normalize() error {
	cfg.NodeRole = NormalizeNodeRole(cfg.NodeRole)
	cfg.Agent.Carrier = NormalizeCarrier(cfg.Agent.Carrier)
	if err := cfg.normalizeAgentPeers(); err != nil {
		return err
	}
	if cfg.Agent.ReportIntervalSec <= 0 {
		cfg.Agent.ReportIntervalSec = cfg.CheckIntervalSec
	}
	if cfg.Agent.ReportTTLSeconds <= 0 {
		cfg.Agent.ReportTTLSeconds = maxInt(cfg.Agent.ReportIntervalSec*3, 900)
	}
	if cfg.IsAgentMode() {
		if strings.TrimSpace(cfg.Agent.ID) == "" {
			return fmt.Errorf("agent.id is required in agent mode")
		}
		if strings.TrimSpace(cfg.Agent.ControllerURL) == "" {
			return fmt.Errorf("agent.controller_url is required in agent mode")
		}
		if strings.TrimSpace(cfg.Agent.Token) == "" {
			return fmt.Errorf("agent.token is required in agent mode")
		}
		if _, err := url.ParseRequestURI(strings.TrimSpace(cfg.Agent.ControllerURL)); err != nil {
			return fmt.Errorf("agent.controller_url is invalid: %w", err)
		}
	}
	if !cfg.IsAgentMode() && cfg.Cloudflare.APIToken == "" {
		return fmt.Errorf("cloudflare.api_token is required")
	}
	if !cfg.IsAgentMode() && cfg.Cloudflare.ZoneID == "" && len(cfg.AirportProfiles) == 0 {
		return fmt.Errorf("cloudflare.zone_id is required")
	}
	if !cfg.IsAgentMode() && cfg.Cloudflare.RecordID == "" && len(cfg.AirportProfiles) == 0 {
		return fmt.Errorf("cloudflare.record_id is required")
	}
	if cfg.TargetDomain == "" && len(cfg.AirportProfiles) == 0 && !cfg.IsAgentMode() {
		return fmt.Errorf("target_domain is required")
	}
	if cfg.FailureConfirmCycles <= 0 {
		cfg.FailureConfirmCycles = 3
	}
	if cfg.PingAttempts < 1 {
		cfg.PingAttempts = 1
	}
	if cfg.PingPort <= 0 {
		cfg.PingPort = 443
	}
	if cfg.PingPort > 65535 {
		return fmt.Errorf("ping_port must be between 1 and 65535")
	}
	if cfg.SwitchStableSec < 0 {
		cfg.SwitchStableSec = 0
	}
	if cfg.FailedOrphanTTLHours < 0 {
		cfg.FailedOrphanTTLHours = 0
	}
	if cfg.TimePenaltyStartHour < 0 || cfg.TimePenaltyStartHour > 23 {
		return fmt.Errorf("time_penalty_start_hour must be between 0 and 23")
	}
	if cfg.TimePenaltyEndHour < 0 || cfg.TimePenaltyEndHour > 24 {
		return fmt.Errorf("time_penalty_end_hour must be between 0 and 24")
	}
	if cfg.TimePenaltyScore < 0 {
		return fmt.Errorf("time_penalty_score cannot be negative")
	}
	for name, value := range map[string]float64{
		"selection_latency_weight": cfg.LatencyWeight, "selection_jitter_weight": cfg.JitterWeight,
		"selection_loss_weight": cfg.LossWeight, "switch_improvement_percent": cfg.SwitchImprovement,
		"ping_min_threshold_ms": cfg.PingMinThresholdMs, "time_penalty_score": cfg.TimePenaltyScore,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("%s must be finite and non-negative", name)
		}
	}
	if cfg.FallbackBaselineIP != "" {
		ip, ok := checker.NormalizeCandidateIP(cfg.FallbackBaselineIP)
		if !ok {
			return fmt.Errorf("fallback_baseline_ip must be a usable public IPv4 address")
		}
		cfg.FallbackBaselineIP = ip
	}
	if cfg.AlertWebhookURL != "" {
		if _, err := url.ParseRequestURI(strings.TrimSpace(cfg.AlertWebhookURL)); err != nil {
			return fmt.Errorf("alert_webhook_url is invalid: %w", err)
		}
	}
	cfg.Carrier = NormalizeCarrier(cfg.Carrier)
	if err := cfg.normalizeAirportProfiles(); err != nil {
		return err
	}

	cfg.PingTimeout = time.Duration(cfg.PingTimeoutSec) * time.Second
	cfg.CheckInterval = time.Duration(cfg.CheckIntervalSec) * time.Second
	cfg.PingMinThreshold = time.Duration(cfg.PingMinThresholdMs * float64(time.Millisecond))
	cfg.FailedOrphanTTL = time.Duration(cfg.FailedOrphanTTLHours) * time.Hour

	return nil
}

func (cfg *Config) normalizeAgentPeers() error {
	seen := make(map[string]struct{}, len(cfg.Agents))
	out := make([]AgentPeerConfig, 0, len(cfg.Agents))
	for i := range cfg.Agents {
		peer := cfg.Agents[i]
		peer.ID = strings.TrimSpace(peer.ID)
		if peer.ID == "" {
			return fmt.Errorf("agents[%d].id is required", i)
		}
		if strings.EqualFold(peer.ID, "controller") {
			return fmt.Errorf("agents[%s] uses reserved id controller", peer.ID)
		}
		key := strings.ToLower(peer.ID)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("agents[%s] is duplicated", peer.ID)
		}
		seen[key] = struct{}{}
		peer.Name = strings.TrimSpace(peer.Name)
		if peer.Name == "" {
			peer.Name = peer.ID
		}
		peer.ProbeSource = strings.TrimSpace(peer.ProbeSource)
		peer.Carrier = NormalizeCarrier(peer.Carrier)
		out = append(out, peer)
	}
	cfg.Agents = out
	return nil
}

func Save(path string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if err := cfg.Normalize(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return atomicWriteFile(path, data, 0644)
}

// atomicWriteFile replaces path only after the complete new contents have
// been written and closed. The temporary file is created beside the target so
// Rename remains a single-filesystem operation.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temporary config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func normalizeSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	prevDash := false
	for _, r := range value {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if (r == '-' || r == '_' || r == ' ') && !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func NormalizeRegion(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "香港", "hongkong", "hong_kong":
		return "hk"
	case "马来西亚", "malaysia":
		return "my"
	case "新加坡", "singapore":
		return "sg"
	case "日本", "japan":
		return "jp"
	case "台湾", "taiwan":
		return "tw"
	case "澳门", "macao", "macau":
		return "mo"
	case "中国", "中国大陆", "大陆", "china", "cn":
		return "cn"
	case "美国", "usa", "united_states":
		return "us"
	default:
		return normalizeSlug(value)
	}
}

func RegionLabel(region string) string {
	switch NormalizeRegion(region) {
	case "hk":
		return "香港"
	case "my":
		return "马来西亚"
	case "sg":
		return "新加坡"
	case "jp":
		return "日本"
	case "tw":
		return "台湾"
	case "mo":
		return "澳门"
	case "cn":
		return "中国大陆"
	case "us":
		return "美国"
	case "default":
		return "默认"
	case "unknown":
		return "未知地区"
	default:
		return strings.ToUpper(region)
	}
}

func countryCodeToRegion(countryCode string) string {
	switch strings.ToUpper(strings.TrimSpace(countryCode)) {
	case "HK":
		return "hk"
	case "MY":
		return "my"
	case "SG":
		return "sg"
	case "JP":
		return "jp"
	case "TW":
		return "tw"
	case "MO":
		return "mo"
	case "CN":
		return "cn"
	case "US":
		return "us"
	default:
		return "unknown"
	}
}

func CountryCodeToRegion(countryCode string) string {
	return countryCodeToRegion(countryCode)
}

func CarrierDomainPrefix(carrier string) string {
	switch NormalizeCarrier(carrier) {
	case "unicom":
		return "cu"
	case "telecom":
		return "ct"
	case "mobile":
		return "cm"
	case "all":
		return "all"
	default:
		return "auto"
	}
}

func ProfileRecordDomain(baseDomain string, profile AirportProfile, region string) string {
	return profileRecordDomain(baseDomain, profile, EffectiveCarrierFor(profile.Carrier, profile.ProbeSource), region)
}

func CarrierRecordDomain(baseDomain string, profile AirportProfile, carrier, region string) string {
	return profileRecordDomain(baseDomain, profile, carrier, region)
}

func profileRecordDomain(baseDomain string, profile AirportProfile, carrier, region string) string {
	base := strings.TrimPrefix(strings.TrimSpace(baseDomain), ".")
	slug := normalizeSlug(profile.Slug)
	if slug == "" {
		slug = normalizeSlug(profile.ID)
	}
	region = NormalizeRegion(region)
	prefix := CarrierDomainPrefix(carrier)
	if base == "" || slug == "" || region == "" || prefix == "" {
		return ""
	}
	return fmt.Sprintf("%s-%s-%s.%s", prefix, slug, region, base)
}

func shouldUseGeneratedProfileDomain(current, baseDomain string, profile AirportProfile, carrier, region string) bool {
	current = strings.TrimSpace(current)
	if current == "" {
		return true
	}
	base := strings.TrimPrefix(strings.TrimSpace(baseDomain), ".")
	slug := normalizeSlug(profile.Slug)
	if slug == "" {
		slug = normalizeSlug(profile.ID)
	}
	region = NormalizeRegion(region)
	if base == "" || slug == "" || region == "" {
		return false
	}
	legacyRegion := fmt.Sprintf("%s-%s.%s", slug, region, base)
	legacyCarrier := fmt.Sprintf("%s-%s.%s", slug, NormalizeCarrier(carrier), base)
	return strings.EqualFold(current, legacyRegion) || strings.EqualFold(current, legacyCarrier)
}

func (c *Config) normalizeAirportProfiles() error {
	for i := range c.AirportProfiles {
		p := &c.AirportProfiles[i]
		if p.ID == "" {
			p.ID = p.Slug
		}
		p.ID = normalizeSlug(p.ID)
		p.Slug = normalizeSlug(p.Slug)
		if p.Slug == "" {
			p.Slug = p.ID
		}
		if p.ID == "" {
			p.ID = p.Slug
		}
		if p.ID == "" {
			return fmt.Errorf("airport_profiles[%d].id or slug is required", i)
		}
		if p.Name == "" {
			p.Name = p.ID
		}
		p.TargetDomains = normalizeDomainList(p.TargetDomains)
		if p.TargetDomain != "" {
			p.TargetDomain = strings.TrimSpace(p.TargetDomain)
			p.TargetDomains = dedupeStrings(append([]string{p.TargetDomain}, p.TargetDomains...))
		}
		if len(p.TargetDomains) == 0 {
			return fmt.Errorf("airport_profiles[%s].target_domain or target_domains is required", p.ID)
		}
		p.TargetDomain = p.TargetDomains[0]
		if p.ProbeSource == "" {
			p.ProbeSource = c.ProbeSource
		}
		if p.Carrier == "" {
			p.Carrier = c.Carrier
		}
		p.Carrier = NormalizeCarrier(p.Carrier)
		if p.EntryRecord.RecordID == "" {
			p.EntryRecord.RecordID = p.EntryRecord.Cloudflare.RecordID
		}
		if p.EntryRecord.Label == "" {
			p.EntryRecord.Label = "全局最快"
		}
		if shouldUseGeneratedProfileDomain(p.EntryRecord.CustomDomain, c.BaseDomain, *p, p.Carrier, "entry") {
			p.EntryRecord.CustomDomain = ProfileRecordDomain(c.BaseDomain, *p, "entry")
		}
		nextRecords := make(map[string]RegionRecord, len(p.RegionRecords))
		for key, rec := range p.RegionRecords {
			region := NormalizeRegion(key)
			if region == "" {
				return fmt.Errorf("airport_profiles[%s].region_records has empty region key", p.ID)
			}
			if rec.Label == "" {
				rec.Label = RegionLabel(region)
			}
			if shouldUseGeneratedProfileDomain(rec.CustomDomain, c.BaseDomain, *p, p.Carrier, region) {
				rec.CustomDomain = ProfileRecordDomain(c.BaseDomain, *p, region)
			}
			if rec.RecordID == "" {
				rec.RecordID = rec.Cloudflare.RecordID
			}
			if rec.RecordID == "" && rec.CustomDomain == "" {
				return fmt.Errorf("airport_profiles[%s].region_records[%s] requires record_id or custom_domain", p.ID, region)
			}
			nextRecords[region] = rec
		}
		if len(nextRecords) > 0 {
			p.RegionRecords = nextRecords
		} else {
			p.RegionRecords = nil
		}
		nextCarrierRecords := make(map[string]RegionRecord, len(p.CarrierRecords))
		for key, rec := range p.CarrierRecords {
			carrier := NormalizeCarrier(key)
			if carrier == "" || carrier == "auto" || carrier == "all" {
				return fmt.Errorf("airport_profiles[%s].carrier_records has invalid carrier key %q", p.ID, key)
			}
			if rec.Label == "" {
				rec.Label = CarrierLabel(carrier)
			}
			if shouldUseGeneratedProfileDomain(rec.CustomDomain, c.BaseDomain, *p, carrier, "entry") {
				rec.CustomDomain = CarrierRecordDomain(c.BaseDomain, *p, carrier, "entry")
			}
			if rec.RecordID == "" {
				rec.RecordID = rec.Cloudflare.RecordID
			}
			if rec.RecordID == "" && rec.CustomDomain == "" {
				return fmt.Errorf("airport_profiles[%s].carrier_records[%s] requires record_id or custom_domain", p.ID, carrier)
			}
			nextCarrierRecords[carrier] = rec
		}
		if len(nextCarrierRecords) > 0 {
			p.CarrierRecords = nextCarrierRecords
		} else {
			p.CarrierRecords = nil
		}
	}
	return nil
}

func normalizeDomainList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return dedupeStrings(out)
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		key := strings.ToLower(strings.TrimSpace(value))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, strings.TrimSpace(value))
	}
	return out
}

func (c *Config) HasAirportProfiles() bool {
	return len(c.AirportProfiles) > 0
}

func (c *Config) IsAgentMode() bool {
	return NormalizeNodeRole(c.NodeRole) == "agent"
}

func (c *Config) IsControllerMode() bool {
	role := NormalizeNodeRole(c.NodeRole)
	return role == "controller" || role == "standalone"
}

func (c *Config) RunsLocalProbes() bool {
	return NormalizeNodeRole(c.NodeRole) == "standalone"
}

func (c *Config) LegacyProfile() AirportProfile {
	slug := "default"
	if c.CustomDomain != "" {
		host := strings.Split(c.CustomDomain, ".")[0]
		if parts := strings.Split(host, "-"); len(parts) > 0 && parts[0] != "" {
			slug = normalizeSlug(parts[0])
		}
	}
	if slug == "" {
		slug = "default"
	}
	return AirportProfile{
		ID:            slug,
		Name:          slug,
		Slug:          slug,
		TargetDomain:  c.TargetDomain,
		TargetDomains: []string{c.TargetDomain},
		ProbeSource:   c.ProbeSource,
		Carrier:       c.Carrier,
		RegionRecords: map[string]RegionRecord{
			"default": {
				Label:        "默认",
				CustomDomain: c.CustomDomain,
				RecordID:     c.Cloudflare.RecordID,
			},
		},
	}
}

func (c *Config) TimePenaltyKeywords() []string {
	parts := strings.Split(c.TimePenaltyOrgKeywords, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, strings.ToLower(part))
		}
	}
	return out
}

func (c *Config) TimePenaltyActiveAt(now time.Time) bool {
	start := c.TimePenaltyStartHour
	end := c.TimePenaltyEndHour
	if start == end {
		return false
	}
	hour := now.Hour()
	if start < end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}

// NormalizeCarrier maps UI/config values to the small carrier strategy set.
func NormalizeCarrier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "unicom", "cu", "china_unicom", "联通", "中国联通":
		return "unicom"
	case "telecom", "ct", "china_telecom", "电信", "中国电信":
		return "telecom"
	case "mobile", "cm", "china_mobile", "移动", "中国移动":
		return "mobile"
	case "all", "default", "custom", "global", "全量", "全部":
		return "all"
	default:
		return "auto"
	}
}

func NormalizeNodeRole(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "agent", "probe_agent", "child", "worker", "子机":
		return "agent"
	case "controller", "master", "server", "host", "主机", "主控":
		return "controller"
	default:
		return "standalone"
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// InferCarrier guesses the probe network from the human-readable probe source.
func InferCarrier(probeSource string) string {
	source := strings.ToLower(strings.TrimSpace(probeSource))
	switch {
	case strings.Contains(source, "unicom"), strings.Contains(source, "联通"):
		return "unicom"
	case strings.Contains(source, "telecom"), strings.Contains(source, "电信"):
		return "telecom"
	case strings.Contains(source, "mobile"), strings.Contains(source, "移动"):
		return "mobile"
	default:
		return "all"
	}
}

func CarrierLabel(carrier string) string {
	switch NormalizeCarrier(carrier) {
	case "unicom":
		return "中国联通"
	case "telecom":
		return "中国电信"
	case "mobile":
		return "中国移动"
	case "all":
		return "全量解析池"
	default:
		return "自动识别"
	}
}

// EffectiveCarrier returns the concrete carrier strategy used by the resolver.
func (c *Config) EffectiveCarrier() string {
	carrier := NormalizeCarrier(c.Carrier)
	if carrier == "auto" {
		return InferCarrier(c.ProbeSource)
	}
	return carrier
}

func EffectiveCarrierFor(carrier, probeSource string) string {
	normalized := NormalizeCarrier(carrier)
	if normalized == "auto" {
		return InferCarrier(probeSource)
	}
	return normalized
}

func (c *Config) EffectiveCarrierLabel() string {
	if NormalizeCarrier(c.Carrier) == "auto" {
		return CarrierLabel(c.EffectiveCarrier()) + "（自动）"
	}
	return CarrierLabel(c.EffectiveCarrier())
}

func EffectiveCarrierLabelFor(carrier, probeSource string) string {
	if NormalizeCarrier(carrier) == "auto" {
		return CarrierLabel(EffectiveCarrierFor(carrier, probeSource)) + "（自动）"
	}
	return CarrierLabel(EffectiveCarrierFor(carrier, probeSource))
}

func dedupeServers(primary, fallback []string) []string {
	seen := make(map[string]struct{}, len(primary)+len(fallback))
	servers := make([]string, 0, len(primary)+len(fallback))
	for _, server := range append(primary, fallback...) {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		if _, ok := seen[server]; ok {
			continue
		}
		seen[server] = struct{}{}
		servers = append(servers, server)
	}
	return servers
}

// EffectiveDNSServers chooses a resolver pool that matches the carrier strategy.
func (c *Config) EffectiveDNSServers() []string {
	return c.EffectiveDNSServersFor(c.Carrier, c.ProbeSource)
}

func (c *Config) EffectiveDNSServersFor(carrier, probeSource string) []string {
	fallback := c.DNSServers
	switch EffectiveCarrierFor(carrier, probeSource) {
	case "unicom":
		return dedupeServers([]string{
			"123.125.81.6",  // China Unicom Beijing
			"140.207.198.6", // China Unicom Shanghai
			"119.29.29.29",  // DNSPod fallback
		}, fallback)
	case "telecom":
		return dedupeServers([]string{
			"114.114.114.114", // 114DNS, telecom-friendly public resolver
			"180.76.76.76",    // Baidu public resolver
			"223.5.5.5",       // AliDNS fallback
		}, fallback)
	case "mobile":
		return dedupeServers([]string{
			"211.136.112.50", // China Mobile
			"221.131.143.69", // China Mobile
			"223.5.5.5",      // AliDNS fallback
		}, fallback)
	default:
		return fallback
	}
}

// UpdateYAMLField updates a root-level field in the YAML config file. It uses
// yaml.Node so values are escaped according to YAML rules and a nested field
// with the same name can never be accidentally changed. If quoted is true,
// the value is emitted as a double-quoted YAML string.
func UpdateYAMLField(path, key, value string, quoted bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config for update: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config for update: %w", err)
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config root must be a mapping")
	}
	newValue := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	if quoted {
		newValue.Style = yaml.DoubleQuotedStyle
	} else {
		// Keep the caller's scalar syntax (numbers, booleans, etc.) valid while
		// avoiding string interpolation into YAML source.
		var parsed yaml.Node
		if err := yaml.Unmarshal([]byte(value), &parsed); err == nil && len(parsed.Content) == 1 && parsed.Content[0].Kind == yaml.ScalarNode {
			newValue = parsed.Content[0]
		}
	}
	found := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i+1] = newValue
			found = true
			break
		}
	}
	if !found {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, newValue)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("marshal config update: %w", err)
	}
	return atomicWriteFile(path, out, 0644)
}
