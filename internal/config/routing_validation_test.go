package config

import (
	"math"
	"testing"
)

func TestFallbackMustBePublicIPv4(t *testing.T) {
	for _, ip := range []string{"10.0.0.1", "127.0.0.1", "203.0.113.1", "2001:4860:4860::8888"} {
		cfg := &Config{Cloudflare: CloudflareConfig{APIToken: "test", ZoneID: "zone", RecordID: "record"}, TargetDomain: "entry.example", FallbackBaselineIP: ip}
		if err := cfg.Normalize(); err == nil {
			t.Errorf("accepted unusable A-record fallback %s", ip)
		}
	}
}

func TestRoutingWeightsMustBeFinite(t *testing.T) {
	for _, weight := range []float64{math.NaN(), math.Inf(1), -1} {
		cfg := &Config{Cloudflare: CloudflareConfig{APIToken: "test", ZoneID: "zone", RecordID: "record"}, TargetDomain: "entry.example", LossWeight: weight}
		if err := cfg.Normalize(); err == nil {
			t.Errorf("accepted invalid loss weight %v", weight)
		}
	}
}
