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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dns-latency-router/internal/agent"
	"dns-latency-router/internal/config"
)

//go:embed dashboard.html assets/flags/*
var templateFS embed.FS

const agentInstallerURL = "https://raw.githubusercontent.com/kuaichu/DnslatencyRouter/main/scripts/install-agent.sh"

// Status holds the current state exposed via API/SSE.
type Status struct {
	TargetDomain      string          `json:"targetDomain"`
	CustomDomain      string          `json:"customDomain"`
	ProbeSource       string          `json:"probeSource"`
	Carrier           string          `json:"carrier"`
	CarrierLabel      string          `json:"carrierLabel"`
	CurrentIP         string          `json:"currentIP"`
	Latency           float64         `json:"latency"`   // ms, 0 = unknown
	LastCheck         string          `json:"lastCheck"` // RFC3339
	NextCheck         string          `json:"nextCheck"` // RFC3339
	IsRunning         bool            `json:"isRunning"`
	DiscoveredCount   int             `json:"discoveredCount"`
	CheckIntervalSec  int             `json:"checkIntervalSec"`
	PingMode          string          `json:"pingMode"`
	PingPort          int             `json:"pingPort"`
	PingAttempts      int             `json:"pingAttempts"`
	LatencyWeight     float64         `json:"latencyWeight"`
	JitterWeight      float64         `json:"jitterWeight"`
	LossWeight        float64         `json:"lossWeight"`
	SwitchImprovement float64         `json:"switchImprovement"`
	SwitchStableSec   int             `json:"switchStableSec"`
	Agents            []AgentStatus   `json:"agents,omitempty"`
	Profiles          []ProfileStatus `json:"profiles,omitempty"`
}

type AgentStatus struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Carrier      string    `json:"carrier"`
	CarrierLabel string    `json:"carrierLabel"`
	ProbeSource  string    `json:"probeSource"`
	LastSeen     time.Time `json:"lastSeen"`
	AgeSeconds   int       `json:"ageSeconds"`
	ProfileCount int       `json:"profileCount"`
	Status       string    `json:"status"`
}

type ProfileStatus struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Slug            string         `json:"slug"`
	TargetDomain    string         `json:"targetDomain"`
	TargetDomains   []string       `json:"targetDomains,omitempty"`
	ProbeSource     string         `json:"probeSource"`
	Carrier         string         `json:"carrier"`
	CarrierLabel    string         `json:"carrierLabel"`
	DiscoveredCount int            `json:"discoveredCount"`
	Regions         []RegionStatus `json:"regions"`
}

type RegionStatus struct {
	Region         string  `json:"region"`
	Label          string  `json:"label"`
	CustomDomain   string  `json:"customDomain"`
	CurrentIP      string  `json:"currentIP"`
	BestIP         string  `json:"bestIP"`
	Latency        float64 `json:"latency"`
	Score          float64 `json:"score"`
	CandidateCount int     `json:"candidateCount"`
	Status         string  `json:"status"`
}

// CheckRecord is one completed check cycle.
type CheckRecord struct {
	Time      time.Time `json:"time"`
	ProfileID string    `json:"profileId,omitempty"`
	Region    string    `json:"region,omitempty"`
	IP        string    `json:"ip"`
	Latency   float64   `json:"latency"` // ms, 0 if failed
	Success   bool      `json:"success"`
	Error     string    `json:"error,omitempty"`
}

// Server is the web dashboard HTTP server.
type Server struct {
	port                 int
	status               atomic.Value
	history              []CheckRecord
	historyMu            sync.Mutex
	samples              []IPSample
	samplesMu            sync.Mutex
	sseClients           map[string]chan sseEvent
	sseMu                sync.Mutex
	sseNextID            int64
	httpServer           *http.Server
	readyCh              chan struct{}
	cfgPath              string                                                                                                                                                                                                                                                                                                                                                                               // for persisting config changes
	onConfig             func(targetDomain, customDomain, probeSource, carrier, pingMode string, pingPort, checkInterval, pingAttempts, switchStableSec int, latencyWeight, jitterWeight, lossWeight, switchImprovement float64, failedOrphanTTLHours int, fallbackBaselineIP, alertWebhookURL string, timePenaltyStartHour, timePenaltyEndHour int, timePenaltyScore float64, timePenaltyOrgKeywords string) // callback to notify main loop
	onProfiles           func(*config.Config)
	onCloudflare         func(config.CloudflareConfig)
	triggerCh            chan<- struct{} // signal main loop to run a check immediately
	logBuf               []LogEntry
	logBufMu             sync.Mutex
	logsPath             string
	historyPath          string
	samplesPath          string
	activeIPs            map[string]bool
	activeIPsByProfile   map[string]map[string]bool
	activeIPsMu          sync.RWMutex
	agentReports         map[string]agent.Report
	agentReportsMu       sync.RWMutex
	geoCache             map[string]GeoInfo
	geoPending           map[string]bool
	geoMu                sync.RWMutex
	geoClient            *http.Client
	runtimeCfgMu         sync.RWMutex
	failedOrphanTTLHours int
	fallbackBaselineIP   string
	alertWebhookURL      string
	timePenaltyStartHour int
	timePenaltyEndHour   int
	timePenaltyScore     float64
	timePenaltyKeywords  string
}

type sseEvent struct {
	Event string
	Data  string
}

type GeoInfo struct {
	IP          string
	Label       string
	CountryCode string
	Country     string
	City        string
	ISP         string
}

