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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dns-latency-router/internal/agent"
	"dns-latency-router/internal/checker"
	"dns-latency-router/internal/config"
	"dns-latency-router/scripts"
)

//go:embed dashboard.html assets/flags/*
var templateFS embed.FS

const agentInstallerPath = "/api/agent/install.sh"

const controllerCandidateCacheTTL = 2 * time.Minute
const controllerCandidateEmptyCacheTTL = 30 * time.Second

const (
	maxAgentReportBodyBytes       = 4 << 20
	maxAgentIDLength              = 128
	maxAgentProfiles              = 256
	maxAgentResolvedIPsPerProfile = 4096
	maxAgentResultsPerProfile     = 4096
)

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
	Version      string    `json:"version,omitempty"`
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
	AgentID        string  `json:"agentId,omitempty"`
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
	traces                *traceManager
	port                  int
	status                atomic.Value
	history               []CheckRecord
	historyMu             sync.Mutex
	samples               []IPSample
	samplesMu             sync.Mutex
	samplePersistMu       sync.Mutex
	ipLifecycles          map[string]IPLifecycle
	ipLifecyclesMu        sync.RWMutex
	sseClients            map[string]chan sseEvent
	sseMu                 sync.Mutex
	sseNextID             int64
	httpServer            *http.Server
	readyCh               chan struct{}
	cfgPath               string
	configMu              sync.Mutex
	onConfigApplied       func(*config.Config)
	triggerCh             chan<- struct{} // signal main loop to run a check immediately
	logBuf                []LogEntry
	logBufMu              sync.Mutex
	store                 *runtimeStore
	dbPath                string
	logsPath              string
	historyPath           string
	samplesPath           string
	activeIPs             map[string]bool
	activeIPsByProfile    map[string]map[string]bool
	activeIPsMu           sync.RWMutex
	controllerCandidates  map[string]controllerCandidateCacheEntry
	controllerRefreshes   map[string]bool
	controllerCandidateMu sync.Mutex
	agentReports          map[string]agent.Report
	agentReportsMu        sync.RWMutex
	geoCache              map[string]GeoInfo
	geoPending            map[string]bool
	geoMu                 sync.RWMutex
	geoClient             *http.Client
	runtimeCfgMu          sync.RWMutex
	failedOrphanTTLHours  int
	fallbackBaselineIP    string
	alertWebhookURL       string
	timePenaltyStartHour  int
	timePenaltyEndHour    int
	timePenaltyScore      float64
	timePenaltyKeywords   string
}

