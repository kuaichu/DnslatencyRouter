package web

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"dns-latency-router/internal/agent"
	"dns-latency-router/internal/config"
)

func agentValidationConfig() *config.Config {
	return &config.Config{
		TargetDomain: "entry.example",
		AirportProfiles: []config.AirportProfile{{
			ID: "sntp",
		}},
	}
}

func validAgentReport() agent.Report {
	return agent.Report{
		AgentID: "agent-a",
		Profiles: []agent.ProfileReport{{
			ProfileID:   "sntp",
			ResolvedIPs: []string{"8.8.8.8"},
			Results: []agent.Result{{
				IP:        "8.8.8.8",
				Latency:   12,
				Jitter:    1,
				LossRate:  0,
				Attempts:  4,
				Successes: 4,
				Score:     12.35,
			}},
		}},
	}
}

func TestValidateAgentReportEnforcesProfileOwnershipAndResultInvariants(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*agent.Report)
	}{
		{"unknown profile", func(report *agent.Report) { report.Profiles[0].ProfileID = "other" }},
		{"result outside resolved set", func(report *agent.Report) { report.Profiles[0].Results[0].IP = "1.1.1.1" }},
		{"duplicate result", func(report *agent.Report) {
			report.Profiles[0].Results = append(report.Profiles[0].Results, report.Profiles[0].Results[0])
		}},
		{"non finite latency", func(report *agent.Report) { report.Profiles[0].Results[0].Latency = math.NaN() }},
		{"inconsistent attempts", func(report *agent.Report) { report.Profiles[0].Results[0].Successes = 5 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := validAgentReport()
			tc.mutate(&report)
			if _, err := validateAgentReport(report, agentValidationConfig()); err == nil {
				t.Fatal("invalid report was accepted")
			}
		})
	}
}

func TestValidateAgentReportKeepsUnknownAgentIDButRejectsReservedID(t *testing.T) {
	report := validAgentReport()
	report.AgentID = "unregistered-agent"
	if _, err := validateAgentReport(report, agentValidationConfig()); err != nil {
		t.Fatalf("unknown agent ID should remain compatible: %v", err)
	}
	report.AgentID = "controller"
	if _, err := validateAgentReport(report, agentValidationConfig()); err == nil {
		t.Fatal("reserved controller ID was accepted")
	}
}

func TestAgentReportHandlerRejectsForeignProfileBeforePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "target_domain: entry.example\ncloudflare:\n  api_token: test\n  zone_id: zone\n  record_id: record\nagent:\n  token: token\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfgPath: path, agentReports: make(map[string]agent.Report), sseClients: make(map[string]chan sseEvent)}
	s.UpdateStatus(&Status{})
	report := validAgentReport()
	report.Profiles[0].ProfileID = "foreign"
	body, _ := json.Marshal(report)
	r := httptest.NewRequest("POST", "/api/agent/reports", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	s.handleAPIAgentReports(w, r)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if len(s.agentReports) != 0 {
		t.Fatal("rejected report was persisted")
	}
}

func TestAgentJobCandidatesIncludeCurrentRouteForSameProfile(t *testing.T) {
	s := &Server{}
	s.UpdateStatus(&Status{Profiles: []ProfileStatus{
		{ID: "a", Regions: []RegionStatus{{CurrentIP: "8.8.8.8"}}},
		{ID: "b", Regions: []RegionStatus{{CurrentIP: "1.1.1.1"}}},
	}})
	ips := s.candidateIPsForProfile("a")
	if len(ips) != 1 || ips[0] != "8.8.8.8" {
		t.Fatalf("current route missing or wrong airport included: %v", ips)
	}
}
