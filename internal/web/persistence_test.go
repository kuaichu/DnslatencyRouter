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