// New creates a web server.
// cfgPath is the path to config.yaml for persisting changes.
// onConfig is called when the user updates target_domain or custom_domain via the web UI.
func New(port int, cfgPath string, triggerCh chan<- struct{}, onConfig func(targetDomain, customDomain, probeSource, carrier, pingMode string, pingPort, checkInterval, pingAttempts, switchStableSec int, latencyWeight, jitterWeight, lossWeight, switchImprovement float64, failedOrphanTTLHours int, fallbackBaselineIP, alertWebhookURL string, timePenaltyStartHour, timePenaltyEndHour int, timePenaltyScore float64, timePenaltyOrgKeywords string)) *Server {
	s := &Server{
		port:               port,
		sseClients:         make(map[string]chan sseEvent),
		readyCh:            make(chan struct{}),
		cfgPath:            cfgPath,
		onConfig:           onConfig,
		triggerCh:          triggerCh,
		activeIPs:          make(map[string]bool),
		activeIPsByProfile: make(map[string]map[string]bool),
		agentReports:       make(map[string]agent.Report),
		geoCache:           make(map[string]GeoInfo),
		geoPending:         make(map[string]bool),
		geoClient:          &http.Client{Timeout: 4 * time.Second},
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
	mux.Handle("/assets/", http.FileServer(http.FS(templateFS)))
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/history", s.handleAPIHistory)
	mux.HandleFunc("/api/ip-stats", s.handleAPIIPStats)
	mux.HandleFunc("/api/ip-samples", s.handleAPIIPSamples)
	mux.HandleFunc("/api/logs", s.handleAPILogs)
	mux.HandleFunc("/api/config", s.handleAPIConfig)
	mux.HandleFunc("/api/airport-profiles", s.handleAPIAirportProfiles)
	mux.HandleFunc("/api/agent/jobs", s.handleAPIAgentJobs)
	mux.HandleFunc("/api/agent/reports", s.handleAPIAgentReports)
	mux.HandleFunc("/api/agent/install-command", s.handleAPIAgentInstallCommand)
	mux.HandleFunc("/api/agent/install.sh", s.handleAPIAgentInstallScript)
	mux.HandleFunc("/api/agent/download/linux-amd64", s.handleAPIAgentDownload)
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

func (s *Server) SetSafeguards(ttlHours int, fallbackIP, webhookURL string) {
	s.runtimeCfgMu.Lock()
	s.failedOrphanTTLHours = ttlHours
	s.fallbackBaselineIP = strings.TrimSpace(fallbackIP)
	s.alertWebhookURL = strings.TrimSpace(webhookURL)
	s.runtimeCfgMu.Unlock()
}

func (s *Server) SetTimePenaltyConfig(startHour, endHour int, score float64, keywords string) {
	s.runtimeCfgMu.Lock()
	s.timePenaltyStartHour = startHour
	s.timePenaltyEndHour = endHour
	s.timePenaltyScore = score
	s.timePenaltyKeywords = strings.TrimSpace(keywords)
	s.runtimeCfgMu.Unlock()
}

func (s *Server) SetGeoProxy(proxyURL string) {
	proxyURL = strings.TrimSpace(proxyURL)
	transport := &http.Transport{}
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			log.Printf("[geo] invalid proxy_url %q, geo lookup will use direct connection: %v", proxyURL, err)
		} else {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	s.geoMu.Lock()
	s.geoClient = &http.Client{Timeout: 4 * time.Second, Transport: transport}
	s.geoMu.Unlock()
}

func (s *Server) SetProfilesCallback(cb func(*config.Config)) {
	s.runtimeCfgMu.Lock()
	s.onProfiles = cb
	s.runtimeCfgMu.Unlock()
}

func (s *Server) SetCloudflareCallback(cb func(config.CloudflareConfig)) {
	s.runtimeCfgMu.Lock()
	s.onCloudflare = cb
	s.runtimeCfgMu.Unlock()
}

func (s *Server) safeguards() (int, string, string) {
	s.runtimeCfgMu.RLock()
	defer s.runtimeCfgMu.RUnlock()
	return s.failedOrphanTTLHours, s.fallbackBaselineIP, s.alertWebhookURL
}

func (s *Server) timePenaltyConfig() (int, int, float64, string) {
	s.runtimeCfgMu.RLock()
	defer s.runtimeCfgMu.RUnlock()
	return s.timePenaltyStartHour, s.timePenaltyEndHour, s.timePenaltyScore, s.timePenaltyKeywords
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
	pruned := s.pruneInactiveOrphanSamplesLocked()
	s.samplesMu.Unlock()
	s.persistSamples()
	s.ensureGeoForIPs(sampleIPs(samples))
	if pruned {
		log.Printf("[gc] runtime sample store compacted after orphan inactivity pruning")
	}
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
	s.UpdateResolvedIPsForProfile("", ips)
}

func (s *Server) UpdateResolvedIPsForProfile(profileID string, ips []string) {
	next := make(map[string]bool, len(ips))
	for _, ip := range ips {
		if ip == "" {
			continue
		}
		next[ip] = true
	}
	s.activeIPsMu.Lock()
	if profileID == "" {
		s.activeIPs = next
	} else {
		s.activeIPsByProfile[profileID] = next
		merged := make(map[string]bool)
		for _, group := range s.activeIPsByProfile {
			for ip := range group {
				merged[ip] = true
			}
		}
		s.activeIPs = merged
	}
	s.activeIPsMu.Unlock()
}

func (s *Server) isIPActive(ip string) bool {
	s.activeIPsMu.RLock()
	active := s.activeIPs[ip]
	s.activeIPsMu.RUnlock()
	return active
}

func (s *Server) isIPActiveInProfile(profileID, ip string) bool {
	if profileID == "" {
		return s.isIPActive(ip)
	}
	s.activeIPsMu.RLock()
	defer s.activeIPsMu.RUnlock()
	group := s.activeIPsByProfile[profileID]
	if group == nil {
		return s.activeIPs[ip]
	}
	return group[ip]
}

func (s *Server) GeoForIP(ip string) GeoInfo {
	if !isPublicIPv4(ip) {
		return GeoInfo{IP: ip}
	}
	s.geoMu.RLock()
	info, ok := s.geoCache[ip]
	s.geoMu.RUnlock()
	if ok {
		return info
	}
	info = s.fetchGeoInfo(ip)
	s.geoMu.Lock()
	if geoInfoUseful(info) {
		s.geoCache[ip] = info
	}
	delete(s.geoPending, ip)
	s.geoMu.Unlock()
	return info
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
			if geoInfoUseful(info) {
				s.geoCache[target] = info
			}
			s.geoMu.Unlock()
		}(ip)
	}
}

func geoInfoUseful(info GeoInfo) bool {
	return strings.TrimSpace(info.Label) != "" ||
		strings.TrimSpace(info.CountryCode) != "" ||
		strings.TrimSpace(info.Country) != "" ||
		strings.TrimSpace(info.City) != "" ||
		strings.TrimSpace(info.ISP) != ""
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

func (s *Server) geoHTTPClient() *http.Client {
	s.geoMu.RLock()
	client := s.geoClient
	s.geoMu.RUnlock()
	if client == nil {
		return &http.Client{Timeout: 4 * time.Second}
	}
	return client
}

func (s *Server) fetchGeoInfo(ip string) GeoInfo {
	info := GeoInfo{IP: ip}
	url := fmt.Sprintf("http://ip-api.com/json/%s?lang=zh-CN&fields=status,countryCode,country,city,isp", ip)
	resp, err := s.geoHTTPClient().Get(url)
	if err != nil {
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info
	}

	var payload struct {
		Status      string `json:"status"`
		CountryCode string `json:"countryCode"`
		Country     string `json:"country"`
		City        string `json:"city"`
		ISP         string `json:"isp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return info
	}
	if payload.Status != "success" {
		return info
	}

	info.CountryCode = strings.TrimSpace(payload.CountryCode)
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
	tmpl, err := template.ParseFS(templateFS, "dashboard.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	tmpl.Execute(w, nil)
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	st := *s.status.Load().(*Status)
	st.Agents = s.AgentStatuses(0)
	writeJSON(w, &st)
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

func (s *Server) handleAPIAgentInstallScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.Redirect(w, r, agentInstallerURL, http.StatusTemporaryRedirect)
}

func (s *Server) handleAPIAgentInstallCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		writeJSON(w, map[string]string{"error": "load config: " + err.Error()})
		return
	}
	token := strings.TrimSpace(cfg.Agent.Token)
	if token == "" {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]string{"error": "agent token is not configured; set it in Agent 探针 first"})
		return
	}
	controllerURL := agentControllerURL(cfg, r)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]interface{}{
		"ok":             true,
		"script_url":     agentInstallerURL,
		"controller_url": controllerURL,
		"command":        buildAgentInstallCommand(controllerURL, token, cfg.CheckIntervalSec),
	})
}

func (s *Server) handleAPIAgentDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path, ok := findAgentBinary()
	if !ok {
		http.Error(w, "agent binary not found on controller; place dns-latency-router-agent-linux-amd64 next to controller binary", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="dns-latency-router-agent"`)
	http.ServeFile(w, r, path)
}

func publicBaseURL(r *http.Request) string {
	scheme := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return strings.TrimRight(scheme+"://"+host, "/")
}

func agentControllerURL(cfg *config.Config, r *http.Request) string {
	if cfg != nil {
		if configured := strings.TrimSpace(cfg.Agent.ControllerURL); configured != "" {
			return strings.TrimRight(configured, "/")
		}
	}
	return publicBaseURL(r)
}

func findAgentBinary() (string, bool) {
	names := []string{
		"dns-latency-router-agent-linux-amd64",
		"dns-latency-router-agent",
		"dns-latency-router-agent.exe",
	}
	dirs := []string{}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	for _, dir := range dirs {
		for _, name := range names {
			path := filepath.Join(dir, name)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path, true
			}
		}
	}
	return "", false
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func concreteAgentCarrier(value string) string {
	carrier := config.NormalizeCarrier(value)
	switch carrier {
	case "unicom", "telecom", "mobile":
		return carrier
	default:
		return ""
	}
}

func agentCarrierLabel(carrier string) string {
	if carrier := concreteAgentCarrier(carrier); carrier != "" {
		return config.CarrierLabel(carrier)
	}
	return "未知运营商"
}

func buildAgentInstallCommand(controllerURL, token string, interval int) string {
	if interval <= 0 {
		interval = 300
	}
	parts := []string{
		"curl -fsSL",
		shellQuote(agentInstallerURL),
		"| bash -s --",
		"--controller",
		shellQuote(controllerURL),
		"--token",
		shellQuote(token),
		"--interval",
		shellQuote(fmt.Sprintf("%d", interval)),
	}
	return strings.Join(parts, " ")
}

func (s *Server) agentAuthorized(r *http.Request) bool {
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		return false
	}
	token := strings.TrimSpace(cfg.Agent.Token)
	if token == "" {
		return false
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		header = strings.TrimSpace(header[7:])
	}
	if header == "" {
		header = strings.TrimSpace(r.Header.Get("X-Agent-Token"))
	}
	return header == token
}

