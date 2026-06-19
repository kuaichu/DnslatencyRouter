package web

import (
	"testing"
	"time"
)

func TestUpdateIPLifecyclesKeepsOriginalFirstSeen(t *testing.T) {
	first := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	later := first.Add(2 * time.Hour)
	s := &Server{ipLifecycles: make(map[string]IPLifecycle)}

	s.updateIPLifecycles([]IPSample{{
		Time:      first,
		AgentID:   "agent-a",
		ProfileID: "svc",
		Region:    "carrier-unicom",
		IP:        "34.96.159.37",
	}})
	s.updateIPLifecycles([]IPSample{{
		Time:      later,
		AgentID:   "agent-a",
		ProfileID: "svc",
		Region:    "carrier-unicom",
		IP:        "34.96.159.37",
	}})

	rec := s.ipLifecycleSnapshot()[sampleKey("agent-a", "svc", "carrier-unicom", "34.96.159.37")]
	if !rec.FirstSeen.Equal(first) {
		t.Fatalf("first seen moved: got %s, want %s", rec.FirstSeen, first)
	}
	if !rec.LastSeen.Equal(later) {
		t.Fatalf("last seen not updated: got %s, want %s", rec.LastSeen, later)
	}
}

func TestPruneSamplesKeepsSevenDayWindowWithoutCountCap(t *testing.T) {
	now := time.Now()
	samples := make([]IPSample, 2500)
	for i := range samples {
		samples[i] = IPSample{
			Time:    now.Add(-time.Duration(i) * time.Second),
			IP:      "34.96.159.37",
			Latency: 30,
			Success: true,
		}
	}

	pruned := pruneSamples(samples)
	if len(pruned) != len(samples) {
		t.Fatalf("samples were capped by count: got %d, want %d", len(pruned), len(samples))
	}
}
