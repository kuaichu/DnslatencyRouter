package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dns-latency-router/internal/config"
)

//go:embed dashboard_v2.html
var templateFS embed.FS

// Status holds the current state exposed via API/SSE.
type Status struct {
	TargetDomain     string  `json:"targetDomain"`
	CustomDomain     string  `json:"customDomain"`
	ProbeSource      string  `json:"probeSource"`
	CurrentIP        string  `json:"currentIP"`
	Latency          float64 `json:"latency"`          // ms, 0 = unknown
	LastCheck        string  `json:"lastCheck"`        // RFC3339
	NextCheck        string  `json:"nextCheck"`        // RFC3339
	IsRunning        bool    `json:"isRunning"`
	DiscoveredCount  int     `json:"discoveredCount"`
	CheckIntervalSec int     `json:"checkIntervalSec"`
	PingMode         string  `json:"pingMode"`
	PingPort         int     `json:"pingPort"`
	PingAttempts     int     `json:"pingAttempts"`
	LatencyWeight    float64 `json:"latencyWeight"`
	JitterWeight     float64 `json:"jitterWeight"`
	LossWeight       float64 `json:"lossWeight"`
	SwitchImprovement float64 `json:"switchImprovement"`
	SwitchStableSec  int     `json:"switchStableSec"`
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
	samples    []IPSample
	samplesMu  sync.Mutex
	sseClients map[string]chan sseEvent
	sseMu      sync.Mutex
	sseNextID  int64
	httpServer *http.Server
	readyCh    chan struct{}
	cfgPath    string // for persisting config changes
	onConfig   func(targetDomain, customDomain, probeSource, pingMode string, pingPort, checkInterval, pingAttempts, switchStableSec int, latencyWeight, jitterWeight, lossWeight, switchImprovement float64) // callback to notify main loop
	triggerCh  chan<- struct{} // signal main loop to run a check immediately
	logBuf     []LogEntry
	logBufMu   sync.Mutex
	logsPath   string
	historyPath string
	samplesPath string
	activeIPs   map[string]bool
	activeIPsMu sync.RWMutex
	geoCache   map[string]GeoInfo
	geoPending map[string]bool
	geoMu      sync.RWMutex
	geoClient  *http.Client
}

type sseEvent struct {
	Event string
	Data  string
}

type GeoInfo struct {
	IP      string
	Label   string
	Country string
	City    string
	ISP     string
}

// New creates a web server.
// cfgPath is the path to config.yaml for persisting changes.
// onConfig is called when the user updates target_domain or custom_domain via the web UI.
func New(port int, cfgPath string, triggerCh chan<- struct{}, onConfig func(targetDomain, customDomain, probeSource, pingMode string, pingPort, checkInterval, pingAttempts, switchStableSec int, latencyWeight, jitterWeight, lossWeight, switchImprovement float64)) *Server {
	s := &Server{
		port:       port,
		sseClients: make(map[string]chan sseEvent),
		readyCh:    make(chan struct{}),
		cfgPath:    cfgPath,
		onConfig:   onConfig,
		triggerCh:  triggerCh,
		activeIPs:  make(map[string]bool),
		geoCache:   make(map[string]GeoInfo),
		geoPending: make(map[string]bool),
		geoClient:  &http.Client{Timeout: 4 * time.Second},
	}
	stateDir := filepath.Join(filepath.Dir(cfgPath), "data")
	s.logsPath = filepath.Join(stateDir, "runtime-logs.jsonl")
	s.historyPath = filepath.Join(stateDir, "runtime-history.json")
	s.samplesPath = filepath.Join(stateDir, "runtime-samples.json")
	s.loadPersistedData()
	s.ensureGeoForSamples()
	s.status.Store(&Status{CheckIntervalSec: 300})
	return s
}

// Start begins the HTTP server in a goroutine. Returns immediately.
func (s *Server) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/history", s.handleAPIHistory)
	mux.HandleFunc("/api/ip-stats", s.handleAPIIPStats)
	mux.HandleFunc("/api/ip-samples", s.handleAPIIPSamples)
	mux.HandleFunc("/api/logs", s.handleAPILogs)
	mux.HandleFunc("/api/config", s.handleAPIConfig)
	mux.HandleFunc("/api/check", s.handleCheck)
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
	s.history = pruneHistory(s.history)
	hist := make([]CheckRecord, len(s.history))
	copy(hist, s.history)
	s.historyMu.Unlock()
	s.persistHistory()
	s.broadcast("history", mustJSON(hist))
}