func (s *Server) handleAPIAgentJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.agentAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		writeJSON(w, map[string]string{"error": "load config: " + err.Error()})
		return
	}
	agentID := strings.TrimSpace(r.Header.Get("X-Agent-ID"))
	if agentID == "" {
		agentID = strings.TrimSpace(r.URL.Query().Get("agent_id"))
	}
	var peer config.AgentPeerConfig
	peerFound := false
	for _, candidate := range cfg.Agents {
		if strings.EqualFold(strings.TrimSpace(candidate.ID), agentID) {
			peer = candidate
			peerFound = true
			break
		}
	}
	agentName := strings.TrimSpace(peer.Name)
	agentProbeSource := strings.TrimSpace(peer.ProbeSource)
	agentCarrier := ""
	agentCarrierLabel := ""
	if peerFound {
		agentCarrier = concreteAgentCarrier(peer.Carrier)
		if agentCarrier != "" {
			agentCarrierLabel = config.CarrierLabel(agentCarrier)
		}
	}
	profiles := cfg.AirportProfiles
	if len(profiles) == 0 && cfg.TargetDomain != "" {
		profiles = []config.AirportProfile{cfg.LegacyProfile()}
	}
	jobs := make([]agent.ProfileJob, 0, len(profiles))
	for _, profile := range profiles {
		jobs = append(jobs, agent.ProfileJob{
			ID:            profile.ID,
			Name:          profile.Name,
			Slug:          profile.Slug,
			TargetDomains: append([]string(nil), profile.TargetDomains...),
			ProbeSource:   profile.ProbeSource,
			Carrier:       profile.Carrier,
		})
	}
	writeJSON(w, agent.JobResponse{
		ServerTime:         time.Now(),
		AgentName:          agentName,
		AgentProbeSource:   agentProbeSource,
		AgentCarrier:       agentCarrier,
		AgentCarrierLabel:  agentCarrierLabel,
		CheckInterval:      cfg.CheckIntervalSec,
		PingMode:           cfg.PingMode,
		PingPort:           cfg.PingPort,
		PingTimeoutSeconds: cfg.PingTimeoutSec,
		PingAttempts:       cfg.PingAttempts,
		LatencyWeight:      cfg.LatencyWeight,
		JitterWeight:       cfg.JitterWeight,
		LossWeight:         cfg.LossWeight,
		Profiles:           jobs,
	})
}

func (s *Server) handleAPIAgentReports(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		cfg, err := config.Load(s.cfgPath)
		ttl := 15 * time.Minute
		if err == nil && cfg.Agent.ReportTTLSeconds > 0 {
			ttl = time.Duration(cfg.Agent.ReportTTLSeconds) * time.Second
		}
		reports := s.AgentReports(ttl)
		sort.Slice(reports, func(i, j int) bool {
			if reports[i].Carrier != reports[j].Carrier {
				return reports[i].Carrier < reports[j].Carrier
			}
			if reports[i].AgentName != reports[j].AgentName {
				return reports[i].AgentName < reports[j].AgentName
			}
			return reports[i].AgentID < reports[j].AgentID
		})
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, reports)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.agentAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var report agent.Report
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		writeJSON(w, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	report.AgentID = strings.TrimSpace(report.AgentID)
	if report.AgentID == "" {
		writeJSON(w, map[string]string{"error": "agentId is required"})
		return
	}
	if cfg, err := config.Load(s.cfgPath); err == nil {
		authorizedCarrier := ""
		for _, peer := range cfg.Agents {
			if !strings.EqualFold(strings.TrimSpace(peer.ID), report.AgentID) {
				continue
			}
			if name := strings.TrimSpace(peer.Name); name != "" {
				report.AgentName = name
			}
			if source := strings.TrimSpace(peer.ProbeSource); source != "" {
				report.ProbeSource = source
			}
			if carrier := concreteAgentCarrier(peer.Carrier); carrier != "" {
				authorizedCarrier = carrier
				report.Carrier = carrier
				report.CarrierLabel = config.CarrierLabel(carrier)
			}
			break
		}
		if authorizedCarrier == "" {
			report.Carrier = ""
			report.CarrierLabel = agentCarrierLabel("")
		}
	} else {
		report.Carrier = ""
		report.CarrierLabel = agentCarrierLabel("")
	}
	if report.FinishedAt.IsZero() {
		report.FinishedAt = time.Now()
	}
	if report.StartedAt.IsZero() {
		report.StartedAt = report.FinishedAt
	}

	s.agentReportsMu.Lock()
	s.agentReports[report.AgentID] = report
	s.agentReportsMu.Unlock()

	for _, profile := range report.Profiles {
		if len(profile.ResolvedIPs) > 0 {
			s.UpdateResolvedIPsForProfile(profile.ProfileID, profile.ResolvedIPs)
		}
	}
	s.AddSamples(agentSamplesFromReport(report))

	st := s.status.Load().(*Status)
	st.Agents = s.AgentStatuses(0)
	s.UpdateStatus(st)

	select {
	case s.triggerCh <- struct{}{}:
	default:
	}
	log.Printf("[agent] report accepted from %s (%s), profiles=%d", report.AgentID, report.CarrierLabel, len(report.Profiles))
	writeJSON(w, map[string]bool{"ok": true})
}

