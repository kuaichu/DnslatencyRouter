package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type CloudflareConfig struct {
	APIToken string `yaml:"api_token"`
	ZoneID   string `yaml:"zone_id"`
	RecordID string `yaml:"record_id"`
}

type Config struct {
	Cloudflare             CloudflareConfig `yaml:"cloudflare"`
	TargetDomain           string           `yaml:"target_domain"`
	CustomDomain           string           `yaml:"custom_domain"`
	ProbeSource            string           `yaml:"probe_source"`
	Carrier                string           `yaml:"carrier"`
	CheckIntervalSec       int              `yaml:"check_interval"`
	ProxyURL               string           `yaml:"proxy_url"` // SOCKS5/HTTP proxy for Cloudflare API
	PingMode               string           `yaml:"ping_mode"` // "icmp" or "tcp"
	PingPort               int              `yaml:"ping_port"`
	PingTimeoutSec         int              `yaml:"ping_timeout_seconds"`
	PingAttempts           int              `yaml:"ping_attempts"`
	PingMinThresholdMs     float64          `yaml:"ping_min_threshold_ms"`
	LatencyWeight          float64          `yaml:"selection_latency_weight"`
	JitterWeight           float64          `yaml:"selection_jitter_weight"`
	LossWeight             float64          `yaml:"selection_loss_weight"`
	SwitchImprovement      float64          `yaml:"switch_improvement_percent"`
	SwitchStableSec        int              `yaml:"switch_stable_seconds"`
	FailedOrphanTTLHours   int              `yaml:"failed_orphan_ttl_hours"`
	FallbackBaselineIP     string           `yaml:"fallback_baseline_ip"`
	AlertWebhookURL        string           `yaml:"alert_webhook_url"`
	TimePenaltyStartHour   int              `yaml:"time_penalty_start_hour"`
	TimePenaltyEndHour     int              `yaml:"time_penalty_end_hour"`
	TimePenaltyScore       float64          `yaml:"time_penalty_score"`
	TimePenaltyOrgKeywords string           `yaml:"time_penalty_org_keywords"`
	DNSServers             []string         `yaml:"dns_servers"`
	WebPort                int              `yaml:"web_port"`

	// derived
	PingTimeout      time.Duration
	CheckInterval    time.Duration
	PingMinThreshold time.Duration
	FailedOrphanTTL  time.Duration
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		CheckIntervalSec:       300,
		ProbeSource:            "宁波联通",
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
		TimePenaltyStartHour:   0,
		TimePenaltyEndHour:     5,
		TimePenaltyScore:       60,
		TimePenaltyOrgKeywords: "Google LLC",
		WebPort:                0, // 0 = disabled
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

	// validate
	if cfg.Cloudflare.APIToken == "" {
		return nil, fmt.Errorf("cloudflare.api_token is required")
	}
	if cfg.Cloudflare.ZoneID == "" {
		return nil, fmt.Errorf("cloudflare.zone_id is required")
	}
	if cfg.Cloudflare.RecordID == "" {
		return nil, fmt.Errorf("cloudflare.record_id is required")
	}
	if cfg.TargetDomain == "" {
		return nil, fmt.Errorf("target_domain is required")
	}
	if cfg.PingAttempts < 1 {
		cfg.PingAttempts = 1
	}
	if cfg.SwitchStableSec < 0 {
		cfg.SwitchStableSec = 0
	}
	if cfg.FailedOrphanTTLHours < 0 {
		cfg.FailedOrphanTTLHours = 0
	}
	if cfg.TimePenaltyStartHour < 0 || cfg.TimePenaltyStartHour > 23 {
		return nil, fmt.Errorf("time_penalty_start_hour must be between 0 and 23")
	}
	if cfg.TimePenaltyEndHour < 0 || cfg.TimePenaltyEndHour > 24 {
		return nil, fmt.Errorf("time_penalty_end_hour must be between 0 and 24")
	}
	if cfg.TimePenaltyScore < 0 {
		return nil, fmt.Errorf("time_penalty_score cannot be negative")
	}
	if cfg.FallbackBaselineIP != "" && net.ParseIP(strings.TrimSpace(cfg.FallbackBaselineIP)) == nil {
		return nil, fmt.Errorf("fallback_baseline_ip must be a valid IP address")
	}
	if cfg.AlertWebhookURL != "" {
		if _, err := url.ParseRequestURI(strings.TrimSpace(cfg.AlertWebhookURL)); err != nil {
			return nil, fmt.Errorf("alert_webhook_url is invalid: %w", err)
		}
	}
	cfg.Carrier = NormalizeCarrier(cfg.Carrier)

	cfg.PingTimeout = time.Duration(cfg.PingTimeoutSec) * time.Second
	cfg.CheckInterval = time.Duration(cfg.CheckIntervalSec) * time.Second
	cfg.PingMinThreshold = time.Duration(cfg.PingMinThresholdMs * float64(time.Millisecond))
	cfg.FailedOrphanTTL = time.Duration(cfg.FailedOrphanTTLHours) * time.Hour

	return cfg, nil
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

func (c *Config) EffectiveCarrierLabel() string {
	if NormalizeCarrier(c.Carrier) == "auto" {
		return CarrierLabel(c.EffectiveCarrier()) + "（自动）"
	}
	return CarrierLabel(c.EffectiveCarrier())
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
	fallback := c.DNSServers
	switch c.EffectiveCarrier() {
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

// UpdateYAMLField updates a specific field in the YAML config file,
// preserving comments and formatting (line-based replacement).
// If quoted is true, the value is wrapped in double quotes (for string fields).
func UpdateYAMLField(path, key, value string, quoted bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config for update: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	prefix := key + ":"
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			if quoted {
				lines[i] = indent + key + ": \"" + value + "\""
			} else {
				lines[i] = indent + key + ": " + value
			}
			replaced = true
			break
		}
	}

	if !replaced {
		newLine := key + ": " + value
		if quoted {
			newLine = key + ": \"" + value + "\""
		}
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, newLine)
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}