type controllerCandidateCacheEntry struct {
	IPs       []string
	ExpiresAt time.Time
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
func New(port int, cfgPath string, triggerCh chan<- struct{}) *Server {
	s := &Server{
		port:                 port,
		sseClients:           make(map[string]chan sseEvent),
		readyCh:              make(chan struct{}),
		cfgPath:              cfgPath,
		triggerCh:            triggerCh,
		ipLifecycles:         make(map[string]IPLifecycle),
		activeIPs:            make(map[string]bool),
		activeIPsByProfile:   make(map[string]map[string]bool),
		controllerCandidates: make(map[string]controllerCandidateCacheEntry),
		controllerRefreshes:  make(map[string]bool),
		agentReports:         make(map[string]agent.Report),
		geoCache:             make(map[string]GeoInfo),
		geoPending:           make(map[string]bool),
		geoClient:            &http.Client{Timeout: 3 * time.Second},
	}
	stateDir := filepath.Join(filepath.Dir(cfgPath), "data")
	s.dbPath = filepath.Join(stateDir, "runtime.db")
	s.logsPath = filepath.Join(stateDir, "runtime-logs.jsonl")
	s.historyPath = filepath.Join(stateDir, "runtime-history.json")
	s.samplesPath = filepath.Join(stateDir, "runtime-samples.json")
	if store, err := openRuntimeStore(s.dbPath); err == nil {
		s.store = store
	} else {
		log.Printf("[store] sqlite unavailable, falling back to legacy JSON runtime files: %v", err)
	}
	s.loadPersistedData()
	s.ensureGeoForSamples()
	s.status.Store(&Status{CheckIntervalSec: 300})
	s.initMTR()
	return s
}

// Start begins the HTTP server in a goroutine. Returns immediately.
func (s *Server) Start() {
	mux := http.NewServeMux()
	mux.Handle("/assets/", http.FileServer(http.FS(templateFS)))
	mux.HandleFunc("/console", s.redirectLegacyConsole)
	mux.HandleFunc("/console/", s.redirectLegacyConsole)
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/mtr", s.handleAPIMTR)
	mux.HandleFunc("/api/agent/mtr", s.handleAPIAgentMTR)
	mux.HandleFunc("/api/agent/nexttrace/", s.handleAPINextTraceDownload)
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
	mux.HandleFunc("/api/agent/download/", s.handleAPIAgentDownload)
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
	if s.traces != nil && s.traces.local != nil {
		s.traces.local.Close()
	}
	if s.httpServer != nil {
		s.httpServer.Close()
	}
	if s.store != nil {
		s.store.close()
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
	if strings.TrimSpace(proxyURL) != "" {
		log.Printf("[geo] proxy_url is ignored for IP geo lookup; using direct connection")
	}
	s.geoMu.Lock()
	s.geoClient = &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{}}
	s.geoMu.Unlock()
}

// SetConfigCallback must be called before Start. The callback receives a complete
// configuration and must queue it for application between probe cycles.
func (s *Server) SetConfigCallback(cb func(*config.Config)) {
	s.onConfigApplied = cb
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

func cloneStatus(st *Status) *Status {
	if st == nil {
		return &Status{}
	}
	next := *st
	next.Agents = append([]AgentStatus(nil), st.Agents...)
	next.Profiles = append([]ProfileStatus(nil), st.Profiles...)
	for i := range next.Profiles {
		next.Profiles[i].TargetDomains = append([]string(nil), st.Profiles[i].TargetDomains...)
		next.Profiles[i].Regions = append([]RegionStatus(nil), st.Profiles[i].Regions...)
	}
	return &next
}

// GetStatus returns an independent snapshot, including nested slices.
func (s *Server) GetStatus() *Status {
	st, _ := s.status.Load().(*Status)
	return cloneStatus(st)
}

// UpdateStatus publishes an immutable copy so callers can reuse their snapshot.
func (s *Server) UpdateStatus(st *Status) {
	next := cloneStatus(st)
	s.status.Store(next)
	s.broadcast("status", mustJSON(next))
}

// AddHistory appends a check record and broadcasts the full history.
func (s *Server) AddHistory(rec CheckRecord) {
	s.historyMu.Lock()
	s.history = append(s.history, rec)
	s.history = pruneHistory(s.history)
	hist := recentHistory(s.history, maxHistoryAPIItems)
	s.historyMu.Unlock()
	s.persistHistoryRecord(rec)
	s.broadcast("history", mustJSON(hist))
}

// AddLog broadcasts a log line via SSE and stores it in the buffer.
func (s *Server) AddLog(line string) {
	s.logBufMu.Lock()
	entry := LogEntry{Time: time.Now(), Line: line}
	s.logBuf = append(s.logBuf, entry)
	s.logBuf = pruneLogEntries(s.logBuf)
	s.logBufMu.Unlock()
	s.persistLogEntry(entry)
	s.broadcast("log", mustJSON(entry))
}

// AddSamples stores per-IP latency samples for long-term stats.
func (s *Server) AddSamples(samples []IPSample) {
	samples = filterUsableSamples(samples)
	if len(samples) == 0 {
		return
	}
	s.samplePersistMu.Lock()
	s.updateIPLifecycles(samples)
	s.samplesMu.Lock()
	s.samples = append(s.samples, samples...)
	s.samples = pruneSamples(s.samples)
	pruned := s.pruneInactiveOrphanSamplesLocked()
	s.samplesMu.Unlock()
	s.persistSampleBatch(samples, pruned)
	s.samplePersistMu.Unlock()
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
	return checker.IsUsableCandidateIP(ip)
}

func (s *Server) UpdateResolvedIPs(ips []string) {
	s.UpdateResolvedIPsForProfile("", ips)
}

func (s *Server) UpdateResolvedIPsForProfile(profileID string, ips []string) {
	next := make(map[string]bool, len(ips))
	for _, ip := range ips {
		if normalized, ok := checker.NormalizeCandidateIP(ip); ok {
			next[normalized] = true
		}
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
	if s.store != nil {
		if err := s.store.replaceActiveIPs(profileID, next); err != nil {
			log.Printf("[store] persist active IPs failed: %v", err)
		}
	}
}

func (s *Server) candidateIPsForProfile(profileID string) []string {
	seen := make(map[string]struct{})
	add := func(value string) {
		ip, ok := checker.NormalizeCandidateIP(value)
		if !ok {
			return
		}
		seen[ip] = struct{}{}
	}

	// Keep the in-use route measurable even if a DNS response temporarily
	// omits it. Remote agents must measure their own path, not the controller's.
	status := s.GetStatus()
	if len(status.Profiles) == 0 {
		add(status.CurrentIP)
	}
	for _, profile := range status.Profiles {
		if profileID != "" && profile.ID != profileID {
			continue
		}
		for _, region := range profile.Regions {
			add(region.CurrentIP)
		}
	}
	currentIPs := make(map[string]bool, len(seen))
	for ip := range seen {
		currentIPs[ip] = true
	}
	s.activeIPsMu.RLock()
	if profileID != "" {
		for ip := range s.activeIPsByProfile[profileID] {
			add(ip)
		}
	} else {
		for ip := range s.activeIPs {
			add(ip)
		}
	}
	s.activeIPsMu.RUnlock()

	s.agentReportsMu.RLock()
	for _, report := range s.agentReports {
		for _, profile := range report.Profiles {
			if profileID != "" && profile.ProfileID != profileID {
				continue
			}
			for _, ip := range profile.ResolvedIPs {
				add(ip)
			}
			for _, result := range profile.Results {
				add(result.IP)
			}
		}
	}
	s.agentReportsMu.RUnlock()

	cutoff := time.Now().Add(-24 * time.Hour)
	s.samplesMu.Lock()
	for _, sample := range s.samples {
		if profileID != "" && sample.ProfileID != profileID {
			continue
		}
		if !sample.Time.IsZero() && sample.Time.Before(cutoff) {
			continue
		}
		add(sample.IP)
	}
	s.samplesMu.Unlock()

	ips := make([]string, 0, len(seen))
	for ip := range seen {
		ips = append(ips, ip)
	}
	if len(ips) > 128 {
		// Reserve slots for every in-use route before limiting historical
		// candidates, so agents can still detect failure after DNS omits it.
		sort.Slice(ips, func(i, j int) bool {
			if currentIPs[ips[i]] != currentIPs[ips[j]] {
				return currentIPs[ips[i]]
			}
			return ips[i] < ips[j]
		})
		ips = ips[:max(128, len(currentIPs))]
	}
	sort.Strings(ips)
	return ips
}

func (s *Server) controllerCandidateIPsForProfile(cfg *config.Config, profile config.AirportProfile) []string {
	if cfg == nil {
		return nil
	}
	targets := profileTargetDomains(profile)
	if len(targets) == 0 {
		return nil
	}
	servers := controllerResolveDNSServers(cfg)
	if len(servers) == 0 {
		return nil
	}
	key := controllerCandidateCacheKey(profile.ID, targets, servers)
	now := time.Now()

	s.controllerCandidateMu.Lock()
	if entry, ok := s.controllerCandidates[key]; ok && now.Before(entry.ExpiresAt) {
		ips := append([]string(nil), entry.IPs...)
		s.controllerCandidateMu.Unlock()
		return ips
	}
	var stale []string
	if entry, ok := s.controllerCandidates[key]; ok {
		stale = append([]string(nil), entry.IPs...)
	}
	if !s.controllerRefreshes[key] {
		s.controllerRefreshes[key] = true
		go s.refreshControllerCandidateIPs(profile.ID, key, append([]string(nil), targets...), append([]string(nil), servers...))
	}
	s.controllerCandidateMu.Unlock()
	if s.store != nil {
		entry, ok, err := s.store.loadControllerCandidate(key)
		if err != nil {
			log.Printf("[store] load controller candidates failed: %v", err)
		}
		if ok {
			s.controllerCandidateMu.Lock()
			s.controllerCandidates[key] = entry
			s.controllerCandidateMu.Unlock()
			if len(stale) == 0 {
				stale = append([]string(nil), entry.IPs...)
			}
			if now.Before(entry.ExpiresAt) {
				return append([]string(nil), entry.IPs...)
			}
		}
	}
	return stale
}

func (s *Server) refreshControllerCandidateIPs(profileID, key string, targets, servers []string) {
	resolved := make([]string, 0)
	for _, domain := range targets {
		ips, err := checker.ResolveFromAllDNS(domain, servers)
		if err != nil {
			log.Printf("[agent] [%s] controller DNS resolve failed for %s: %v", profileID, domain, err)
			continue
		}
		resolved = append(resolved, ips...)
	}
	resolved = checker.FilterUsableCandidateIPs(resolved)
	sort.Strings(resolved)

	ttl := controllerCandidateCacheTTL
	if len(resolved) == 0 {
		ttl = controllerCandidateEmptyCacheTTL
	} else {
		log.Printf("[agent] [%s] controller resolved %d candidate IP(s) from %d DNS server(s): %v", profileID, len(resolved), len(servers), resolved)
	}
	s.controllerCandidateMu.Lock()
	s.controllerCandidates[key] = controllerCandidateCacheEntry{
		IPs:       append([]string(nil), resolved...),
		ExpiresAt: time.Now().Add(ttl),
	}
	entry := s.controllerCandidates[key]
	delete(s.controllerRefreshes, key)
	s.controllerCandidateMu.Unlock()
	if s.store != nil {
		if err := s.store.upsertControllerCandidates(profileID, key, entry); err != nil {
			log.Printf("[store] persist controller candidates failed: %v", err)
		}
	}
}

func profileTargetDomains(profile config.AirportProfile) []string {
	targets := append([]string(nil), profile.TargetDomains...)
	if strings.TrimSpace(profile.TargetDomain) != "" {
		targets = append([]string{profile.TargetDomain}, targets...)
	}
	return dedupeNonEmptyStrings(targets)
}

func controllerResolveDNSServers(cfg *config.Config) []string {
	servers := make([]string, 0)
	for _, carrier := range []string{"unicom", "telecom", "mobile", "all"} {
		servers = append(servers, cfg.EffectiveDNSServersFor(carrier, "")...)
	}
	return dedupeNonEmptyStrings(servers)
}

func controllerCandidateCacheKey(profileID string, targets, servers []string) string {
	parts := []string{
		strings.ToLower(strings.TrimSpace(profileID)),
		strings.Join(targets, ","),
		strings.Join(servers, ","),
	}
	return strings.Join(parts, "|")
}

func mergeCandidateIPLists(lists ...[]string) []string {
	ips := make([]string, 0)
	for _, list := range lists {
		ips = append(ips, list...)
	}
	ips = checker.FilterUsableCandidateIPs(ips)
	sort.Strings(ips)
	return ips
}

func dedupeNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func filterUsableSamples(samples []IPSample) []IPSample {
	if len(samples) == 0 {
		return nil
	}
	out := samples[:0]
	for _, sample := range samples {
		normalized, ok := checker.NormalizeCandidateIP(sample.IP)
		if !ok {
			continue
		}
		sample.IP = normalized
		out = append(out, sample)
	}
	return out
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
	targets := make([]string, 0, len(ips))
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

		targets = append(targets, ip)
	}
	if len(targets) == 0 {
		return
	}
	go func(items []string) {
		for i, target := range items {
			info := s.fetchGeoInfo(target)
			s.geoMu.Lock()
			delete(s.geoPending, target)
			if geoInfoUseful(info) {
				s.geoCache[target] = info
			}
			s.geoMu.Unlock()
			if i < len(items)-1 {
				time.Sleep(600 * time.Millisecond)
			}
		}
	}(targets)
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
			sep := " "
			if containsCJK(location) || containsCJK(city) {
				sep = ""
			}
			location += sep + city
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

func containsCJK(s string) bool {
	for _, r := range s {
		if (r >= '\u4e00' && r <= '\u9fff') || (r >= '\u3400' && r <= '\u4dbf') {
			return true
		}
	}
	return false
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
	providers := []func(string) GeoInfo{
		s.fetchGeoInfoFromIPSB,
		s.fetchGeoInfoFromIPAPI,
		s.fetchGeoInfoFromIPWhoIs,
		s.fetchGeoInfoFromIPAPICo,
	}
	for _, provider := range providers {
		info := provider(ip)
		if geoInfoUseful(info) {
			return info
		}
	}
	return GeoInfo{IP: ip}
}

func (s *Server) fetchGeoInfoFromIPAPI(ip string) GeoInfo {
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

func (s *Server) fetchGeoInfoFromIPWhoIs(ip string) GeoInfo {
	info := GeoInfo{IP: ip}
	url := fmt.Sprintf("https://ipwho.is/%s?lang=zh-CN", ip)
	resp, err := s.geoHTTPClient().Get(url)
	if err != nil {
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info
	}

	var payload struct {
		Success     bool   `json:"success"`
		CountryCode string `json:"country_code"`
		Country     string `json:"country"`
		City        string `json:"city"`
		Connection  struct {
			ISP string `json:"isp"`
			Org string `json:"org"`
		} `json:"connection"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return info
	}
	if !payload.Success {
		return info
	}

	info.CountryCode = strings.TrimSpace(payload.CountryCode)
	info.Country = strings.TrimSpace(payload.Country)
	info.City = strings.TrimSpace(payload.City)
	info.ISP = strings.TrimSpace(payload.Connection.ISP)
	if info.ISP == "" {
		info.ISP = strings.TrimSpace(payload.Connection.Org)
	}
	info.Label = compactGeoLabel(info.Country, info.City, info.ISP)
	return info
}

func (s *Server) fetchGeoInfoFromIPSB(ip string) GeoInfo {
	info := GeoInfo{IP: ip}
	url := fmt.Sprintf("https://api.ip.sb/geoip/%s", ip)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return info
	}
	req.Header.Set("User-Agent", "dns-latency-router")
	resp, err := s.geoHTTPClient().Do(req)
	if err != nil {
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info
	}

	var payload struct {
		CountryCode     string `json:"country_code"`
		Country         string `json:"country"`
		City            string `json:"city"`
		Region          string `json:"region"`
		ISP             string `json:"isp"`
		Organization    string `json:"organization"`
		ASNOrganization string `json:"asn_organization"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return info
	}

	info.CountryCode = strings.TrimSpace(payload.CountryCode)
	info.Country = strings.TrimSpace(payload.Country)
	info.City = strings.TrimSpace(payload.City)
	if info.City == "" {
		info.City = strings.TrimSpace(payload.Region)
	}
	info.ISP = strings.TrimSpace(payload.ISP)
	if info.ISP == "" {
		info.ISP = strings.TrimSpace(payload.Organization)
	}
	if info.ISP == "" {
		info.ISP = strings.TrimSpace(payload.ASNOrganization)
	}
	info.Label = compactGeoLabel(info.Country, info.City, info.ISP)
	return info
}

func (s *Server) fetchGeoInfoFromIPAPICo(ip string) GeoInfo {
	info := GeoInfo{IP: ip}
	url := fmt.Sprintf("https://ipapi.co/%s/json/", ip)
	resp, err := s.geoHTTPClient().Get(url)
	if err != nil {
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info
	}

	var payload struct {
		Error       bool   `json:"error"`
		CountryCode string `json:"country_code"`
		CountryName string `json:"country_name"`
		City        string `json:"city"`
		Org         string `json:"org"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return info
	}
	if payload.Error {
		return info
	}

	info.CountryCode = strings.TrimSpace(payload.CountryCode)
	info.Country = strings.TrimSpace(payload.CountryName)
	info.City = strings.TrimSpace(payload.City)
	info.ISP = strings.TrimSpace(payload.Org)
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

// Preserve bookmarks after consolidating the UI into the dashboard.
func (s *Server) redirectLegacyConsole(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/console" && r.URL.Path != "/console/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusMovedPermanently)
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	st := s.GetStatus()
	st.Agents = s.AgentStatuses(0)
	writeJSON(w, st)
}

func (s *Server) handleAPIHistory(w http.ResponseWriter, r *http.Request) {
	s.historyMu.Lock()
	hist := recentHistory(s.history, maxHistoryAPIItems)
	s.historyMu.Unlock()
	writeJSON(w, hist)
}

func (s *Server) handleAPIIPStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.computeIPStats())
}

func (s *Server) handleAPIIPSamples(w http.ResponseWriter, r *http.Request) {
	assignments := s.agentAssignments()
	q := r.URL.Query()
	wantAgent := strings.TrimSpace(q.Get("agent_id"))
	wantProfile := strings.TrimSpace(q.Get("profile_id"))
	wantIP := strings.TrimSpace(q.Get("ip"))
	if normalized, ok := checker.NormalizeCandidateIP(wantIP); ok {
		wantIP = normalized
	}
	limit := maxSampleAPIItems
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			if n <= 0 {
				limit = 0
			} else if n < limit {
				limit = n
			}
		}
	}
	s.samplesMu.Lock()
	samples := make([]IPSample, 0, len(s.samples))
	for _, sample := range s.samples {
		if wantAgent != "" && sample.AgentID != wantAgent {
			continue
		}
		if wantProfile != "" && sample.ProfileID != wantProfile {
			continue
		}
		if wantIP != "" && sample.IP != wantIP {
			continue
		}
		samples = append(samples, sample)
	}
	s.samplesMu.Unlock()
	for i := range samples {
		samples[i] = normalizeIPSampleWithAssignments(samples[i], assignments)
	}
	samples = filterUsableSamples(samples)
	samples = recentSamples(samples, limit)
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
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, strings.ReplaceAll(string(scripts.AgentInstaller), "\r\n", "\n"))
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
		"script_url":     controllerURL + agentInstallerPath,
		"controller_url": controllerURL,
		"command":        buildAgentInstallCommand(controllerURL, token, cfg.CheckIntervalSec),
	})
}

func (s *Server) handleAPIAgentDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	platform := strings.TrimPrefix(r.URL.Path, "/api/agent/download/")
	if platform == "" || platform == r.URL.Path {
		platform = "linux-amd64"
	}
	path, ok := findAgentBinary(platform)
	if !ok {
		http.Error(w, "agent binary not found on controller; place dns-latency-router-agent-"+platform+" next to controller binary", http.StatusNotFound)
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

func findAgentBinary(platform string) (string, bool) {
	platform = strings.TrimSpace(platform)
	names := []string{
		"dns-latency-router-agent-" + platform,
	}
	if platform == "linux-amd64" {
		names = append(names,
			"dns-latency-router-agent",
			"dns-latency-router-agent.exe",
		)
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

type agentAssignment struct {
	Name        string
	ProbeSource string
	Carrier     string
}

func (s *Server) agentAssignments() map[string]agentAssignment {
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		return nil
	}
	assignments := make(map[string]agentAssignment, len(cfg.Agents))
	for _, peer := range cfg.Agents {
		id := strings.ToLower(strings.TrimSpace(peer.ID))
		if id == "" || id == "controller" {
			continue
		}
		assignments[id] = agentAssignment{
			Name:        strings.TrimSpace(peer.Name),
			ProbeSource: strings.TrimSpace(peer.ProbeSource),
			Carrier:     concreteAgentCarrier(peer.Carrier),
		}
	}
	return assignments
}

func normalizeAgentReportWithAssignments(report agent.Report, assignments map[string]agentAssignment) agent.Report {
	id := strings.ToLower(strings.TrimSpace(report.AgentID))
	assignment, ok := assignments[id]
	if ok {
		if assignment.Name != "" {
			report.AgentName = assignment.Name
		}
		if assignment.ProbeSource != "" {
			report.ProbeSource = assignment.ProbeSource
		}
	}
	if ok && assignment.Carrier != "" {
		report.Carrier = assignment.Carrier
		report.CarrierLabel = config.CarrierLabel(assignment.Carrier)
		return report
	}
	report.Carrier = ""
	report.CarrierLabel = agentCarrierLabel("")
	return report
}

func validateAgentReport(report agent.Report, cfg *config.Config) (agent.Report, error) {
	if cfg == nil {
		return report, fmt.Errorf("server config is unavailable")
	}
	report.AgentID = strings.TrimSpace(report.AgentID)
	if report.AgentID == "" {
		return report, fmt.Errorf("agentId is required")
	}
	if len(report.AgentID) > maxAgentIDLength {
		return report, fmt.Errorf("agentId is too long")
	}
	if strings.EqualFold(report.AgentID, "controller") {
		return report, fmt.Errorf("agentId is reserved")
	}
	if len(report.Profiles) > maxAgentProfiles {
		return report, fmt.Errorf("too many profiles")
	}

	allowed := make(map[string]string)
	profiles := cfg.AirportProfiles
	if len(profiles) == 0 && cfg.TargetDomain != "" {
		profiles = []config.AirportProfile{cfg.LegacyProfile()}
	}
	for _, profile := range profiles {
		id := strings.TrimSpace(profile.ID)
		if id != "" {
			allowed[strings.ToLower(id)] = id
		}
	}
	seenProfiles := make(map[string]struct{}, len(report.Profiles))
	for profileIdx := range report.Profiles {
		profile := &report.Profiles[profileIdx]
		key := strings.ToLower(strings.TrimSpace(profile.ProfileID))
		canonicalID, ok := allowed[key]
		if !ok {
			return report, fmt.Errorf("profileId %q is not configured", profile.ProfileID)
		}
		if _, duplicate := seenProfiles[key]; duplicate {
			return report, fmt.Errorf("profileId %q is duplicated", profile.ProfileID)
		}
		seenProfiles[key] = struct{}{}
		profile.ProfileID = canonicalID
		if len(profile.ResolvedIPs) > maxAgentResolvedIPsPerProfile {
			return report, fmt.Errorf("too many resolved IPs for profile %q", canonicalID)
		}
		resolved := make(map[string]struct{}, len(profile.ResolvedIPs))
		normalizedResolved := make([]string, 0, len(profile.ResolvedIPs))
		for _, rawIP := range profile.ResolvedIPs {
			ip, ok := checker.NormalizeCandidateIP(rawIP)
			if !ok {
				return report, fmt.Errorf("resolved IP %q is unusable", rawIP)
			}
			if _, duplicate := resolved[ip]; duplicate {
				return report, fmt.Errorf("resolved IP %q is duplicated", ip)
			}
			resolved[ip] = struct{}{}
			normalizedResolved = append(normalizedResolved, ip)
		}
		profile.ResolvedIPs = normalizedResolved
		if len(profile.Results) > maxAgentResultsPerProfile {
			return report, fmt.Errorf("too many results for profile %q", canonicalID)
		}
		seenResults := make(map[string]struct{}, len(profile.Results))
		results := profile.Results[:0]
		for _, result := range profile.Results {
			ip, ok := checker.NormalizeCandidateIP(result.IP)
			if !ok {
				return report, fmt.Errorf("result contains unusable IP")
			}
			if _, ok := resolved[ip]; !ok {
				return report, fmt.Errorf("result IP %q is not resolved for profile %q", ip, canonicalID)
			}
			if _, duplicate := seenResults[ip]; duplicate {
				return report, fmt.Errorf("result IP %q is duplicated", ip)
			}
			if err := agent.ValidateResult(result); err != nil {
				return report, fmt.Errorf("result %q: %w", ip, err)
			}
			result.Score = checker.Score(result.Latency, result.Jitter, result.LossRate, cfg.LatencyWeight, cfg.JitterWeight, cfg.LossWeight)
			if err := agent.ValidateResult(result); err != nil {
				return report, fmt.Errorf("recomputed result %q: %w", ip, err)
			}
			result.IP = ip
			seenResults[ip] = struct{}{}
			results = append(results, result)
		}
		profile.Results = results
	}
	return report, nil
}

func normalizeIPSampleWithAssignments(sample IPSample, assignments map[string]agentAssignment) IPSample {
	if strings.TrimSpace(sample.AgentID) == "" {
		return sample
	}
	id := strings.ToLower(strings.TrimSpace(sample.AgentID))
	assignment, ok := assignments[id]
	if ok {
		if assignment.Name != "" {
			sample.AgentName = assignment.Name
		}
		if assignment.ProbeSource != "" {
			sample.ProbeSource = assignment.ProbeSource
		}
	}
	carrier := ""
	if ok {
		carrier = assignment.Carrier
	}
	sample.Carrier = carrier
	sample.CarrierLabel = agentCarrierLabel(carrier)
	sample.Region = "agent-unknown"
	if carrier != "" {
		sample.Region = "carrier-" + carrier
	}
	sample.RegionLabel = sample.CarrierLabel
	return sample
}

func buildAgentInstallCommand(controllerURL, token string, interval int) string {
	if interval <= 0 {
		interval = 300
	}
	parts := []string{
		"curl -fsSL",
		shellQuote(strings.TrimRight(controllerURL, "/") + agentInstallerPath),
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
		candidateIPs := mergeCandidateIPLists(
			s.controllerCandidateIPsForProfile(cfg, profile),
			s.candidateIPsForProfile(profile.ID),
		)
		jobs = append(jobs, agent.ProfileJob{
			ID:            profile.ID,
			Name:          profile.Name,
			Slug:          profile.Slug,
			TargetDomains: append([]string(nil), profile.TargetDomains...),
			CandidateIPs:  candidateIPs,
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
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		http.Error(w, "load config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentReportBodyBytes)
	var report agent.Report
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&report); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			http.Error(w, "invalid JSON: multiple values", http.StatusBadRequest)
		} else {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		}
		return
	}
	if headerID := strings.TrimSpace(r.Header.Get("X-Agent-ID")); headerID != "" && !strings.EqualFold(headerID, strings.TrimSpace(report.AgentID)) {
		http.Error(w, "agentId does not match X-Agent-ID", http.StatusBadRequest)
		return
	}
	report = normalizeAgentReportWithAssignments(report, s.agentAssignments())
	if report, err = validateAgentReport(report, cfg); err != nil {
		http.Error(w, "invalid agent report: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Never trust a client-supplied receipt timestamp.
	report.ReceivedAt = time.Now()
	if report.FinishedAt.IsZero() {
		report.FinishedAt = time.Now()
	}
	if report.StartedAt.IsZero() {
		report.StartedAt = report.FinishedAt
	}

	s.agentReportsMu.Lock()
	s.agentReports[report.AgentID] = report
	s.agentReportsMu.Unlock()
	if s.store != nil {
		if err := s.store.upsertAgentReport(report); err != nil {
			log.Printf("[store] persist agent report failed: %v", err)
		}
	}

	for _, profile := range report.Profiles {
		if len(profile.ResolvedIPs) > 0 {
			s.UpdateResolvedIPsForProfile(profile.ProfileID, profile.ResolvedIPs)
		}
	}
	s.AddSamples(agentSamplesFromReport(report))

	st := s.GetStatus()
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
	now := time.Now()
	carrier := concreteAgentCarrier(report.Carrier)
	carrierLabel := agentCarrierLabel(carrier)
	region := "agent-unknown"
	if carrier != "" {
		region = "carrier-" + carrier
	}
	for _, profile := range report.Profiles {
		for _, result := range profile.Results {
			if !checker.IsUsableCandidateIP(result.IP) {
				continue
			}
			sampleTime := result.ObservedAt
			if sampleTime.IsZero() {
				sampleTime = profile.FinishedAt
			}
			if sampleTime.IsZero() {
				sampleTime = report.FinishedAt
			}
			if sampleTime.IsZero() {
				sampleTime = report.ReceivedAt
			}
			if sampleTime.IsZero() {
				sampleTime = now
			}
			if sampleTime.After(now) {
				sampleTime = report.ReceivedAt
				if sampleTime.IsZero() || sampleTime.After(now) {
					sampleTime = now
				}
			}
			sample := IPSample{
				Time:         sampleTime,
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
	filterTTL := ttl
	if filterTTL == 0 {
		ttl = 15 * time.Minute
		filterTTL = ttl
	}
	assignments := s.agentAssignments()
	if s.store != nil {
		reports, err := s.store.loadAgentReports(filterTTL)
		if err != nil {
			log.Printf("[store] load agent reports failed: %v", err)
		} else {
			out := make([]agent.Report, 0, len(reports))
			for _, report := range reports {
				out = append(out, normalizeAgentReportWithAssignments(report, assignments))
			}
			return out
		}
	}
	now := time.Now()
	var cutoff time.Time
	if filterTTL > 0 {
		cutoff = now.Add(-filterTTL)
	}
	s.agentReportsMu.RLock()
	defer s.agentReportsMu.RUnlock()
	reports := make([]agent.Report, 0, len(s.agentReports))
	for _, report := range s.agentReports {
		if stamp := report.FreshnessTime(); stamp.IsZero() || stamp.After(now) || (!cutoff.IsZero() && stamp.Before(cutoff)) {
			continue
		}
		reports = append(reports, normalizeAgentReportWithAssignments(report, assignments))
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
	for _, report := range s.AgentReports(-1) {
		id := strings.TrimSpace(report.AgentID)
		if id == "" {
			continue
		}
		status := statusesByID[id]
		status.ID = id
		status.Version = strings.TrimSpace(report.Version)
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
		status.LastSeen = report.FreshnessTime()
		status.ProfileCount = len(report.Profiles)
		status.AgeSeconds = 0
		status.Status = "offline"
		if !report.FreshnessTime().IsZero() {
			status.AgeSeconds = int(now.Sub(report.FreshnessTime()).Seconds())
			if status.AgeSeconds < 0 {
				status.AgeSeconds = 0
			}
			status.Status = "online"
		}
		if !report.FreshnessTime().IsZero() && report.FreshnessTime().Before(cutoff) {
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
	st := s.GetStatus()
	fmt.Fprintf(w, "event: status\ndata: %s\n\n", mustJSON(st))

	// Send initial history
	s.historyMu.Lock()
	hist := recentHistory(s.history, maxHistoryAPIItems)
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
