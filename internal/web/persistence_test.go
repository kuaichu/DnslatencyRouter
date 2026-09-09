package web

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistenceLogFailureDoesNotDeadlock(t *testing.T) {
	if os.Getenv("DLR_PERSISTENCE_LOG_HELPER") == "1" {
		store, err := openRuntimeStore(filepath.Join(t.TempDir(), "runtime.db"))
		if err != nil {
			t.Fatal(err)
		}
		store.close()
		s := &Server{store: store, sseClients: make(map[string]chan sseEvent)}
		log.SetOutput(s.LogWriter())
		s.AddLog("exercise failed runtime store")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPersistenceLogFailureDoesNotDeadlock$", "-test.v")
	cmd.Env = append(os.Environ(), "DLR_PERSISTENCE_LOG_HELPER=1")
	if err := cmd.Run(); err != nil {
		t.Fatalf("persistence failure helper failed or deadlocked: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("persistence failure helper timed out: %v", ctx.Err())
	}
}

func TestJSONFallbackPersistsRetentionPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime-logs.jsonl")
	old := time.Now().Add(-retentionWindow - time.Hour)
	current := time.Now()
	if err := rewriteJSONLines(path, []LogEntry{{Time: old, Line: "old"}}); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		logsPath: path,
		logBuf:   pruneLogEntries([]LogEntry{{Time: old, Line: "old"}, {Time: current, Line: "current"}}),
	}
	s.persistLogEntry(LogEntry{Time: current, Line: "current"})
	logs := readJSONLines[LogEntry](path)
	if len(logs) != 1 || logs[0].Line != "current" {
		t.Fatalf("fallback logs = %#v, want only current entry", logs)
	}
}

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

func TestPruneSamplesKeepsThirtyDayWindowWithoutCountCap(t *testing.T) {
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

func TestPruneSamplesDropsOlderThanThirtyDays(t *testing.T) {
	now := time.Now()
	samples := []IPSample{
		{Time: now.Add(-29 * 24 * time.Hour), IP: "34.96.159.37", Latency: 30, Success: true},
		{Time: now.Add(-31 * 24 * time.Hour), IP: "34.96.143.153", Latency: 40, Success: true},
	}

	pruned := pruneSamples(samples)
	if len(pruned) != 1 || pruned[0].IP != "34.96.159.37" {
		t.Fatalf("unexpected pruned samples: %#v", pruned)
	}
}
