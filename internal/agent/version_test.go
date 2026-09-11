package agent

import (
	"testing"

	"dns-latency-router/internal/config"
)

func TestRunJobIncludesVersion(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{ID: "agent-test"}}
	report := runJob(cfg, &JobResponse{})
	if report.Version != Version {
		t.Fatalf("report version = %q, want %q", report.Version, Version)
	}
}