// AddLog broadcasts a log line via SSE and stores it in the buffer.
func (s *Server) AddLog(line string) {
	s.logBufMu.Lock()
	entry := LogEntry{Time: time.Now(), Line: line}
	s.logBuf = append(s.logBuf, entry)
	s.logBuf = pruneLogEntries(s.logBuf)
	s.logBufMu.Unlock()
	s.persistLogs()
	s.broadcast("log", mustJSON(entry))
}

// AddSamples stores per-IP latency samples for long-term stats.
func (s *Server) AddSamples(samples []IPSample) {
	if len(samples) == 0 {
		return
	}
	s.samplesMu.Lock()
	s.samples = append(s.samples, samples...)
	s.samples = pruneSamples(s.samples)
	s.samplesMu.Unlock()
	s.persistSamples()
	s.ensureGeoForIPs(sampleIPs(samples))
	s.broadcast("ipstats", mustJSON(s.computeIPStats()))
}

func sampleIPs(samples []IPSample) []string {
	seen := make(map[string]struct{}, len(samples))
	ips := make([]string, 0, len(samples))
	for _, sample := range samples {
		if sample.IP == "" {
			continue
		}
		if _, ok := seen[sample.IP]; ok {
			continue
		}
		seen[sample.IP] = struct{}{}
		ips = append(ips, sample.IP)
	}
	return ips
}

func isPublicIPv4(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	v4 := parsed.To4()
	if v4 == nil {
		return false
	}
	if v4[0] == 10 || v4[0] == 127 {
		return false
	}
	if v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31 {
		return false
	}
	if v4[0] == 192 && v4[1] == 168 {
		return false
	}
	return true
}

func (s *Server) UpdateResolvedIPs(ips []string) {
	next := make(map[string]bool, len(ips))
	for _, ip := range ips {
		if ip == "" {
			continue
		}
		next[ip] = true
	}
	s.activeIPsMu.Lock()
	s.activeIPs = next
	s.activeIPsMu.Unlock()
}

func (s *Server) isIPActive(ip string) bool {
	s.activeIPsMu.RLock()
	active := s.activeIPs[ip]
	s.activeIPsMu.RUnlock()
	return active
}

func (s *Server) ensureGeoForSamples() {
	s.samplesMu.Lock()
	ips := sampleIPs(s.samples)
	s.samplesMu.Unlock()
	go s.ensureGeoForIPs(ips)
}

func (s *Server) geoLabel(ip string) string {
	s.geoMu.RLock()
	info, ok := s.geoCache[ip]
	s.geoMu.RUnlock()
	if !ok {
		return ""
	}
	return info.Label
}

func (s *Server) ensureGeoForIPs(ips []string) {
	for _, ip := range ips {
		if !isPublicIPv4(ip) {
			continue
		}
		s.geoMu.Lock()
		if _, ok := s.geoCache[ip]; ok {
			s.geoMu.Unlock()
			continue
		}
		if s.geoPending[ip] {
			s.geoMu.Unlock()
			continue
		}
		s.geoPending[ip] = true
		s.geoMu.Unlock()

		go func(target string) {
			info := s.fetchGeoInfo(target)
			s.geoMu.Lock()
			delete(s.geoPending, target)
			s.geoCache[target] = info
			s.geoMu.Unlock()
		}(ip)
	}
}

func compactGeoLabel(country, city, isp string) string {
	location := strings.TrimSpace(country)
	if city != "" {
		if location == "" {
			location = city
		} else if !strings.Contains(city, country) {
			location += city
		} else {
			location = city
		}
	}
	if location != "" && isp != "" {
		return location + " - " + isp
	}
	if isp != "" {
		return isp
	}
	return location
}

