package main

import (
	"errors"
	"testing"
	"time"

	"dns-latency-router/internal/agent"
	"dns-latency-router/internal/checker"
	"dns-latency-router/internal/cloudflare"
	"dns-latency-router/internal/config"
)

type fakeDNSRecords struct {
	ip               string
	readErr          error
	updates, deletes int
}

func TestAgentScoresUseCurrentControllerWeights(t *testing.T) {
	results := []agent.Result{
		{IP: "1.1.1.1", Latency: 20, Jitter: 10, Attempts: 4, Successes: 4, Score: 1},
		{IP: "8.8.8.8", Latency: 25, Jitter: 0, Attempts: 4, Successes: 4, Score: 999},
	}
	got := checkerResultsFromAgent(results, &config.Config{LatencyWeight: 1, JitterWeight: 2, LossWeight: 4}, time.Now())
	if len(got) != 2 || got[0].IP != "8.8.8.8" || got[0].Score != 25 || got[1].Score != 40 {
		t.Fatalf("controller trusted stale agent scores: %+v", got)
	}
}

func (f *fakeDNSRecords) CurrentIP() (string, error)                   { return f.ip, f.readErr }
func (f *fakeDNSRecords) CurrentIPByName(string) (string, error)       { return f.ip, f.readErr }
func (f *fakeDNSRecords) UpdateRecord(ip string) error                 { f.updates++; f.ip = ip; return nil }
func (f *fakeDNSRecords) UpdateRecordByName(_ string, ip string) error { return f.UpdateRecord(ip) }
func (f *fakeDNSRecords) DeleteRecord() error                          { f.deletes++; f.ip = ""; return nil }
func (f *fakeDNSRecords) DeleteRecordByName(string) error              { return f.DeleteRecord() }

func TestRecordReadFailureNeverTriggersCreation(t *testing.T) {
	client := &fakeDNSRecords{readErr: errors.New("Cloudflare rate limited")}
	result := decideProfileRecord(&config.Config{}, config.AirportProfile{ID: "airport"}, "entry", config.RegionRecord{CustomDomain: "route.example"}, []checker.Result{{IP: "1.1.1.1", Latency: time.Millisecond * 20, Score: 20}}, &switchController{}, time.Now(), client)
	if client.updates != 0 || result.Status != "read_failed" {
		t.Fatalf("temporary API failure became a write: updates=%d status=%s", client.updates, result.Status)
	}
}

func TestBelowThresholdResponseDoesNotDeleteDNS(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		client := &fakeDNSRecords{ip: "1.1.1.1"}
		cfg := &config.Config{PingMinThreshold: time.Millisecond}
		results := []checker.Result{{IP: "1.1.1.1", Latency: time.Microsecond * 500, Score: 0.5, Attempts: 1, Successes: 1}}
		if legacy {
			decideLegacyRoute(cfg, client, &switchController{}, []string{"1.1.1.1"}, results)
		} else {
			decideProfileRecord(cfg, config.AirportProfile{ID: "airport"}, "entry", config.RegionRecord{CustomDomain: "route.example"}, results, &switchController{}, time.Now(), client)
		}
		if client.deletes != 0 || client.updates != 0 {
			t.Fatalf("legacy=%t: successful but excluded measurement caused DNS write", legacy)
		}
	}
}

func TestOutageRequiresIndependentFailedRounds(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		client := &fakeDNSRecords{ip: "1.1.1.1"}
		sc := &switchController{}
		cfg := &config.Config{FailureConfirmCycles: 3}
		now := time.Now()
		decide := func(stamp time.Time) {
			results := []checker.Result{{IP: "1.1.1.1", Err: errors.New("timeout"), FinishedAt: stamp}}
			if legacy {
				decideLegacyRoute(cfg, client, sc, []string{"1.1.1.1"}, results)
			} else {
				decideProfileRecord(cfg, config.AirportProfile{ID: "airport"}, "entry", config.RegionRecord{CustomDomain: "route.example"}, results, sc, stamp, client)
			}
		}
		decide(now)
		for i := 0; i < 5; i++ {
			decide(now)
		}
		decide(now.Add(time.Minute))
		if client.deletes != 0 {
			t.Fatalf("legacy=%v: removed record without 3 independent failures", legacy)
		}
		decide(now.Add(2 * time.Minute))
		if client.deletes != 1 {
			t.Fatalf("legacy=%v: confirmed outage should execute deletion once", legacy)
		}
	}
}

func TestHealthyObservationResetsFailureConfirmation(t *testing.T) {
	sc := &switchController{}
	client := &fakeDNSRecords{ip: "1.1.1.1"}
	cfg := &config.Config{FailureConfirmCycles: 3}
	now := time.Now()
	failed := func(stamp time.Time) []checker.Result {
		return []checker.Result{{IP: "1.1.1.1", Err: errors.New("timeout"), FinishedAt: stamp}}
	}
	decideLegacyRoute(cfg, client, sc, nil, failed(now))
	decideLegacyRoute(cfg, client, sc, nil, []checker.Result{{IP: "1.1.1.1", Latency: time.Millisecond, Score: 1}})
	decideLegacyRoute(cfg, client, sc, nil, failed(now.Add(time.Minute)))
	decideLegacyRoute(cfg, client, sc, nil, failed(now.Add(2*time.Minute)))
	if client.deletes != 0 {
		t.Fatal("success did not interrupt consecutive failures")
	}
}

func TestLegacyDeletedRecordCanRecoverByName(t *testing.T) {
	client := &fakeDNSRecords{readErr: cloudflare.ErrRecordNotFound}
	result := decideLegacyRoute(&config.Config{CustomDomain: "route.example"}, client, &switchController{}, []string{"1.1.1.1"}, []checker.Result{{IP: "1.1.1.1", Latency: 20 * time.Millisecond, Score: 20}})
	if client.updates != 1 || result.ActiveIP != "1.1.1.1" {
		t.Fatalf("deleted record did not recover: %+v", result)
	}
}

func TestMissingCurrentMeasurementCannotBypassImprovementThreshold(t *testing.T) {
	client := &fakeDNSRecords{ip: "8.8.8.8"}
	sc := &switchController{}
	sc.route("airport|entry").candidateIP = "1.1.1.1"
	sc.route("airport|entry").candidateSince = time.Now().Add(-time.Hour)
	result := decideProfileRecord(&config.Config{SwitchImprovement: 15}, config.AirportProfile{ID: "airport"}, "entry", config.RegionRecord{CustomDomain: "route.example"}, []checker.Result{{IP: "1.1.1.1", Latency: 20 * time.Millisecond, Score: 20}}, sc, time.Now(), client)
	if client.updates != 0 || result.Status != "keeping" || result.Latency != 0 {
		t.Fatalf("unmeasured current route was replaced or shown with candidate latency: %+v", result)
	}
}

func TestCurrentRouteAddedToLocalProbeSetOnlyOnce(t *testing.T) {
	ips := includeCurrentProbeIP([]string{"1.1.1.1"}, "8.8.8.8")
	ips = includeCurrentProbeIP(ips, "8.8.8.8")
	ips = includeCurrentProbeIP(ips, "127.0.0.1")
	if len(ips) != 2 || ips[1] != "8.8.8.8" {
		t.Fatalf("unexpected probe set: %v", ips)
	}
}
