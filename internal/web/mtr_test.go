package web

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dns-latency-router/internal/mtr"
)

func TestMTRAgentAPIRejectsWrongIdentityAndReadDoesNotCreateJobs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("target_domain: example.com\ncloudflare:\n  api_token: test\n  zone_id: zone\n  record_id: record\nagent:\n  token: test-agent-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(0, path, nil)
	defer s.Stop()
	read := httptest.NewRecorder()
	s.handleAPIMTR(read, httptest.NewRequest("GET", "/api/mtr", nil))
	if read.Body.String() != "[]\n" || len(s.traces.state.Results) != 0 {
		t.Fatal("read created a trace")
	}
	selection := traceSelection{AgentID: "agent-a", ProfileID: "p", IP: "1.1.1.1", Port: 443}
	if err := s.traces.syncTargets(map[string]traceSelection{"p|entry": selection}, map[string]bool{"p": true}); err != nil {
		t.Fatal(err)
	}
	job := s.traces.jobs("agent-a")[0]
	result := mtr.Result{Request: job, Status: "completed", Output: "report", FinishedAt: time.Now()}
	data, _ := json.Marshal(result)
	for _, tc := range []struct {
		source, token string
		code          int
	}{{"agent-a", "", 401}, {"agent-b", "test-agent-token", 400}, {"controller", "test-agent-token", 400}, {"agent-a", "test-agent-token", 200}} {
		r := httptest.NewRequest("POST", "/api/agent/mtr", bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("X-Agent-ID", tc.source)
		w := httptest.NewRecorder()
		s.handleAPIAgentMTR(w, r)
		if w.Code != tc.code {
			t.Fatalf("source=%s got %d want %d: %s", tc.source, w.Code, tc.code, w.Body.String())
		}
	}
	if len(s.traces.jobs("agent-a")) != 0 {
		t.Fatal("completed task still scheduled")
	}
}

func TestMTRSelectionCacheChangesOnlyWithSelectedTarget(t *testing.T) {
	store, err := openRuntimeStore(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	_, err = store.db.Exec(`CREATE TABLE route_trace_state (id INTEGER PRIMARY KEY,state_json TEXT NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	m := &traceManager{store: store, state: traceState{Selections: map[string]traceSelection{}, Results: map[string]mtr.Result{}}}
	profiles := map[string]bool{"p": true}
	selection := traceSelection{AgentID: "a", ProfileID: "p", IP: "1.1.1.1", Port: 443}
	updates := map[string]traceSelection{"p|entry": selection, "p|sg": selection}
	if err = m.syncTargets(updates, profiles); err != nil {
		t.Fatal(err)
	}
	jobs := m.jobs("a")
	if len(jobs) != 1 {
		t.Fatalf("same IP in two regions scheduled %d traces", len(jobs))
	}
	first := jobs[0]
	if err = m.accept(mtr.Result{Request: first, Status: "completed", Output: "test", FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = m.syncTargets(updates, profiles); err != nil {
		t.Fatal(err)
	}
	if len(m.jobs("a")) != 0 {
		t.Fatal("unchanged selection retraced")
	}
	changed := selection
	changed.IP = "8.8.8.8"
	updates["p|entry"] = changed
	updates["p|sg"] = changed
	if err = m.syncTargets(updates, profiles); err != nil {
		t.Fatal(err)
	}
	jobs = m.jobs("a")
	if len(jobs) != 1 || jobs[0].IP != changed.IP || jobs[0].ID == first.ID {
		t.Fatal("changed selection did not create new job")
	}
	if err = m.accept(mtr.Result{Request: first, Status: "completed", FinishedAt: time.Now()}); err == nil {
		t.Fatal("late obsolete report accepted")
	}
	updates["p|entry"] = selection
	updates["p|sg"] = selection
	if err = m.syncTargets(updates, profiles); err != nil {
		t.Fatal(err)
	}
	jobs = m.jobs("a")
	if len(jobs) != 1 || jobs[0].ID == first.ID {
		t.Fatal("A -> B -> A failed to retrace")
	}
	if len(m.jobs("other")) != 0 {
		t.Fatal("job leaked to wrong source")
	}
}

func TestMTRStateLoadsAfterControllerRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	s := New(0, cfg, nil)
	if s.traces == nil {
		t.Fatal("trace cache not initialized")
	}
	selection := traceSelection{AgentID: "remote", ProfileID: "p", IP: "1.1.1.1", Port: 443}
	err := s.traces.syncTargets(map[string]traceSelection{"p|entry": selection}, map[string]bool{"p": true})
	if err != nil {
		t.Fatal(err)
	}
	request := s.traces.jobs("remote")[0]
	if err = s.traces.accept(mtr.Result{Request: request, Status: "failed", Error: "no reply", FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	s = New(0, cfg, nil)
	defer s.Stop()
	if err = s.traces.syncTargets(map[string]traceSelection{"p|entry": selection}, map[string]bool{"p": true}); err != nil {
		t.Fatal(err)
	}
	if len(s.traces.jobs("remote")) != 0 {
		t.Fatal("restart or failure triggered repeated execution")
	}
}