func (s *Server) fetchGeoInfo(ip string) GeoInfo {
	info := GeoInfo{IP: ip}
	url := fmt.Sprintf("http://ip-api.com/json/%s?lang=zh-CN&fields=status,country,city,isp", ip)
	resp, err := s.geoClient.Get(url)
	if err != nil {
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info
	}

	var payload struct {
		Status  string `json:"status"`
		Country string `json:"country"`
		City    string `json:"city"`
		ISP     string `json:"isp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return info
	}
	if payload.Status != "success" {
		return info
	}

	info.Country = strings.TrimSpace(payload.Country)
	info.City = strings.TrimSpace(payload.City)
	info.ISP = strings.TrimSpace(payload.ISP)
	info.Label = compactGeoLabel(info.Country, info.City, info.ISP)
	return info
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
	tmpl, err := template.ParseFS(templateFS, "dashboard_v2.html")
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

func (s *Server) handleAPIIPStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.computeIPStats())
}

func (s *Server) handleAPIIPSamples(w http.ResponseWriter, r *http.Request) {
	s.samplesMu.Lock()
	samples := make([]IPSample, len(s.samples))
	copy(samples, s.samples)
	s.samplesMu.Unlock()
	writeJSON(w, samples)
}

func (s *Server) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	s.logBufMu.Lock()
	logs := make([]LogEntry, len(s.logBuf))
	copy(logs, s.logBuf)
	s.logBufMu.Unlock()

	const maxLogs = 400
	if len(logs) > maxLogs {
		logs = logs[len(logs)-maxLogs:]
	}
	writeJSON(w, logs)
}

func (s *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		st := s.status.Load().(*Status)
		writeJSON(w, map[string]interface{}{
			"target_domain":              st.TargetDomain,
			"custom_domain":              st.CustomDomain,
			"probe_source":               st.ProbeSource,
			"ping_mode":                  st.PingMode,
			"ping_port":                  st.PingPort,
			"check_interval":             st.CheckIntervalSec,
			"ping_attempts":              st.PingAttempts,
			"selection_latency_weight":   st.LatencyWeight,
			"selection_jitter_weight":    st.JitterWeight,
			"selection_loss_weight":      st.LossWeight,
			"switch_improvement_percent": st.SwitchImprovement,
			"switch_stable_seconds":      st.SwitchStableSec,
		})
		return
	}

	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		TargetDomain      *string  `json:"target_domain"`
		CustomDomain      *string  `json:"custom_domain"`
		ProbeSource       *string  `json:"probe_source"`
		PingMode          *string  `json:"ping_mode"`
		PingPort          *int     `json:"ping_port"`
		CheckInterval     *int     `json:"check_interval"`
		PingAttempts      *int     `json:"ping_attempts"`
		LatencyWeight     *float64 `json:"selection_latency_weight"`
		JitterWeight      *float64 `json:"selection_jitter_weight"`
		LossWeight        *float64 `json:"selection_loss_weight"`
		SwitchImprovement *float64 `json:"switch_improvement_percent"`
		SwitchStableSec   *int     `json:"switch_stable_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	st := s.status.Load().(*Status)

	// Build what changed
	var newTarget, newCustom, newProbeSource, newPingMode string
	var newPingPort, newCheckInterval, newPingAttempts, newSwitchStableSec int
	var newLatencyWeight, newJitterWeight, newLossWeight, newSwitchImprovement float64
	hasPingPort := false
	hasCheckInterval := false
	hasPingMode := false
	hasPingAttempts := false
	hasLatencyWeight := false
	hasJitterWeight := false
	hasLossWeight := false
	hasSwitchImprovement := false
	hasSwitchStableSec := false

	if body.TargetDomain != nil && *body.TargetDomain != "" && *body.TargetDomain != st.TargetDomain {
		newTarget = *body.TargetDomain
	}
	if body.CustomDomain != nil && *body.CustomDomain != "" && *body.CustomDomain != st.CustomDomain {
		newCustom = *body.CustomDomain
	}
	if body.ProbeSource != nil && *body.ProbeSource != "" && *body.ProbeSource != st.ProbeSource {
		newProbeSource = *body.ProbeSource
	}
	if body.PingMode != nil && *body.PingMode != "" && *body.PingMode != st.PingMode {
		newPingMode = *body.PingMode
		hasPingMode = true
	}
	if body.PingPort != nil && *body.PingPort > 0 && *body.PingPort != st.PingPort {
		newPingPort = *body.PingPort
		hasPingPort = true
	}
	if body.CheckInterval != nil && *body.CheckInterval > 0 && *body.CheckInterval != st.CheckIntervalSec {
		newCheckInterval = *body.CheckInterval
		hasCheckInterval = true
	}
	if body.PingAttempts != nil && *body.PingAttempts > 0 && *body.PingAttempts != st.PingAttempts {
		newPingAttempts = *body.PingAttempts
		hasPingAttempts = true
	}
	if body.LatencyWeight != nil && *body.LatencyWeight > 0 && *body.LatencyWeight != st.LatencyWeight {
		newLatencyWeight = *body.LatencyWeight
		hasLatencyWeight = true
	}
	if body.JitterWeight != nil && *body.JitterWeight >= 0 && *body.JitterWeight != st.JitterWeight {
		newJitterWeight = *body.JitterWeight
		hasJitterWeight = true
	}
	if body.LossWeight != nil && *body.LossWeight >= 0 && *body.LossWeight != st.LossWeight {
		newLossWeight = *body.LossWeight
		hasLossWeight = true
	}
	if body.SwitchImprovement != nil && *body.SwitchImprovement >= 0 && *body.SwitchImprovement != st.SwitchImprovement {
		newSwitchImprovement = *body.SwitchImprovement
		hasSwitchImprovement = true
	}
	if body.SwitchStableSec != nil && *body.SwitchStableSec >= 0 && *body.SwitchStableSec != st.SwitchStableSec {
		newSwitchStableSec = *body.SwitchStableSec
		hasSwitchStableSec = true
	}

	if newTarget == "" && newCustom == "" && newProbeSource == "" && !hasPingMode && !hasPingPort && !hasCheckInterval && !hasPingAttempts && !hasLatencyWeight && !hasJitterWeight && !hasLossWeight && !hasSwitchImprovement && !hasSwitchStableSec {
		writeJSON(w, map[string]string{"error": "no changes or empty values"})
		return
	}

	// Persist to config.yaml
	if newTarget != "" {
		if err := config.UpdateYAMLField(s.cfgPath, "target_domain", newTarget, true); err != nil {
			writeJSON(w, map[string]string{"error": "persist target_domain: " + err.Error()})
			return
		}
	}
	if newCustom != "" {
		if err := config.UpdateYAMLField(s.cfgPath, "custom_domain", newCustom, true); err != nil {
			writeJSON(w, map[string]string{"error": "persist custom_domain: " + err.Error()})
			return
		}
	}
	if newProbeSource != "" {
		if err := config.UpdateYAMLField(s.cfgPath, "probe_source", newProbeSource, true); err != nil {
			writeJSON(w, map[string]string{"error": "persist probe_source: " + err.Error()})
			return
		}
	}
	if hasPingMode {
		if newPingMode != "icmp" && newPingMode != "tcp" {
			writeJSON(w, map[string]string{"error": "ping_mode must be icmp or tcp"})
			return
		}
		if err := config.UpdateYAMLField(s.cfgPath, "ping_mode", newPingMode, true); err != nil {
			writeJSON(w, map[string]string{"error": "persist ping_mode: " + err.Error()})
			return
		}
	}
	if hasPingPort {
		if err := config.UpdateYAMLField(s.cfgPath, "ping_port", fmt.Sprintf("%d", newPingPort), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist ping_port: " + err.Error()})
			return
		}
	}
	if hasCheckInterval {
		if err := config.UpdateYAMLField(s.cfgPath, "check_interval", fmt.Sprintf("%d", newCheckInterval), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist check_interval: " + err.Error()})
			return
		}
	}
	if hasPingAttempts {
		if err := config.UpdateYAMLField(s.cfgPath, "ping_attempts", fmt.Sprintf("%d", newPingAttempts), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist ping_attempts: " + err.Error()})
			return
		}
	}
	if hasLatencyWeight {
		if err := config.UpdateYAMLField(s.cfgPath, "selection_latency_weight", fmt.Sprintf("%.2f", newLatencyWeight), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist selection_latency_weight: " + err.Error()})
			return
		}
	}
	if hasJitterWeight {
		if err := config.UpdateYAMLField(s.cfgPath, "selection_jitter_weight", fmt.Sprintf("%.2f", newJitterWeight), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist selection_jitter_weight: " + err.Error()})
			return
		}
	}
	if hasLossWeight {
		if err := config.UpdateYAMLField(s.cfgPath, "selection_loss_weight", fmt.Sprintf("%.2f", newLossWeight), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist selection_loss_weight: " + err.Error()})
			return
		}
	}
	if hasSwitchImprovement {
		if err := config.UpdateYAMLField(s.cfgPath, "switch_improvement_percent", fmt.Sprintf("%.2f", newSwitchImprovement), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist switch_improvement_percent: " + err.Error()})
			return
		}
	}
	if hasSwitchStableSec {
		if err := config.UpdateYAMLField(s.cfgPath, "switch_stable_seconds", fmt.Sprintf("%d", newSwitchStableSec), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist switch_stable_seconds: " + err.Error()})
			return
		}
	}

	// Notify main loop
	if s.onConfig != nil {
		finalTarget := newTarget
		if finalTarget == "" {
			finalTarget = st.TargetDomain
		}
		finalCustom := newCustom
		if finalCustom == "" {
			finalCustom = st.CustomDomain
		}
		finalProbeSource := newProbeSource
		if finalProbeSource == "" {
			finalProbeSource = st.ProbeSource
		}
		finalPingMode := st.PingMode
		if hasPingMode {
			finalPingMode = newPingMode
		}
		finalPingPort := st.PingPort
		if hasPingPort {
			finalPingPort = newPingPort
		}
		finalCheckInterval := st.CheckIntervalSec
		if hasCheckInterval {
			finalCheckInterval = newCheckInterval
		}
		finalPingAttempts := st.PingAttempts
		if hasPingAttempts {
			finalPingAttempts = newPingAttempts
		}
		finalLatencyWeight := st.LatencyWeight
		if hasLatencyWeight {
			finalLatencyWeight = newLatencyWeight
		}
		finalJitterWeight := st.JitterWeight
		if hasJitterWeight {
			finalJitterWeight = newJitterWeight
		}
		finalLossWeight := st.LossWeight
		if hasLossWeight {
			finalLossWeight = newLossWeight
		}
		finalSwitchImprovement := st.SwitchImprovement
		if hasSwitchImprovement {
			finalSwitchImprovement = newSwitchImprovement
		}
		finalSwitchStableSec := st.SwitchStableSec
		if hasSwitchStableSec {
			finalSwitchStableSec = newSwitchStableSec
		}
		s.onConfig(finalTarget, finalCustom, finalProbeSource, finalPingMode, finalPingPort, finalCheckInterval, finalPingAttempts, finalSwitchStableSec, finalLatencyWeight, finalJitterWeight, finalLossWeight, finalSwitchImprovement)
	}

	// Update in-memory status
	if newTarget != "" {
		st.TargetDomain = newTarget
	}
	if newCustom != "" {
		st.CustomDomain = newCustom
	}
	if newProbeSource != "" {
		st.ProbeSource = newProbeSource
	}
	if hasPingMode {
		st.PingMode = newPingMode
	}
	if hasPingPort {
		st.PingPort = newPingPort
	}
	if hasCheckInterval {
		st.CheckIntervalSec = newCheckInterval
	}
	if hasPingAttempts {
		st.PingAttempts = newPingAttempts
	}
	if hasLatencyWeight {
		st.LatencyWeight = newLatencyWeight
	}
	if hasJitterWeight {
		st.JitterWeight = newJitterWeight
	}
	if hasLossWeight {
		st.LossWeight = newLossWeight
	}
	if hasSwitchImprovement {
		st.SwitchImprovement = newSwitchImprovement
	}
	if hasSwitchStableSec {
		st.SwitchStableSec = newSwitchStableSec
	}
	s.UpdateStatus(st)

	log.Printf("[config] updated: target_domain=%q custom_domain=%q probe_source=%q ping_mode=%q ping_port=%d check_interval=%d ping_attempts=%d latency_weight=%.2f jitter_weight=%.2f loss_weight=%.2f switch_improvement=%.2f switch_stable_seconds=%d",
		newTarget, newCustom, newProbeSource, newPingMode, newPingPort, newCheckInterval, newPingAttempts, newLatencyWeight, newJitterWeight, newLossWeight, newSwitchImprovement, newSwitchStableSec)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	select {
	case s.triggerCh <- struct{}{}:
		writeJSON(w, map[string]bool{"ok": true})
	default:
		writeJSON(w, map[string]string{"error": "check already in progress"})
	}
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

	stats := s.computeIPStats()
	if len(stats) > 0 {
		fmt.Fprintf(w, "event: ipstats\ndata: %s\n\n", mustJSON(stats))
	}

	// Send buffered logs (so refresh doesn't lose them); signal reset first
	fmt.Fprintf(w, "event: reset\ndata: {}\n\n")
	s.logBufMu.Lock()
	logs := make([]LogEntry, len(s.logBuf))
	copy(logs, s.logBuf)
	s.logBufMu.Unlock()
	for _, entry := range logs {
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", mustJSON(entry))
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
