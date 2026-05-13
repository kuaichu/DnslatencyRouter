package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"dns-latency-router/internal/config"
)

//go:embed dashboard.html
var templateFS embed.FS

// Status holds the current state exposed via API/SSE.
type Status struct {
	TargetDomain     string  `json:"targetDomain"`
	CustomDomain     string  `json:"customDomain"`
	CurrentIP        string  `json:"currentIP"`
	Latency          float64 `json:"latency"`          // ms, 0 = unknown
	LastCheck        string  `json:"lastCheck"`        // RFC3339
	NextCheck        string  `json:"nextCheck"`        // RFC3339
	IsRunning        bool    `json:"isRunning"`
	DiscoveredCount  int     `json:"discoveredCount"`
	CheckIntervalSec int     `json:"checkIntervalSec"`
}

// CheckRecord is one completed check cycle.
type CheckRecord struct {
	Time    time.Time `json:"time"`
	IP      string    `json:"ip"`
	Latency float64   `json:"latency"`          // ms, 0 if failed
	Success bool      `json:"success"`
	Error   string    `json:"error,omitempty"`
}

// Server is the web dashboard HTTP server.
type Server struct {
	port       int
	status     atomic.Value
	history    []CheckRecord
	historyMu  sync.Mutex
	sseClients map[string]chan sseEvent
	sseMu      sync.Mutex
	sseNextID  int64
	httpServer *http.Server
	readyCh    chan struct{}
	cfgPath    string // for persisting config changes
	onConfig   func(targetDomain, customDomain string) // callback to notify main loop
}

type sseEvent struct {
	Event string
	Data  string
}

// New creates a web server.
// cfgPath is the path to config.yaml for persisting changes.
// onConfig is called when the user updates target_domain or custom_domain via the web UI.
func New(port int, cfgPath string, onConfig func(targetDomain, customDomain string)) *Server {
	s := &Server{
		port:       port,
		sseClients: make(map[string]chan sseEvent),
		readyCh:    make(chan struct{}),
		cfgPath:    cfgPath,
		onConfig:   onConfig,
	}
	s.status.Store(&Status{CheckIntervalSec: 300})
	return s
}

// Start begins the HTTP server in a goroutine. Returns immediately.
func (s *Server) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/history", s.handleAPIHistory)
	mux.HandleFunc("/api/config", s.handleAPIConfig)
	mux.HandleFunc("/api/events", s.handleSSE)

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	go func() {
		log.Printf("[web] dashboard at http://0.0.0.0:%d", s.port)
		close(s.readyCh)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[web] server error: %v", err)
		}
	}()
}

// WaitReady blocks until the server is accepting connections.
func (s *Server) WaitReady() {
	<-s.readyCh
	time.Sleep(50 * time.Millisecond) // tiny extra margin for listener
}

// Stop shuts down the HTTP server.
func (s *Server) Stop() {
	if s.httpServer != nil {
		s.httpServer.Close()
	}
}

// --- Status updates (called by main loop) ---

// GetStatus returns the current status copy.
func (s *Server) GetStatus() *Status {
	return s.status.Load().(*Status)
}

// UpdateStatus sets the current status and broadcasts via SSE.
func (s *Server) UpdateStatus(st *Status) {
	s.status.Store(st)
	s.broadcast("status", mustJSON(st))
}

// AddHistory appends a check record and broadcasts the full history.
func (s *Server) AddHistory(rec CheckRecord) {
	s.historyMu.Lock()
	s.history = append(s.history, rec)
	if len(s.history) > 100 {
		s.history = s.history[len(s.history)-100:]
	}
	hist := make([]CheckRecord, len(s.history))
	copy(hist, s.history)
	s.historyMu.Unlock()
	s.broadcast("history", mustJSON(hist))
}

// AddLog broadcasts a log line via SSE.
func (s *Server) AddLog(line string) {
	s.broadcast("log", line)
}

// logWriter is an io.Writer that feeds logs to both SSE clients and stderr.
// Use with log.SetOutput() to capture all log output.
type logWriter struct {
	s *Server
}

func (w *logWriter) Write(p []byte) (int, error) {
	line := string(p)
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	w.s.AddLog(line)
	os.Stderr.Write(p)
	return len(p), nil
}