func agentSamplesFromReport(report agent.Report) []IPSample {
	samples := make([]IPSample, 0)
	carrier := concreteAgentCarrier(report.Carrier)
	carrierLabel := agentCarrierLabel(carrier)
	region := "agent-unknown"
	if carrier != "" {
		region = "carrier-" + carrier
	}
	for _, profile := range report.Profiles {
		for _, result := range profile.Results {
			sample := IPSample{
				Time:         report.FinishedAt,
				AgentID:      report.AgentID,
				AgentName:    report.AgentName,
				Carrier:      carrier,
				CarrierLabel: carrierLabel,
				ProbeSource:  report.ProbeSource,
				ProfileID:    profile.ProfileID,
				ProfileName:  profile.ProfileName,
				Region:       region,
				RegionLabel:  carrierLabel,
				IP:           result.IP,
				Latency:      result.Latency,
				Jitter:       result.Jitter,
				LossRate:     result.LossRate,
				Score:        result.Score,
				Success:      result.Error == "",
				Error:        result.Error,
			}
			samples = append(samples, sample)
		}
	}
	return samples
}

func (s *Server) AgentReports(ttl time.Duration) []agent.Report {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	cutoff := time.Now().Add(-ttl)
	s.agentReportsMu.RLock()
	defer s.agentReportsMu.RUnlock()
	reports := make([]agent.Report, 0, len(s.agentReports))
	for _, report := range s.agentReports {
		if !report.FinishedAt.IsZero() && report.FinishedAt.Before(cutoff) {
			continue
		}
		reports = append(reports, report)
	}
	return reports
}

func (s *Server) AgentStatuses(ttl time.Duration) []AgentStatus {
	var peers []config.AgentPeerConfig
	if loadedCfg, err := config.Load(s.cfgPath); err == nil {
		peers = loadedCfg.Agents
		if ttl <= 0 && loadedCfg.Agent.ReportTTLSeconds > 0 {
			ttl = time.Duration(loadedCfg.Agent.ReportTTLSeconds) * time.Second
		}
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	now := time.Now()
	cutoff := now.Add(-ttl)
	statusesByID := make(map[string]AgentStatus, len(peers))
	for _, peer := range peers {
		id := strings.TrimSpace(peer.ID)
		if id == "" {
			continue
		}
		if strings.EqualFold(id, "controller") {
			continue
		}
		name := strings.TrimSpace(peer.Name)
		if name == "" {
			name = id
		}
		carrier := concreteAgentCarrier(peer.Carrier)
		statusesByID[id] = AgentStatus{
			ID:           id,
			Name:         name,
			Carrier:      carrier,
			CarrierLabel: agentCarrierLabel(carrier),
			ProbeSource:  strings.TrimSpace(peer.ProbeSource),
			Status:       "offline",
		}
	}
	s.agentReportsMu.RLock()
	defer s.agentReportsMu.RUnlock()
	for _, report := range s.agentReports {
		id := strings.TrimSpace(report.AgentID)
		if id == "" {
			continue
		}
		status := statusesByID[id]
		status.ID = id
		if strings.TrimSpace(status.Name) == "" {
			status.Name = strings.TrimSpace(report.AgentName)
		}
		if strings.TrimSpace(status.Name) == "" {
			status.Name = id
		}
		status.Carrier = concreteAgentCarrier(status.Carrier)
		if strings.TrimSpace(status.CarrierLabel) == "" || status.Carrier == "" {
			status.CarrierLabel = agentCarrierLabel(status.Carrier)
		} else {
			status.CarrierLabel = config.CarrierLabel(status.Carrier)
		}
		if strings.TrimSpace(status.ProbeSource) == "" && strings.TrimSpace(report.ProbeSource) != "" {
			status.ProbeSource = strings.TrimSpace(report.ProbeSource)
		}
		status.LastSeen = report.FinishedAt
		status.ProfileCount = len(report.Profiles)
		status.AgeSeconds = 0
		status.Status = "offline"
		if !report.FinishedAt.IsZero() {
			status.AgeSeconds = int(now.Sub(report.FinishedAt).Seconds())
			if status.AgeSeconds < 0 {
				status.AgeSeconds = 0
			}
			status.Status = "online"
		}
		if !report.FinishedAt.IsZero() && report.FinishedAt.Before(cutoff) {
			status.Status = "stale"
		}
		statusesByID[id] = status
	}
	statuses := make([]AgentStatus, 0, len(statusesByID))
	for _, status := range statusesByID {
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool {
		rank := func(status string) int {
			switch status {
			case "online":
				return 0
			case "stale":
				return 1
			default:
				return 2
			}
		}
		if rank(statuses[i].Status) != rank(statuses[j].Status) {
			return rank(statuses[i].Status) < rank(statuses[j].Status)
		}
		if statuses[i].Carrier != statuses[j].Carrier {
			return statuses[i].Carrier < statuses[j].Carrier
		}
		if statuses[i].Name != statuses[j].Name {
			return statuses[i].Name < statuses[j].Name
		}
		return statuses[i].ID < statuses[j].ID
	})
	return statuses
}

type airportProfilesResponse struct {
	BaseDomain string                  `json:"base_domain"`
	Profiles   []airportProfilePayload `json:"airport_profiles"`
}

type airportProfilesRequest struct {
	BaseDomain *string                 `json:"base_domain"`
	Profiles   []airportProfilePayload `json:"airport_profiles"`
}

type airportProfilePayload struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Slug          string   `json:"slug"`
	TargetDomains []string `json:"target_domains"`
	ProbeSource   string   `json:"probe_source"`
	Carrier       string   `json:"carrier"`
}

type agentPeerPayload struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ProbeSource string `json:"probe_source"`
	Carrier     string `json:"carrier"`
}

func airportProfileToPayload(profile config.AirportProfile) airportProfilePayload {
	return airportProfilePayload{
		ID:            profile.ID,
		Name:          profile.Name,
		Slug:          profile.Slug,
		TargetDomains: append([]string(nil), profile.TargetDomains...),
		ProbeSource:   profile.ProbeSource,
		Carrier:       profile.Carrier,
	}
}

func agentPeerToPayload(peer config.AgentPeerConfig) agentPeerPayload {
	return agentPeerPayload{
		ID:          peer.ID,
		Name:        peer.Name,
		ProbeSource: peer.ProbeSource,
		Carrier:     peer.Carrier,
	}
}

func agentPeersToPayload(peers []config.AgentPeerConfig) []agentPeerPayload {
	out := make([]agentPeerPayload, 0, len(peers))
	for _, peer := range peers {
		out = append(out, agentPeerToPayload(peer))
	}
	return out
}

func profileLookup(profiles []config.AirportProfile) map[string]config.AirportProfile {
	out := make(map[string]config.AirportProfile, len(profiles)*2)
	for _, profile := range profiles {
		if profile.ID != "" {
			out[strings.ToLower(profile.ID)] = profile
		}
		if profile.Slug != "" {
			out[strings.ToLower(profile.Slug)] = profile
		}
	}
	return out
}

