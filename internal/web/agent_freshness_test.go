package web

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dns-latency-router/internal/agent"
)

func TestAgentReceiptTimeCannotBeSuppliedByClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "target_domain: entry.example\ncloudflare:\n  api_token: test-only\n  zone_id: zone\n  record_id: record\nagent:\n  token: agent-test-only\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfgPath: path, agentReports: make(map[string]agent.Report)}
	s.UpdateStatus(&Status{})
	future := time.Now().Add(24 * time.Hour)
	body, _ := json.Marshal(agent.Report{AgentID: "agent-a", FinishedAt: future, ReceivedAt: future})
	r := httptest.NewRequest("POST", "/api/agent/reports", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer agent-test-only")
	w := httptest.NewRecorder()
	before := time.Now()
	s.handleAPIAgentReports(w, r)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("report rejected: %d %s", w.Code, w.Body.String())
	}
	report := s.agentReports["agent-a"]
	if report.ReceivedAt.Before(before) || report.ReceivedAt.After(time.Now()) {
		t.Fatal("receipt timestamp must come from controller")
	}
	// Even with a future client clock, the report expires after controller TTL.
	report.ReceivedAt = time.Now().Add(-time.Hour)
	s.agentReports["agent-a"] = report
	if reports := s.AgentReports(15 * time.Minute); len(reports) != 0 {
		t.Fatalf("stale report survived because of future client clock: %+v", reports)
	}
	// Older persisted reports lack ReceivedAt: future timestamps cannot be used.
	s.agentReports["agent-a"] = agent.Report{AgentID: "agent-a", FinishedAt: future}
	if reports := s.AgentReports(15 * time.Minute); len(reports) != 0 {
		t.Fatal("legacy future report must not participate in routing")
	}
}
