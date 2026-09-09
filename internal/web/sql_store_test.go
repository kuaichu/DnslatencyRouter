package web

import (
	"path/filepath"
	"testing"
	"time"

	"dns-latency-router/internal/agent"
)

func TestRuntimeStoreAppendLogCompactsByCount(t *testing.T) {
	store, err := openRuntimeStore(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()

	now := time.Now()
	for i := 0; i < maxLogEntries+17; i++ {
		if err := store.appendLog(LogEntry{Time: now.Add(time.Duration(i) * time.Millisecond), Line: "line"}); err != nil {
			t.Fatalf("append log %d: %v", i, err)
		}
	}
	logs, err := store.loadLogs(maxLogEntries)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(logs), maxLogEntries; got != want {
		t.Fatalf("log count = %d, want %d", got, want)
	}
	if !logs[0].Time.Equal(now.Add(17 * time.Millisecond)) {
		t.Fatalf("oldest retained log = %s, want %s", logs[0].Time, now.Add(17*time.Millisecond))
	}
}

func TestRuntimeStoreAppendLogPrunesRetention(t *testing.T) {
	store, err := openRuntimeStore(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()

	old := time.Now().Add(-retentionWindow - time.Minute)
	if err := store.replaceLogs([]LogEntry{{Time: old, Line: "old"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.appendLog(LogEntry{Time: time.Now(), Line: "new"}); err != nil {
		t.Fatal(err)
	}
	logs, err := store.loadLogs(maxLogEntries)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Line != "new" {
		t.Fatalf("retained logs = %#v, want only new entry", logs)
	}
}

func TestRuntimeStoreAppendSamplesAndPrune(t *testing.T) {
	store, err := openRuntimeStore(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()

	if err := store.appendSamples([]IPSample{
		{Time: time.Now().Add(-retentionWindow - time.Minute), IP: "34.96.159.37"},
		{Time: time.Now(), IP: "34.96.143.153"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.prune(); err != nil {
		t.Fatal(err)
	}
	samples, err := store.loadSamples(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].IP != "34.96.143.153" {
		t.Fatalf("retained samples = %#v, want only new sample", samples)
	}
}

func TestRuntimeStoreAgentReportTTLUsesReceiptTime(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		finishedAt time.Time
		receivedAt time.Time
		want       int
	}{
		{name: "old receipt overrides future finish", finishedAt: now.Add(time.Hour), receivedAt: now.Add(-time.Hour), want: 0},
		{name: "current receipt overrides future finish", finishedAt: now.Add(time.Hour), receivedAt: now.Add(-time.Minute), want: 1},
		{name: "legacy future finish rejected", finishedAt: now.Add(time.Hour), want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := openRuntimeStore(filepath.Join(t.TempDir(), "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.close()
			if err := store.upsertAgentReport(agent.Report{
				AgentID:    tt.name,
				FinishedAt: tt.finishedAt,
				ReceivedAt: tt.receivedAt,
			}); err != nil {
				t.Fatal(err)
			}
			reports, err := store.loadAgentReports(15 * time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(reports); got != tt.want {
				t.Fatalf("reports = %d, want %d", got, tt.want)
			}
		})
	}
}