func (s *Server) handleAPIAirportProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		cfg, err := config.Load(s.cfgPath)
		if err != nil {
			writeJSON(w, map[string]string{"error": "load config: " + err.Error()})
			return
		}
		profiles := make([]airportProfilePayload, 0, len(cfg.AirportProfiles))
		for _, profile := range cfg.AirportProfiles {
			profiles = append(profiles, airportProfileToPayload(profile))
		}
		writeJSON(w, airportProfilesResponse{BaseDomain: cfg.BaseDomain, Profiles: profiles})
		return
	}

	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body airportProfilesRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	current, err := config.Load(s.cfgPath)
	if err != nil {
		writeJSON(w, map[string]string{"error": "load config: " + err.Error()})
		return
	}

	next := *current
	if body.BaseDomain != nil {
		next.BaseDomain = strings.TrimSpace(*body.BaseDomain)
	}
	existing := profileLookup(current.AirportProfiles)
	nextProfiles := make([]config.AirportProfile, 0, len(body.Profiles))
	for i, incoming := range body.Profiles {
		id := strings.TrimSpace(incoming.ID)
		slug := strings.TrimSpace(incoming.Slug)
		name := strings.TrimSpace(incoming.Name)
		if slug == "" {
			slug = id
		}
		key := strings.ToLower(id)
		if key == "" {
			key = strings.ToLower(slug)
		}
		prev := existing[key]
		if prev.ID == "" && slug != "" {
			prev = existing[strings.ToLower(slug)]
		}
		entry := prev.EntryRecord
		if entry.Label == "" {
			entry.Label = "全局最快"
		}
		profile := config.AirportProfile{
			ID:             id,
			Name:           name,
			Slug:           slug,
			TargetDomains:  incoming.TargetDomains,
			ProbeSource:    strings.TrimSpace(incoming.ProbeSource),
			Carrier:        config.NormalizeCarrier(incoming.Carrier),
			EntryRecord:    entry,
			RegionRecords:  prev.RegionRecords,
			CarrierRecords: prev.CarrierRecords,
		}
		if len(profile.TargetDomains) == 0 {
			writeJSON(w, map[string]string{"error": fmt.Sprintf("airport_profiles[%d] 至少需要一个入口域名", i+1)})
			return
		}
		nextProfiles = append(nextProfiles, profile)
	}
	if len(nextProfiles) == 0 {
		writeJSON(w, map[string]string{"error": "至少保留一个机场配置"})
		return
	}
	next.AirportProfiles = nextProfiles
	if err := next.Normalize(); err != nil {
		writeJSON(w, map[string]string{"error": "配置校验失败: " + err.Error()})
		return
	}
	if err := config.Save(s.cfgPath, &next); err != nil {
		writeJSON(w, map[string]string{"error": "保存失败: " + err.Error()})
		return
	}
	latest, err := config.Load(s.cfgPath)
	if err != nil {
		writeJSON(w, map[string]string{"error": "重新加载失败: " + err.Error()})
		return
	}
	if s.onProfiles != nil {
		s.onProfiles(latest)
	}

	st := s.status.Load().(*Status)
	st.Profiles = buildProfileStatuses(latest)
	if len(st.Profiles) > 0 {
		st.TargetDomain = st.Profiles[0].TargetDomain
		st.CustomDomain = ""
		if len(st.Profiles[0].Regions) > 0 {
			st.CustomDomain = st.Profiles[0].Regions[0].CustomDomain
		}
		st.ProbeSource = st.Profiles[0].ProbeSource
		st.Carrier = st.Profiles[0].Carrier
		st.CarrierLabel = st.Profiles[0].CarrierLabel
	}
	s.UpdateStatus(st)
	writeJSON(w, map[string]bool{"ok": true})
}

func buildProfileStatuses(cfg *config.Config) []ProfileStatus {
	profiles := make([]ProfileStatus, 0, len(cfg.AirportProfiles))
	for _, profile := range cfg.AirportProfiles {
		regions := make([]RegionStatus, 0, len(profile.RegionRecords)+len(profile.CarrierRecords)+1)
		if profile.EntryRecord.CustomDomain != "" || profile.EntryRecord.RecordID != "" {
			label := profile.EntryRecord.Label
			if label == "" {
				label = "全局最快"
			}
			regions = append(regions, RegionStatus{
				Region:       "entry",
				Label:        label,
				CustomDomain: profile.EntryRecord.CustomDomain,
				Status:       "no_candidate",
			})
		}
		for carrier, rec := range profile.CarrierRecords {
			label := rec.Label
			if label == "" {
				label = config.CarrierLabel(carrier)
			}
			regions = append(regions, RegionStatus{
				Region:       "carrier-" + carrier,
				Label:        label,
				CustomDomain: rec.CustomDomain,
				Status:       "no_candidate",
			})
		}
		profiles = append(profiles, ProfileStatus{
			ID:            profile.ID,
			Name:          profile.Name,
			Slug:          profile.Slug,
			TargetDomain:  profile.TargetDomain,
			TargetDomains: append([]string(nil), profile.TargetDomains...),
			ProbeSource:   profile.ProbeSource,
			Carrier:       profile.Carrier,
			CarrierLabel:  config.EffectiveCarrierLabelFor(profile.Carrier, profile.ProbeSource),
			Regions:       regions,
		})
	}
	return profiles
}

