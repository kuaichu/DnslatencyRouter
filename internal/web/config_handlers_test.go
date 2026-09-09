package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dns-latency-router/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		TargetDomain:           "entry.example.com",
		CustomDomain:           "route.example.com",
		ProbeSource:            "本机网络",
		Carrier:                "auto",
		Cloudflare:             config.CloudflareConfig{APIToken: "secret-token", ZoneID: "zone", RecordID: "record"},
		PingMode:               "icmp",
		PingPort:               443,
		CheckIntervalSec:       300,
		PingAttempts:           4,
		LatencyWeight:          1,
		JitterWeight:           0.35,
		LossWeight:             4,
		SwitchImprovement:      15,
		SwitchStableSec:        120,
		TimePenaltyStartHour:   0,
		TimePenaltyEndHour:     5,
		TimePenaltyScore:       60,
		TimePenaltyOrgKeywords: "Google LLC",
	}
}

func newConfigTestServer(t *testing.T, cfg *config.Config) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfgPath: path, sseClients: make(map[string]chan sseEvent)}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.status.Store(&Status{
		TargetDomain: cfg.TargetDomain, CustomDomain: cfg.CustomDomain, ProbeSource: cfg.ProbeSource,
		Carrier: cfg.Carrier, PingMode: cfg.PingMode, PingPort: cfg.PingPort,
		CheckIntervalSec: cfg.CheckIntervalSec, PingAttempts: cfg.PingAttempts,
		LatencyWeight: cfg.LatencyWeight, JitterWeight: cfg.JitterWeight, LossWeight: cfg.LossWeight,
		SwitchImprovement: cfg.SwitchImprovement, SwitchStableSec: cfg.SwitchStableSec,
	})
	return s, path
}

func postConfig(t *testing.T, s *Server, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.handleAPIConfig(rr, req)
	return rr
}

func TestConfigInvalidValuesDoNotWriteOrCallback(t *testing.T) {
	s, path := newConfigTestServer(t, testConfig())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var callbackCount int
	s.onConfigApplied = func(*config.Config) { callbackCount++ }
	for _, payload := range []string{
		`{"ping_mode":"invalid"}`,
		`{"time_penalty_start_hour":24}`,
		`{"time_penalty_end_hour":25}`,
		`{"time_penalty_score":-1}`,
	} {
		rr := postConfig(t, s, payload)
		if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), `"ok":true`) {
			t.Fatalf("invalid payload %s unexpectedly succeeded: %s", payload, rr.Body.String())
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("invalid requests changed config file")
	}
	if callbackCount != 0 {
		t.Fatalf("callback count = %d, want 0", callbackCount)
	}
}

func TestConfigProbeSourceQuotesAndReloads(t *testing.T) {
	s, path := newConfigTestServer(t, testConfig())
	var got string
	s.onConfigApplied = func(cfg *config.Config) { got = cfg.ProbeSource }
	want := `ISP "quoted": line`
	payload, _ := json.Marshal(map[string]string{"probe_source": want})
	rr := postConfig(t, s, string(payload))
	if !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("probe source update failed: %s", rr.Body.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProbeSource != want || got != want {
		t.Fatalf("probe_source = %q, callback = %q, want %q", cfg.ProbeSource, got, want)
	}
}

func TestConfigConcurrentPostsPreserveBothFields(t *testing.T) {
	s, path := newConfigTestServer(t, testConfig())
	var wg sync.WaitGroup
	for _, payload := range []string{`{"ping_port":8443}`, `{"ping_attempts":7}`} {
		wg.Add(1)
		go func(payload string) {
			defer wg.Done()
			if rr := postConfig(t, s, payload); !strings.Contains(rr.Body.String(), `"ok":true`) {
				t.Errorf("concurrent update failed: %s", rr.Body.String())
			}
		}(payload)
	}
	wg.Wait()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PingPort != 8443 || cfg.PingAttempts != 7 {
		t.Fatalf("concurrent updates lost: port=%d attempts=%d", cfg.PingPort, cfg.PingAttempts)
	}
}

func TestConfigTokenIsWriteOnlyAndBlankDoesNotOverwrite(t *testing.T) {
	s, path := newConfigTestServer(t, testConfig())
	rr := httptest.NewRecorder()
	s.handleAPIConfig(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if strings.Contains(rr.Body.String(), "secret-token") || strings.Contains(rr.Body.String(), `cloudflare_api_token"`) && strings.Contains(rr.Body.String(), `secret-token`) {
		t.Fatalf("GET leaked API token: %s", rr.Body.String())
	}
	if got := postConfig(t, s, `{"cloudflare_api_token":"","ping_port":8444}`); !strings.Contains(got.Body.String(), `"ok":true`) {
		t.Fatalf("blank token update failed: %s", got.Body.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cloudflare.APIToken != "secret-token" {
		t.Fatalf("blank token overwrote existing token: %q", cfg.Cloudflare.APIToken)
	}
}

func TestYAMLRootCarrierDoesNotChangeNestedAgentCarrier(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := testConfig()
	cfg.Carrier = "telecom"
	cfg.Agent.Carrier = "unicom"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateYAMLField(path, "carrier", "mobile", true); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Carrier != "mobile" || loaded.Agent.Carrier != "unicom" {
		t.Fatalf("carrier values = root %q, agent %q", loaded.Carrier, loaded.Agent.Carrier)
	}
}

func TestAirportProfilesPostPreservesExistingRecordID(t *testing.T) {
	cfg := testConfig()
	cfg.Cloudflare.ZoneID, cfg.Cloudflare.RecordID = "", ""
	cfg.BaseDomain = "example.com"
	cfg.AirportProfiles = []config.AirportProfile{{
		ID: "hk", Name: "香港", Slug: "hk", TargetDomains: []string{"hk-entry.example.com"},
		ProbeSource: "香港网络", Carrier: "auto",
		EntryRecord: config.RegionRecord{RecordID: "entry-record-id", CustomDomain: "entry.example.com"},
	}}
	s, path := newConfigTestServer(t, cfg)
	payload := `{"base_domain":"example.com","airport_profiles":[{"id":"hk","name":"香港新名","slug":"hk","target_domains":["hk-entry.example.com"],"probe_source":"香港网络","carrier":"auto"}]}`
	rr := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/airport-profiles", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		s.handleAPIAirportProfiles(out, req)
		return out
	}()
	if !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("airport update failed: %s", rr.Body.String())
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.AirportProfiles[0].EntryRecord.RecordID; got != "entry-record-id" {
		t.Fatalf("entry record ID = %q, want preserved ID", got)
	}
}
