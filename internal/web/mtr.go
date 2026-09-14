package web

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"dns-latency-router/internal/checker"
	"dns-latency-router/internal/config"
	"dns-latency-router/internal/mtr"
)

type traceSelection struct {
	ProfileID string `json:"profileId"`
	AgentID   string `json:"agentId"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
}

const traceFormatVersion = "route-v3"

func (t traceSelection) key() string {
	return traceFormatVersion + "|" + t.AgentID + "|" + t.IP + fmt.Sprintf("|%d", t.Port)
}

type traceState struct {
	Selections map[string]traceSelection `json:"selections"`
	Results    map[string]mtr.Result     `json:"results"`
}

type traceManager struct {
	mu    sync.Mutex
	state traceState
	store *runtimeStore
	local *mtr.Worker
}

func (s *Server) initMTR() {
	if s.store == nil {
		return
	}
	t := &traceManager{store: s.store, state: traceState{Selections: map[string]traceSelection{}, Results: map[string]mtr.Result{}}}
	if _, err := s.store.db.Exec(`CREATE TABLE IF NOT EXISTS route_trace_state (id INTEGER PRIMARY KEY, state_json TEXT NOT NULL)`); err != nil {
		log.Printf("[mtr] storage unavailable: %v", err)
		return
	}
	var data string
	err := s.store.db.QueryRow(`SELECT state_json FROM route_trace_state WHERE id=1`).Scan(&data)
	if err == nil {
		if err := json.Unmarshal([]byte(data), &t.state); err != nil {
			log.Printf("[mtr] refusing to discard unreadable trace cache: %v", err)
			return
		}
	} else if err != sql.ErrNoRows {
		log.Printf("[mtr] cannot read saved selections: %v", err)
		return
	}
	if t.state.Selections == nil {
		t.state.Selections = map[string]traceSelection{}
	}
	if t.state.Results == nil {
		t.state.Results = map[string]mtr.Result{}
	}
	s.traces = t
	base := filepath.Dir(s.cfgPath)
	worker, err := mtr.NewWorker(filepath.Join(base, "data", "mtr-local.json"), func(ctx context.Context, r mtr.Request) mtr.Result { return mtr.Run(ctx, base, r) }, func(r mtr.Result) {
		if err := t.accept(r); err != nil {
			log.Printf("[mtr] save local result: %v", err)
		}
	})
	if err != nil {
		log.Printf("[mtr] local worker unavailable: %v", err)
		return
	}
	t.local = worker
}

func (t *traceManager) saveLocked() error {
	data, err := json.Marshal(t.state)
	if err != nil {
		return err
	}
	_, err = t.store.db.Exec(`INSERT INTO route_trace_state(id,state_json) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET state_json=excluded.state_json`, string(data))
	return err
}

func (s *Server) UpdateTraceTargets(st *Status, cfg *config.Config) {
	if s.traces == nil || cfg == nil {
		return
	}
	updates := map[string]traceSelection{}
	validProfiles := map[string]bool{}
	if !cfg.HasAirportProfiles() {
		validProfiles[""] = true
		if st.LastCheck != "" {
			updates["|entry"] = traceSelection{AgentID: "controller", IP: st.CurrentIP, Port: cfg.PingPort}
		}
	} else {
		for _, p := range cfg.AirportProfiles {
			validProfiles[p.ID] = true
		}
		for _, p := range st.Profiles {
			for _, r := range p.Regions {
				if r.Status == "read_failed" || r.Status == "delete_failed" {
					continue
				}
				source := r.AgentID
				if source == "" {
					// A carrier route must be traced by the agent that supplied it.
					if strings.HasPrefix(r.Region, "carrier-") && !cfg.RunsLocalProbes() {
						continue
					}
					source = "controller"
				}
				if r.CurrentIP == "" && r.Status != "deleted" {
					continue
				}
				updates[p.ID+"|"+r.Region] = traceSelection{ProfileID: p.ID, AgentID: source, IP: r.CurrentIP, Port: cfg.PingPort}
			}
		}
	}
	if err := s.traces.syncTargets(updates, validProfiles); err != nil {
		log.Printf("[mtr] cannot persist selections: %v", err)
		return
	}
	if s.traces.local != nil {
		for _, r := range s.traces.local.Sync(s.traces.jobs("controller")) {
			_ = s.traces.accept(r)
		}
	}
}

func (t *traceManager) syncTargets(updates map[string]traceSelection, validProfiles map[string]bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := traceState{Selections: map[string]traceSelection{}, Results: map[string]mtr.Result{}}
	for key, v := range t.state.Selections {
		if validProfiles[v.ProfileID] {
			next.Selections[key] = v
		}
	}
	for key, v := range updates {
		if checker.IsUsableCandidateIP(v.IP) {
			next.Selections[key] = v
		} else {
			delete(next.Selections, key)
		}
	}
	keys := make([]string, 0, len(next.Selections))
	for key := range next.Selections {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := next.Selections[key]
		target := v.key()
		if _, ok := next.Results[target]; ok {
			continue
		}
		if result, ok := t.state.Results[target]; ok {
			next.Results[target] = result
			continue
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		next.Results[target] = mtr.Result{Request: mtr.Request{ID: hex.EncodeToString(id[:]), AgentID: v.AgentID, ProfileID: v.ProfileID, IP: v.IP, Port: v.Port}, Status: "pending"}
	}
	previous := t.state
	t.state = next
	if err := t.saveLocked(); err != nil {
		t.state = previous
		return err
	}
	return nil
}

func (t *traceManager) jobs(source string) []mtr.Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	jobs := []mtr.Request{}
	for _, r := range t.state.Results {
		if r.AgentID == source && (r.Status == "pending" || r.Status == "running") {
			jobs = append(jobs, r.Request)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs
}

func (t *traceManager) accept(result mtr.Result) error {
	if err := result.Request.Validate(); err != nil {
		return err
	}
	if result.Status != "completed" && result.Status != "failed" {
		return fmt.Errorf("invalid trace status")
	}
	if len(result.Output) > mtr.MaxOutput || len(result.Error) > 1000 || result.FinishedAt.IsZero() {
		return fmt.Errorf("invalid trace output")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := (traceSelection{AgentID: result.AgentID, IP: result.IP, Port: result.Port}).key()
	old, ok := t.state.Results[key]
	if !ok || old.Request != result.Request {
		return fmt.Errorf("trace selection changed")
	}
	if old.Status == "completed" || old.Status == "failed" {
		return nil
	}
	t.state.Results[key] = result
	if err := t.saveLocked(); err != nil {
		t.state.Results[key] = old
		return err
	}
	return nil
}

func (s *Server) handleAPIMTR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	rows := []mtr.Result{}
	if s.traces != nil {
		t := s.traces
		t.mu.Lock()
		seen := map[string]bool{}
		for _, selection := range t.state.Selections {
			if r.URL.Query().Get("agent_id") != "" && selection.AgentID != r.URL.Query().Get("agent_id") {
				continue
			}
			if r.URL.Query().Get("profile_id") != "" && selection.ProfileID != r.URL.Query().Get("profile_id") {
				continue
			}
			key := selection.key()
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, t.state.Results[key])
		}
		t.mu.Unlock()
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, rows)
}

func (s *Server) handleAPIAgentMTR(w http.ResponseWriter, r *http.Request) {
	if !s.agentAuthorized(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	id := strings.TrimSpace(r.Header.Get("X-Agent-ID"))
	if id == "" || strings.EqualFold(id, "controller") {
		http.Error(w, "agent ID required", 400)
		return
	}
	if s.traces == nil {
		http.Error(w, "MTR storage unavailable", 503)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.traces.jobs(id))
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, mtr.MaxOutput+8192)
		var result mtr.Result
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&result); err != nil {
			http.Error(w, "invalid trace JSON", 400)
			return
		}
		var extra any
		if dec.Decode(&extra) != io.EOF || result.AgentID != id {
			http.Error(w, "invalid trace identity", 400)
			return
		}
		if err := s.traces.accept(result); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		log.Printf("[mtr] %s trace of %s: %s", id, result.IP, result.Status)
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleAPINextTraceDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	platform := strings.TrimPrefix(r.URL.Path, "/api/agent/nexttrace/")
	name := mtr.BinaryName(platform)
	switch platform {
	case "LICENSE":
		name = "LICENSE"
	case "source":
		name = "source-v1.7.3.tar.gz"
	}
	if name == "" {
		http.NotFound(w, r)
		return
	}
	for _, base := range []string{filepath.Dir(s.cfgPath), "."} {
		path := filepath.Join(base, "tools", "nexttrace", name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			http.ServeFile(w, r, path)
			return
		}
	}
	http.NotFound(w, r)
}