func (s *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		st := s.status.Load().(*Status)
		ttlHours, fallbackIP, webhookURL := s.safeguards()
		timePenaltyStartHour, timePenaltyEndHour, timePenaltyScore, timePenaltyKeywords := s.timePenaltyConfig()
		payload := map[string]interface{}{
			"target_domain":              st.TargetDomain,
			"custom_domain":              st.CustomDomain,
			"probe_source":               st.ProbeSource,
			"carrier":                    st.Carrier,
			"carrier_label":              st.CarrierLabel,
			"ping_mode":                  st.PingMode,
			"ping_port":                  st.PingPort,
			"check_interval":             st.CheckIntervalSec,
			"ping_attempts":              st.PingAttempts,
			"selection_latency_weight":   st.LatencyWeight,
			"selection_jitter_weight":    st.JitterWeight,
			"selection_loss_weight":      st.LossWeight,
			"switch_improvement_percent": st.SwitchImprovement,
			"switch_stable_seconds":      st.SwitchStableSec,
			"failed_orphan_ttl_hours":    ttlHours,
			"fallback_baseline_ip":       fallbackIP,
			"alert_webhook_url":          webhookURL,
			"time_penalty_start_hour":    timePenaltyStartHour,
			"time_penalty_end_hour":      timePenaltyEndHour,
			"time_penalty_score":         timePenaltyScore,
			"time_penalty_org_keywords":  timePenaltyKeywords,
			"cloudflare_api_token_set":   false,
			"cloudflare_zone_id":         "",
			"cloudflare_record_id":       "",
			"agent_controller_url":       "",
			"agent_token_set":            false,
			"agent_report_ttl_seconds":   900,
			"agents":                     []agentPeerPayload{},
			"agent_statuses":             []AgentStatus{},
		}
		if cfg, err := config.Load(s.cfgPath); err == nil {
			payload["cloudflare_api_token_set"] = strings.TrimSpace(cfg.Cloudflare.APIToken) != ""
			payload["cloudflare_zone_id"] = cfg.Cloudflare.ZoneID
			payload["cloudflare_record_id"] = cfg.Cloudflare.RecordID
			payload["agent_controller_url"] = cfg.Agent.ControllerURL
			payload["agent_token_set"] = strings.TrimSpace(cfg.Agent.Token) != ""
			payload["agent_report_ttl_seconds"] = cfg.Agent.ReportTTLSeconds
			payload["agents"] = agentPeersToPayload(cfg.Agents)
			payload["agent_statuses"] = s.AgentStatuses(0)
		}
		writeJSON(w, payload)
		return
	}

	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		TargetDomain         *string             `json:"target_domain"`
		CustomDomain         *string             `json:"custom_domain"`
		ProbeSource          *string             `json:"probe_source"`
		Carrier              *string             `json:"carrier"`
		PingMode             *string             `json:"ping_mode"`
		PingPort             *int                `json:"ping_port"`
		CheckInterval        *int                `json:"check_interval"`
		PingAttempts         *int                `json:"ping_attempts"`
		LatencyWeight        *float64            `json:"selection_latency_weight"`
		JitterWeight         *float64            `json:"selection_jitter_weight"`
		LossWeight           *float64            `json:"selection_loss_weight"`
		SwitchImprovement    *float64            `json:"switch_improvement_percent"`
		SwitchStableSec      *int                `json:"switch_stable_seconds"`
		FailedOrphanTTLHours *int                `json:"failed_orphan_ttl_hours"`
		FallbackBaselineIP   *string             `json:"fallback_baseline_ip"`
		AlertWebhookURL      *string             `json:"alert_webhook_url"`
		TimePenaltyStartHour *int                `json:"time_penalty_start_hour"`
		TimePenaltyEndHour   *int                `json:"time_penalty_end_hour"`
		TimePenaltyScore     *float64            `json:"time_penalty_score"`
		TimePenaltyKeywords  *string             `json:"time_penalty_org_keywords"`
		CloudflareAPIToken   *string             `json:"cloudflare_api_token"`
		CloudflareZoneID     *string             `json:"cloudflare_zone_id"`
		CloudflareRecordID   *string             `json:"cloudflare_record_id"`
		AgentControllerURL   *string             `json:"agent_controller_url"`
		AgentToken           *string             `json:"agent_token"`
		AgentReportTTL       *int                `json:"agent_report_ttl_seconds"`
		Agents               *[]agentPeerPayload `json:"agents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	st := s.status.Load().(*Status)
	multiProfileMode := len(st.Profiles) > 0

	// Build what changed
	var newTarget, newCustom, newProbeSource, newCarrier, newPingMode, newFallbackBaselineIP, newAlertWebhookURL string
	var newPingPort, newCheckInterval, newPingAttempts, newSwitchStableSec int
	var newFailedOrphanTTLHours int
	var newTimePenaltyStartHour, newTimePenaltyEndHour int
	var newLatencyWeight, newJitterWeight, newLossWeight, newSwitchImprovement, newTimePenaltyScore float64
	var newTimePenaltyKeywords string
	var newAgentToken string
	var newAgentControllerURL string
	var newAgentReportTTL int
	var nextAgentPeers []config.AgentPeerConfig
	hasPingPort := false
	hasCheckInterval := false
	hasPingMode := false
	hasCarrier := false
	hasPingAttempts := false
	hasLatencyWeight := false
	hasJitterWeight := false
	hasLossWeight := false
	hasSwitchImprovement := false
	hasSwitchStableSec := false
	hasFailedOrphanTTLHours := false
	hasFallbackBaselineIP := false
	hasAlertWebhookURL := false
	hasTimePenaltyStartHour := false
	hasTimePenaltyEndHour := false
	hasTimePenaltyScore := false
	hasTimePenaltyKeywords := false
	hasCloudflareAPIToken := false
	hasCloudflareZoneID := false
	hasCloudflareRecordID := false
	hasAgentToken := false
	hasAgentControllerURL := false
	hasAgentReportTTL := false
	hasAgentPeers := false

	if !multiProfileMode && body.TargetDomain != nil && *body.TargetDomain != "" && *body.TargetDomain != st.TargetDomain {
		newTarget = *body.TargetDomain
	}
	if !multiProfileMode && body.CustomDomain != nil && *body.CustomDomain != "" && *body.CustomDomain != st.CustomDomain {
		newCustom = *body.CustomDomain
	}
	if !multiProfileMode && body.ProbeSource != nil && *body.ProbeSource != "" && *body.ProbeSource != st.ProbeSource {
		newProbeSource = *body.ProbeSource
	}
	if !multiProfileMode && body.Carrier != nil {
		candidate := config.NormalizeCarrier(*body.Carrier)
		if candidate != st.Carrier {
			newCarrier = candidate
			hasCarrier = true
		}
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
	currentTTLHours, currentFallbackIP, currentWebhookURL := s.safeguards()
	currentTimePenaltyStartHour, currentTimePenaltyEndHour, currentTimePenaltyScore, currentTimePenaltyKeywords := s.timePenaltyConfig()
	cfgForCloudflare, cfgErr := config.Load(s.cfgPath)
	if cfgErr != nil {
		writeJSON(w, map[string]string{"error": "load config: " + cfgErr.Error()})
		return
	}
	nextCloudflare := cfgForCloudflare.Cloudflare
	if body.FailedOrphanTTLHours != nil && *body.FailedOrphanTTLHours >= 0 && *body.FailedOrphanTTLHours != currentTTLHours {
		newFailedOrphanTTLHours = *body.FailedOrphanTTLHours
		hasFailedOrphanTTLHours = true
	}
	if body.FallbackBaselineIP != nil {
		candidate := strings.TrimSpace(*body.FallbackBaselineIP)
		if candidate != currentFallbackIP {
			newFallbackBaselineIP = candidate
			hasFallbackBaselineIP = true
		}
	}
	if body.AlertWebhookURL != nil {
		candidate := strings.TrimSpace(*body.AlertWebhookURL)
		if candidate != currentWebhookURL {
			newAlertWebhookURL = candidate
			hasAlertWebhookURL = true
		}
	}
	if body.TimePenaltyStartHour != nil && *body.TimePenaltyStartHour != currentTimePenaltyStartHour {
		newTimePenaltyStartHour = *body.TimePenaltyStartHour
		hasTimePenaltyStartHour = true
	}
	if body.TimePenaltyEndHour != nil && *body.TimePenaltyEndHour != currentTimePenaltyEndHour {
		newTimePenaltyEndHour = *body.TimePenaltyEndHour
		hasTimePenaltyEndHour = true
	}
	if body.TimePenaltyScore != nil && *body.TimePenaltyScore != currentTimePenaltyScore {
		newTimePenaltyScore = *body.TimePenaltyScore
		hasTimePenaltyScore = true
	}
	if body.TimePenaltyKeywords != nil {
		candidate := strings.TrimSpace(*body.TimePenaltyKeywords)
		if candidate != currentTimePenaltyKeywords {
			newTimePenaltyKeywords = candidate
			hasTimePenaltyKeywords = true
		}
	}
	if body.CloudflareAPIToken != nil {
		candidate := strings.TrimSpace(*body.CloudflareAPIToken)
		if candidate != "" && candidate != nextCloudflare.APIToken {
			nextCloudflare.APIToken = candidate
			hasCloudflareAPIToken = true
		}
	}
	if body.CloudflareZoneID != nil {
		candidate := strings.TrimSpace(*body.CloudflareZoneID)
		if candidate != "" && candidate != nextCloudflare.ZoneID {
			nextCloudflare.ZoneID = candidate
			hasCloudflareZoneID = true
		}
	}
	if body.CloudflareRecordID != nil {
		candidate := strings.TrimSpace(*body.CloudflareRecordID)
		if candidate != "" && candidate != nextCloudflare.RecordID {
			nextCloudflare.RecordID = candidate
			hasCloudflareRecordID = true
		}
	}
	if body.AgentToken != nil {
		candidate := strings.TrimSpace(*body.AgentToken)
		if candidate != "" && candidate != cfgForCloudflare.Agent.Token {
			newAgentToken = candidate
			hasAgentToken = true
		}
	}
	if body.AgentControllerURL != nil {
		candidate := strings.TrimRight(strings.TrimSpace(*body.AgentControllerURL), "/")
		if candidate != "" {
			if _, err := url.ParseRequestURI(candidate); err != nil {
				writeJSON(w, map[string]string{"error": "agent_controller_url is invalid: " + err.Error()})
				return
			}
		}
		if candidate != strings.TrimRight(strings.TrimSpace(cfgForCloudflare.Agent.ControllerURL), "/") {
			newAgentControllerURL = candidate
			hasAgentControllerURL = true
		}
	}
	if body.AgentReportTTL != nil {
		if *body.AgentReportTTL < 30 {
			writeJSON(w, map[string]string{"error": "agent_report_ttl_seconds must be at least 30"})
			return
		}
		if *body.AgentReportTTL != cfgForCloudflare.Agent.ReportTTLSeconds {
			newAgentReportTTL = *body.AgentReportTTL
			hasAgentReportTTL = true
		}
	}
	if body.Agents != nil {
		hasAgentPeers = true
		nextAgentPeers = make([]config.AgentPeerConfig, 0, len(*body.Agents))
		for _, incoming := range *body.Agents {
			nextAgentPeers = append(nextAgentPeers, config.AgentPeerConfig{
				ID:          strings.TrimSpace(incoming.ID),
				Name:        strings.TrimSpace(incoming.Name),
				ProbeSource: strings.TrimSpace(incoming.ProbeSource),
				Carrier:     config.NormalizeCarrier(incoming.Carrier),
			})
		}
	}

	if newTarget == "" && newCustom == "" && newProbeSource == "" && !hasCarrier && !hasPingMode && !hasPingPort && !hasCheckInterval && !hasPingAttempts && !hasLatencyWeight && !hasJitterWeight && !hasLossWeight && !hasSwitchImprovement && !hasSwitchStableSec && !hasFailedOrphanTTLHours && !hasFallbackBaselineIP && !hasAlertWebhookURL && !hasTimePenaltyStartHour && !hasTimePenaltyEndHour && !hasTimePenaltyScore && !hasTimePenaltyKeywords && !hasCloudflareAPIToken && !hasCloudflareZoneID && !hasCloudflareRecordID && !hasAgentToken && !hasAgentControllerURL && !hasAgentReportTTL && !hasAgentPeers {
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
	if hasCarrier {
		if err := config.UpdateYAMLField(s.cfgPath, "carrier", newCarrier, true); err != nil {
			writeJSON(w, map[string]string{"error": "persist carrier: " + err.Error()})
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
	if hasFailedOrphanTTLHours {
		if err := config.UpdateYAMLField(s.cfgPath, "failed_orphan_ttl_hours", fmt.Sprintf("%d", newFailedOrphanTTLHours), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist failed_orphan_ttl_hours: " + err.Error()})
			return
		}
	}
	if hasFallbackBaselineIP {
		if newFallbackBaselineIP != "" && net.ParseIP(newFallbackBaselineIP) == nil {
			writeJSON(w, map[string]string{"error": "fallback_baseline_ip must be a valid IP"})
			return
		}
		if err := config.UpdateYAMLField(s.cfgPath, "fallback_baseline_ip", newFallbackBaselineIP, true); err != nil {
			writeJSON(w, map[string]string{"error": "persist fallback_baseline_ip: " + err.Error()})
			return
		}
	}
	if hasAlertWebhookURL {
		if newAlertWebhookURL != "" {
			if _, err := url.ParseRequestURI(newAlertWebhookURL); err != nil {
				writeJSON(w, map[string]string{"error": "alert_webhook_url is invalid: " + err.Error()})
				return
			}
		}
		if err := config.UpdateYAMLField(s.cfgPath, "alert_webhook_url", newAlertWebhookURL, true); err != nil {
			writeJSON(w, map[string]string{"error": "persist alert_webhook_url: " + err.Error()})
			return
		}
	}
	if hasTimePenaltyStartHour {
		if newTimePenaltyStartHour < 0 || newTimePenaltyStartHour > 23 {
			writeJSON(w, map[string]string{"error": "time_penalty_start_hour must be between 0 and 23"})
			return
		}
		if err := config.UpdateYAMLField(s.cfgPath, "time_penalty_start_hour", fmt.Sprintf("%d", newTimePenaltyStartHour), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist time_penalty_start_hour: " + err.Error()})
			return
		}
	}
	if hasTimePenaltyEndHour {
		if newTimePenaltyEndHour < 0 || newTimePenaltyEndHour > 24 {
			writeJSON(w, map[string]string{"error": "time_penalty_end_hour must be between 0 and 24"})
			return
		}
		if err := config.UpdateYAMLField(s.cfgPath, "time_penalty_end_hour", fmt.Sprintf("%d", newTimePenaltyEndHour), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist time_penalty_end_hour: " + err.Error()})
			return
		}
	}
	if hasTimePenaltyScore {
		if newTimePenaltyScore < 0 {
			writeJSON(w, map[string]string{"error": "time_penalty_score cannot be negative"})
			return
		}
		if err := config.UpdateYAMLField(s.cfgPath, "time_penalty_score", fmt.Sprintf("%.2f", newTimePenaltyScore), false); err != nil {
			writeJSON(w, map[string]string{"error": "persist time_penalty_score: " + err.Error()})
			return
		}
	}
	if hasTimePenaltyKeywords {
		if err := config.UpdateYAMLField(s.cfgPath, "time_penalty_org_keywords", newTimePenaltyKeywords, true); err != nil {
			writeJSON(w, map[string]string{"error": "persist time_penalty_org_keywords: " + err.Error()})
			return
		}
	}
	if hasCloudflareAPIToken || hasCloudflareZoneID || hasCloudflareRecordID || hasAgentToken || hasAgentControllerURL || hasAgentReportTTL || hasAgentPeers {
		nextCfg, err := config.Load(s.cfgPath)
		if err != nil {
			writeJSON(w, map[string]string{"error": "reload config: " + err.Error()})
			return
		}
		if hasCloudflareAPIToken || hasCloudflareZoneID || hasCloudflareRecordID {
			nextCfg.Cloudflare = nextCloudflare
		}
		if hasAgentToken {
			nextCfg.Agent.Token = newAgentToken
		}
		if hasAgentControllerURL {
			nextCfg.Agent.ControllerURL = newAgentControllerURL
		}
		if hasAgentReportTTL {
			nextCfg.Agent.ReportTTLSeconds = newAgentReportTTL
		}
		if hasAgentPeers {
			nextCfg.Agents = nextAgentPeers
		}
		if err := config.Save(s.cfgPath, nextCfg); err != nil {
			writeJSON(w, map[string]string{"error": "persist structured config: " + err.Error()})
			return
		}
		if (hasCloudflareAPIToken || hasCloudflareZoneID || hasCloudflareRecordID) && s.onCloudflare != nil {
			s.onCloudflare(nextCloudflare)
		}
	}

	hasRuntimeConfigChange := newTarget != "" || newCustom != "" || newProbeSource != "" || hasCarrier || hasPingMode || hasPingPort || hasCheckInterval || hasPingAttempts || hasLatencyWeight || hasJitterWeight || hasLossWeight || hasSwitchImprovement || hasSwitchStableSec || hasFailedOrphanTTLHours || hasFallbackBaselineIP || hasAlertWebhookURL || hasTimePenaltyStartHour || hasTimePenaltyEndHour || hasTimePenaltyScore || hasTimePenaltyKeywords

	// Notify main loop
	if s.onConfig != nil && hasRuntimeConfigChange {
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
		finalCarrier := st.Carrier
		if hasCarrier {
			finalCarrier = newCarrier
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
		finalFailedOrphanTTLHours := currentTTLHours
		if hasFailedOrphanTTLHours {
			finalFailedOrphanTTLHours = newFailedOrphanTTLHours
		}
		finalFallbackBaselineIP := currentFallbackIP
		if hasFallbackBaselineIP {
			finalFallbackBaselineIP = newFallbackBaselineIP
		}
		finalAlertWebhookURL := currentWebhookURL
		if hasAlertWebhookURL {
			finalAlertWebhookURL = newAlertWebhookURL
		}
		finalTimePenaltyStartHour := currentTimePenaltyStartHour
		if hasTimePenaltyStartHour {
			finalTimePenaltyStartHour = newTimePenaltyStartHour
		}
		finalTimePenaltyEndHour := currentTimePenaltyEndHour
		if hasTimePenaltyEndHour {
			finalTimePenaltyEndHour = newTimePenaltyEndHour
		}
		finalTimePenaltyScore := currentTimePenaltyScore
		if hasTimePenaltyScore {
			finalTimePenaltyScore = newTimePenaltyScore
		}
		finalTimePenaltyKeywords := currentTimePenaltyKeywords
		if hasTimePenaltyKeywords {
			finalTimePenaltyKeywords = newTimePenaltyKeywords
		}
		s.onConfig(finalTarget, finalCustom, finalProbeSource, finalCarrier, finalPingMode, finalPingPort, finalCheckInterval, finalPingAttempts, finalSwitchStableSec, finalLatencyWeight, finalJitterWeight, finalLossWeight, finalSwitchImprovement, finalFailedOrphanTTLHours, finalFallbackBaselineIP, finalAlertWebhookURL, finalTimePenaltyStartHour, finalTimePenaltyEndHour, finalTimePenaltyScore, finalTimePenaltyKeywords)
	}

	if hasFailedOrphanTTLHours || hasFallbackBaselineIP || hasAlertWebhookURL {
		nextTTLHours := currentTTLHours
		if hasFailedOrphanTTLHours {
			nextTTLHours = newFailedOrphanTTLHours
		}
		nextFallbackIP := currentFallbackIP
		if hasFallbackBaselineIP {
			nextFallbackIP = newFallbackBaselineIP
		}
		nextWebhookURL := currentWebhookURL
		if hasAlertWebhookURL {
			nextWebhookURL = newAlertWebhookURL
		}
		s.SetSafeguards(nextTTLHours, nextFallbackIP, nextWebhookURL)
	}
	if hasTimePenaltyStartHour || hasTimePenaltyEndHour || hasTimePenaltyScore || hasTimePenaltyKeywords {
		nextTimePenaltyStartHour := currentTimePenaltyStartHour
		if hasTimePenaltyStartHour {
			nextTimePenaltyStartHour = newTimePenaltyStartHour
		}
		nextTimePenaltyEndHour := currentTimePenaltyEndHour
		if hasTimePenaltyEndHour {
			nextTimePenaltyEndHour = newTimePenaltyEndHour
		}
		nextTimePenaltyScore := currentTimePenaltyScore
		if hasTimePenaltyScore {
			nextTimePenaltyScore = newTimePenaltyScore
		}
		nextTimePenaltyKeywords := currentTimePenaltyKeywords
		if hasTimePenaltyKeywords {
			nextTimePenaltyKeywords = newTimePenaltyKeywords
		}
		s.SetTimePenaltyConfig(nextTimePenaltyStartHour, nextTimePenaltyEndHour, nextTimePenaltyScore, nextTimePenaltyKeywords)
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
	if hasCarrier {
		st.Carrier = newCarrier
	}
	if hasCarrier || newProbeSource != "" {
		if config.NormalizeCarrier(st.Carrier) == "auto" {
			st.CarrierLabel = config.CarrierLabel(config.InferCarrier(st.ProbeSource)) + "（自动）"
		} else {
			st.CarrierLabel = config.CarrierLabel(st.Carrier)
		}
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
	if hasAgentToken || hasAgentReportTTL || hasAgentPeers {
		st.Agents = s.AgentStatuses(0)
	}
	s.UpdateStatus(st)

	if hasRuntimeConfigChange {
		log.Printf("[config] updated: target_domain=%q custom_domain=%q probe_source=%q carrier=%q ping_mode=%q ping_port=%d check_interval=%d ping_attempts=%d latency_weight=%.2f jitter_weight=%.2f loss_weight=%.2f switch_improvement=%.2f switch_stable_seconds=%d failed_orphan_ttl_hours=%d fallback_baseline_ip=%q alert_webhook_url_set=%t time_penalty=%02d-%02d/+%.2f keywords=%q",
			newTarget, newCustom, newProbeSource, newCarrier, newPingMode, newPingPort, newCheckInterval, newPingAttempts, newLatencyWeight, newJitterWeight, newLossWeight, newSwitchImprovement, newSwitchStableSec, newFailedOrphanTTLHours, newFallbackBaselineIP, newAlertWebhookURL != "", newTimePenaltyStartHour, newTimePenaltyEndHour, newTimePenaltyScore, newTimePenaltyKeywords)
	}
	if hasAgentToken || hasAgentReportTTL || hasAgentPeers {
		agentTTL := cfgForCloudflare.Agent.ReportTTLSeconds
		if hasAgentReportTTL {
			agentTTL = newAgentReportTTL
		}
		agentCount := len(cfgForCloudflare.Agents)
		if hasAgentPeers {
			agentCount = len(nextAgentPeers)
		}
		log.Printf("[config] agent settings updated: token_set=%t report_ttl_seconds=%d agents=%d", strings.TrimSpace(cfgForCloudflare.Agent.Token) != "" || hasAgentToken, agentTTL, agentCount)
	}
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
