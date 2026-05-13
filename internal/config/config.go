package config

import (
	"fmt"
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
	Cloudflare         CloudflareConfig `yaml:"cloudflare"`
	TargetDomain       string           `yaml:"target_domain"`
	CustomDomain       string           `yaml:"custom_domain"`
	CheckIntervalSec   int              `yaml:"check_interval"`
	PingPort           int              `yaml:"ping_port"`
	PingTimeoutSec     int              `yaml:"ping_timeout_seconds"`
	PingMinThresholdMs float64          `yaml:"ping_min_threshold_ms"`
	DNSServers         []string         `yaml:"dns_servers"`
	WebPort            int              `yaml:"web_port"`

	// derived
	PingTimeout      time.Duration
	CheckInterval    time.Duration
	PingMinThreshold time.Duration
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		CheckIntervalSec:   300,
		PingPort:           443,
		PingTimeoutSec:     5,
		PingMinThresholdMs: 1,
		WebPort:            0, // 0 = disabled
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

	cfg.PingTimeout = time.Duration(cfg.PingTimeoutSec) * time.Second
	cfg.CheckInterval = time.Duration(cfg.CheckIntervalSec) * time.Second
	cfg.PingMinThreshold = time.Duration(cfg.PingMinThresholdMs * float64(time.Millisecond))

	return cfg, nil
}

// UpdateYAMLField updates a specific field in the YAML config file,
// preserving comments and formatting (line-based replacement).
func UpdateYAMLField(path, key, value string) error {
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
			lines[i] = indent + key + ": \"" + value + "\""
			replaced = true
			break
		}
	}

	if !replaced {
		return fmt.Errorf("key %q not found in config", key)
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}
