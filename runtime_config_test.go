package main

import (
	"testing"
	"time"

	"dns-latency-router/internal/config"
	"dns-latency-router/internal/web"
)

func TestRuntimeConfigPreservesAirportSummaryAndLastProbe(t *testing.T) {
	st := &web.Status{LastCheck: "last-probe", Profiles: []web.ProfileStatus{{
		ID: "airport", TargetDomain: "entry.example", ProbeSource: "airport-source", Carrier: "unicom",
		Regions: []web.RegionStatus{{CustomDomain: "route.example", CurrentIP: "1.1.1.1", Latency: 25}},
	}}}
	cfg := &config.Config{TargetDomain: "legacy.example", ProbeSource: "legacy-source", PingAttempts: 8,
		AirportProfiles: []config.AirportProfile{{ID: "airport"}}}
	now := time.Now()
	next := now.Add(time.Minute)
	applyRuntimeConfigStatus(st, cfg, &next, now)
	if st.TargetDomain != "entry.example" || st.ProbeSource != "airport-source" || st.CurrentIP != "1.1.1.1" || st.Latency != 25 || st.LastCheck != "last-probe" || st.PingAttempts != 8 {
		t.Fatalf("config update overwrote the airport snapshot: %+v", st)
	}
}

func TestQueueConfigUpdateRetainsLatestCompleteConfig(t *testing.T) {
	updates := make(chan *config.Config, 1)
	first := &config.Config{TargetDomain: "old.example", PingAttempts: 2}
	latest := &config.Config{TargetDomain: "new.example", PingAttempts: 8}
	queueConfigUpdate(updates, first)
	queueConfigUpdate(updates, latest)
	if got := <-updates; got != latest || first.PingAttempts != 2 {
		t.Fatal("pending update must replace the whole snapshot without mutating an active cycle")
	}
}

func TestMissingAgentRouteCannotCarryStabilityAcrossGap(t *testing.T) {
	sc := &switchController{}
	for _, key := range []string{"airport|carrier-unicom", "airport|carrier-mobile", "other|entry"} {
		sc.route(key).candidateIP = "1.1.1.1"
		sc.route(key).candidateSince = time.Now().Add(-time.Hour)
	}
	sc.resetUnobservedRoutes("airport", []web.RegionStatus{{Region: "carrier-mobile", Status: "stabilizing", CandidateCount: 1}})
	if sc.route("airport|carrier-unicom").candidateIP != "" {
		t.Fatal("missing agent route retained its previous observation")
	}
	if sc.route("airport|carrier-mobile").candidateIP == "" || sc.route("other|entry").candidateIP == "" {
		t.Fatal("valid or unrelated observations were cleared")
	}
}

func TestFailedProfileDiscoveryClearsOnlyItsStabilityWindow(t *testing.T) {
	sc := &switchController{}
	old := time.Now().Add(-time.Hour)
	for _, key := range []string{"airport|entry", "airport|carrier-unicom", "other|entry"} {
		sc.route(key).candidateIP = "1.1.1.1"
		sc.route(key).candidateSince = old
		sc.route(key).outageActive = true
	}
	// No targets: discovery fails without external network requests.
	runAirportProfileOnce(&config.Config{}, config.AirportProfile{ID: "airport"}, nil, sc)
	for _, key := range []string{"airport|entry", "airport|carrier-unicom"} {
		st := sc.route(key)
		if st.candidateIP != "" || !st.candidateSince.IsZero() || !st.outageActive {
			t.Fatalf("failed discovery must reset observations, preserving alert state: %+v", st)
		}
	}
	if !sc.route("other|entry").candidateSince.Equal(old) {
		t.Fatal("unrelated profile observation was reset")
	}
}