// LogWriter returns an io.Writer for use with log.SetOutput().
func (s *Server) LogWriter() io.Writer {
	return &logWriter{s: s}
}

// --- SSE ---

func (s *Server) subscribe() (int64, <-chan sseEvent) {
	id := atomic.AddInt64(&s.sseNextID, 1)
	ch := make(chan sseEvent, 64)
	s.sseMu.Lock()
	s.sseClients[fmt.Sprintf("%d", id)] = ch
	s.sseMu.Unlock()
	return id, ch
}

func (s *Server) unsubscribe(id int64) {
	key := fmt.Sprintf("%d", id)
	s.sseMu.Lock()
	if ch, ok := s.sseClients[key]; ok {
		close(ch)
		delete(s.sseClients, key)
	}
	s.sseMu.Unlock()
}

func (s *Server) broadcast(event, data string) {
	e := sseEvent{Event: event, Data: data}
	s.sseMu.Lock()
	for id, ch := range s.sseClients {
		select {
		case ch <- e:
		default:
			close(ch)
			delete(s.sseClients, id)
		}
	}
	s.sseMu.Unlock()
}

// --- HTTP handlers ---

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tmpl, err := template.ParseFS(templateFS, "dashboard.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, nil)
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	st := s.status.Load().(*Status)
	writeJSON(w, st)
}

func (s *Server) handleAPIHistory(w http.ResponseWriter, r *http.Request) {
	s.historyMu.Lock()
	hist := make([]CheckRecord, len(s.history))
	copy(hist, s.history)
	s.historyMu.Unlock()
	writeJSON(w, hist)
}

func (s *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		st := s.status.Load().(*Status)
		writeJSON(w, map[string]string{
			"target_domain": st.TargetDomain,
			"custom_domain": st.CustomDomain,
		})
		return
	}

	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		TargetDomain *string `json:"target_domain"`
		CustomDomain *string `json:"custom_domain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	// Build what changed
	var newTarget, newCustom string
	st := s.status.Load().(*Status)

	if body.TargetDomain != nil && *body.TargetDomain != "" && *body.TargetDomain != st.TargetDomain {
		newTarget = *body.TargetDomain
	}
	if body.CustomDomain != nil && *body.CustomDomain != "" && *body.CustomDomain != st.CustomDomain {
		newCustom = *body.CustomDomain
	}

	if newTarget == "" && newCustom == "" {
		writeJSON(w, map[string]string{"error": "no changes or empty values"})
		return
	}

	// Persist to config.yaml (line-based replacement preserves comments)
	if newTarget != "" {
		if err := config.UpdateYAMLField(s.cfgPath, "target_domain", newTarget); err != nil {
			writeJSON(w, map[string]string{"error": "persist target_domain: " + err.Error()})
			return
		}
	}
	if newCustom != "" {
		if err := config.UpdateYAMLField(s.cfgPath, "custom_domain", newCustom); err != nil {
			writeJSON(w, map[string]string{"error": "persist custom_domain: " + err.Error()})
			return
		}
	}

	// Notify main loop of the change
	if s.onConfig != nil {
		finalTarget := newTarget
		if finalTarget == "" {
			finalTarget = st.TargetDomain
		}
		finalCustom := newCustom
		if finalCustom == "" {
			finalCustom = st.CustomDomain
		}
		s.onConfig(finalTarget, finalCustom)
	}

	// Update status with new domains
	st.TargetDomain = newTarget
	st.CustomDomain = newCustom
	s.UpdateStatus(st)

	log.Printf("[config] updated: target_domain=%q custom_domain=%q", newTarget, newCustom)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send initial status
	st := s.status.Load().(*Status)
	fmt.Fprintf(w, "event: status\ndata: %s\n\n", mustJSON(st))

	// Send initial history
	s.historyMu.Lock()
	hist := make([]CheckRecord, len(s.history))
	copy(hist, s.history)
	s.historyMu.Unlock()
	if len(hist) > 0 {
		fmt.Fprintf(w, "event: history\ndata: %s\n\n", mustJSON(hist))
	}
	flusher.Flush()

	id, ch := s.subscribe()
	defer s.unsubscribe(id)

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Event, e.Data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}
