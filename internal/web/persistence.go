package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dns-latency-router/internal/checker"
)

const (
	retentionWindow = 7 * 24 * time.Hour
	maxLogEntries   = 2000
	maxHistoryItems = 2000
	maxSampleItems  = 2000
)

type LogEntry struct {
	Time time.Time `json:"time"`
	Line string    `json:"line"`
}

type IPSample struct {
	Time         time.Time `json:"time"`
	AgentID      string    `json:"agentId,omitempty"`
	AgentName    string    `json:"agentName,omitempty"`
	Carrier      string    `json:"carrier,omitempty"`
	CarrierLabel string    `json:"carrierLabel,omitempty"`
	ProbeSource  string    `json:"probeSource,omitempty"`
	ProfileID    string    `json:"profileId,omitempty"`
	ProfileName  string    `json:"profileName,omitempty"`
	Region       string    `json:"region,omitempty"`
	RegionLabel  string    `json:"regionLabel,omitempty"`
	IP           string    `json:"ip"`
	Latency      float64   `json:"latency"`
	Jitter       float64   `json:"jitter"`
	LossRate     float64   `json:"lossRate"`
	Score        float64   `json:"score"`
	Success      bool      `json:"success"`
	Error        string    `json:"error,omitempty"`
}

type IPStat struct {
	IP           string    `json:"ip"`
	AgentID      string    `json:"agentId,omitempty"`
	AgentName    string    `json:"agentName,omitempty"`
	Carrier      string    `json:"carrier,omitempty"`
	CarrierLabel string    `json:"carrierLabel,omitempty"`
	ProbeSource  string    `json:"probeSource,omitempty"`
	ProfileID    string    `json:"profileId,omitempty"`
	ProfileName  string    `json:"profileName,omitempty"`
	Region       string    `json:"region,omitempty"`
	RegionLabel  string    `json:"regionLabel,omitempty"`
	Geo          string    `json:"geo,omitempty"`
	Active       bool      `json:"active"`
	Status       string    `json:"status"`
	SeenCount    int       `json:"seenCount"`
	SuccessCount int       `json:"successCount"`
	SuccessRate  float64   `json:"successRate"`
	AvgLatency   float64   `json:"avgLatency"`
	BestLatency  float64   `json:"bestLatency"`
	LastLatency  float64   `json:"lastLatency"`
	AvgJitter    float64   `json:"avgJitter"`
	AvgLossRate  float64   `json:"avgLossRate"`
	BestScore    float64   `json:"bestScore"`
	LastScore    float64   `json:"lastScore"`
	FirstSeen    time.Time `json:"firstSeen"`
	LastSeen     time.Time `json:"lastSeen"`
	LastActiveAt time.Time `json:"lastActiveAt"`
	LastError    string    `json:"lastError,omitempty"`
}

type IPLifecycle struct {
	AgentID      string
	ProfileID    string
	Region       string
	IP           string
	FirstSeen    time.Time
	LastSeen     time.Time
	LastActiveAt time.Time
}

func sampleKey(agentID, profileID, region, ip string) string {
	return agentID + "|" + profileID + "|" + region + "|" + ip
}

func (s *Server) loadPersistedData() {
	s.logBuf = s.loadLogs()
	s.history = s.loadHistory()
	s.samples = s.loadSamples()
	s.loadAgentReports()
	s.loadActiveIPs()
	s.loadControllerCandidates()
	s.loadIPLifecycles()
	if s.store != nil {
		if err := s.store.prune(); err != nil {
			log.Printf("[store] prune failed: %v", err)
		}
	}
}

func (s *Server) loadLogs() []LogEntry {
	if s.store != nil {
		logs, err := s.store.loadLogs(maxLogEntries)
		if err != nil {
			log.Printf("[store] load logs failed: %v", err)
		}
		if len(logs) == 0 {
			logs = readJSONLines[LogEntry](s.logsPath)
			if len(logs) == 0 {
				logs = s.importPM2Logs()
			}
			logs = pruneLogEntries(logs)
			if len(logs) > 0 {
				if err := s.store.replaceLogs(logs); err != nil {
					log.Printf("[store] import logs failed: %v", err)
				}
			}
		} else {
			logs = pruneLogEntries(logs)
			if err := s.store.replaceLogs(logs); err != nil {
				log.Printf("[store] prune logs failed: %v", err)
			}
		}
		return logs
	}
	logs := readJSONLines[LogEntry](s.logsPath)
	if len(logs) == 0 {
		logs = s.importPM2Logs()
	}
	logs = pruneLogEntries(logs)
	if len(logs) > 0 {
		_ = rewriteJSONLines(s.logsPath, logs)
	}
	return logs
}

func (s *Server) loadHistory() []CheckRecord {
	if s.store != nil {
		hist, err := s.store.loadHistory(maxHistoryItems)
		if err != nil {
			log.Printf("[store] load history failed: %v", err)
		}
		if len(hist) == 0 {
			hist = readJSONArray[CheckRecord](s.historyPath)
			hist = pruneHistory(hist)
			if len(hist) > 0 {
				if err := s.store.replaceHistory(hist); err != nil {
					log.Printf("[store] import history failed: %v", err)
				}
			}
		} else {
			hist = pruneHistory(hist)
			if err := s.store.replaceHistory(hist); err != nil {
				log.Printf("[store] prune history failed: %v", err)
			}
		}
		return hist
	}
	hist := readJSONArray[CheckRecord](s.historyPath)
	hist = pruneHistory(hist)
	if len(hist) > 0 {
		_ = writeJSONArray(s.historyPath, hist)
	}
	return hist
}

func (s *Server) loadSamples() []IPSample {
	if s.store != nil {
		samples, err := s.store.loadSamples(maxSampleItems)
		if err != nil {
			log.Printf("[store] load samples failed: %v", err)
		}
		if len(samples) == 0 {
			samples = readJSONArray[IPSample](s.samplesPath)
			samples = pruneSamples(samples)
			if len(samples) > 0 {
				if err := s.store.replaceSamples(samples); err != nil {
					log.Printf("[store] import samples failed: %v", err)
				}
			}
		} else {
			samples = pruneSamples(samples)
			if err := s.store.replaceSamples(samples); err != nil {
				log.Printf("[store] prune samples failed: %v", err)
			}
		}
		return samples
	}
	samples := readJSONArray[IPSample](s.samplesPath)
	samples = pruneSamples(samples)
	if len(samples) > 0 {
		_ = writeJSONArray(s.samplesPath, samples)
	}
	return samples
}

func (s *Server) loadAgentReports() {
	if s.store == nil {
		return
	}
	reports, err := s.store.loadAgentReports(0)
	if err != nil {
		log.Printf("[store] load agent reports failed: %v", err)
		return
	}
	s.agentReportsMu.Lock()
	for _, report := range reports {
		if report.AgentID == "" {
			continue
		}
		s.agentReports[report.AgentID] = report
	}
	s.agentReportsMu.Unlock()
}

func (s *Server) loadActiveIPs() {
	if s.store == nil {
		return
	}
	active, byProfile, err := s.store.loadActiveIPs()
	if err != nil {
		log.Printf("[store] load active IPs failed: %v", err)
		return
	}
	if len(active) == 0 && len(byProfile) == 0 {
		return
	}
	s.activeIPsMu.Lock()
	s.activeIPs = active
	s.activeIPsByProfile = byProfile
	s.activeIPsMu.Unlock()
}

func (s *Server) loadControllerCandidates() {
	if s.store == nil {
		return
	}
	candidates, err := s.store.loadControllerCandidates()
	if err != nil {
		log.Printf("[store] load controller candidates failed: %v", err)
		return
	}
	if len(candidates) == 0 {
		return
	}
	s.controllerCandidateMu.Lock()
	s.controllerCandidates = candidates
	s.controllerCandidateMu.Unlock()
}

func (s *Server) loadIPLifecycles() {
	records := make(map[string]IPLifecycle)
	if s.store != nil {
		loaded, err := s.store.loadIPLifecycles()
		if err != nil {
			log.Printf("[store] load IP lifecycles failed: %v", err)
		} else {
			records = loaded
		}
	}
	for _, sample := range pruneSamples(readJSONArray[IPSample](s.samplesPath)) {
		mergeIPLifecycle(records, sample)
	}
	for _, sample := range s.samples {
		mergeIPLifecycle(records, sample)
	}
	if s.store != nil && len(records) > 0 {
		if err := s.store.replaceIPLifecycles(records); err != nil {
			log.Printf("[store] seed IP lifecycles failed: %v", err)
		}
	}
	s.ipLifecyclesMu.Lock()
	s.ipLifecycles = records
	s.ipLifecyclesMu.Unlock()
}

func (s *Server) importPM2Logs() []LogEntry {
	baseDir := filepath.Dir(s.cfgPath)
	candidates := []string{
		filepath.Join(baseDir, "logs", "output.log"),
		filepath.Join(baseDir, "logs", "error.log"),
	}

	var imported []LogEntry
	for _, path := range candidates {
		file, err := os.Open(path)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			entry := LogEntry{
				Time: time.Now(),
				Line: line,
			}
			if len(line) >= 25 {
				if ts, err := time.Parse("2006-01-02 15:04:05 Z07:00", line[:25]); err == nil {
					entry.Time = ts
					entry.Line = strings.TrimSpace(strings.TrimPrefix(line[25:], ":"))
				}
			}
			imported = append(imported, entry)
		}
		file.Close()
	}

	sort.Slice(imported, func(i, j int) bool {
		return imported[i].Time.Before(imported[j].Time)
	})
	return imported
}

func pruneLogEntries(logs []LogEntry) []LogEntry {
	cutoff := time.Now().Add(-retentionWindow)
	out := logs[:0]
	for _, entry := range logs {
		if entry.Time.IsZero() || entry.Time.Before(cutoff) {
			continue
		}
		out = append(out, entry)
	}
	if len(out) > maxLogEntries {
		out = out[len(out)-maxLogEntries:]
	}
	return out
}

func pruneHistory(hist []CheckRecord) []CheckRecord {
	cutoff := time.Now().Add(-retentionWindow)
	out := hist[:0]
	for _, rec := range hist {
		if rec.Time.Before(cutoff) {
			continue
		}
		out = append(out, rec)
	}
	if len(out) > maxHistoryItems {
		out = out[len(out)-maxHistoryItems:]
	}
	return out
}

func pruneSamples(samples []IPSample) []IPSample {
	cutoff := time.Now().Add(-retentionWindow)
	out := samples[:0]
	for _, rec := range samples {
		normalized, ok := checker.NormalizeCandidateIP(rec.IP)
		if rec.Time.Before(cutoff) || !ok {
			continue
		}
		rec.IP = normalized
		out = append(out, rec)
	}
	if len(out) > maxSampleItems {
		out = out[len(out)-maxSampleItems:]
	}
	return out
}

func (s *Server) persistLogs() {
	s.logBufMu.Lock()
	logs := make([]LogEntry, len(s.logBuf))
	copy(logs, s.logBuf)
	s.logBufMu.Unlock()
	if s.store != nil {
		if err := s.store.replaceLogs(logs); err != nil {
			log.Printf("[store] persist logs failed: %v", err)
		}
		return
	}
	_ = rewriteJSONLines(s.logsPath, logs)
}

func (s *Server) persistHistory() {
	s.historyMu.Lock()
	hist := make([]CheckRecord, len(s.history))
	copy(hist, s.history)
	s.historyMu.Unlock()
	if s.store != nil {
		if err := s.store.replaceHistory(hist); err != nil {
			log.Printf("[store] persist history failed: %v", err)
		}
		return
	}
	_ = writeJSONArray(s.historyPath, hist)
}

func (s *Server) persistSamples() {
	s.samplesMu.Lock()
	samples := make([]IPSample, len(s.samples))
	copy(samples, s.samples)
	s.samplesMu.Unlock()
	if s.store != nil {
		if err := s.store.replaceSamples(samples); err != nil {
			log.Printf("[store] persist samples failed: %v", err)
		}
		return
	}
	_ = writeJSONArray(s.samplesPath, samples)
}

func buildIPLifecycles(samples []IPSample) map[string]IPLifecycle {
	records := make(map[string]IPLifecycle)
	for _, sample := range samples {
		mergeIPLifecycle(records, sample)
	}
	return records
}

func mergeIPLifecycle(records map[string]IPLifecycle, sample IPSample) {
	if sample.IP == "" || sample.Time.IsZero() {
		return
	}
	key := sampleKey(sample.AgentID, sample.ProfileID, sample.Region, sample.IP)
	rec, ok := records[key]
	if !ok {
		records[key] = IPLifecycle{
			AgentID:      sample.AgentID,
			ProfileID:    sample.ProfileID,
			Region:       sample.Region,
			IP:           sample.IP,
			FirstSeen:    sample.Time,
			LastSeen:     sample.Time,
			LastActiveAt: sample.Time,
		}
		return
	}
	if sample.Time.Before(rec.FirstSeen) || rec.FirstSeen.IsZero() {
		rec.FirstSeen = sample.Time
	}
	if sample.Time.After(rec.LastSeen) {
		rec.LastSeen = sample.Time
	}
	if sample.Time.After(rec.LastActiveAt) {
		rec.LastActiveAt = sample.Time
	}
	records[key] = rec
}

func (s *Server) updateIPLifecycles(samples []IPSample) {
	if len(samples) == 0 {
		return
	}
	s.ipLifecyclesMu.Lock()
	if s.ipLifecycles == nil {
		s.ipLifecycles = make(map[string]IPLifecycle)
	}
	for _, sample := range samples {
		mergeIPLifecycle(s.ipLifecycles, sample)
	}
	snapshot := make(map[string]IPLifecycle, len(s.ipLifecycles))
	for key, rec := range s.ipLifecycles {
		snapshot[key] = rec
	}
	s.ipLifecyclesMu.Unlock()
	if s.store != nil {
		if err := s.store.replaceIPLifecycles(snapshot); err != nil {
			log.Printf("[store] persist IP lifecycles failed: %v", err)
		}
	}
}

func (s *Server) ipLifecycleSnapshot() map[string]IPLifecycle {
	s.ipLifecyclesMu.RLock()
	defer s.ipLifecyclesMu.RUnlock()
	if len(s.ipLifecycles) == 0 {
		return nil
	}
	out := make(map[string]IPLifecycle, len(s.ipLifecycles))
	for key, rec := range s.ipLifecycles {
		out[key] = rec
	}
	return out
}

func (s *Server) pruneInactiveOrphanSamplesLocked() bool {
	ttlHours, _, _ := s.safeguards()
	if ttlHours <= 0 || len(s.samples) == 0 {
		return false
	}

	cutoff := time.Now().Add(-time.Duration(ttlHours) * time.Hour)
	lastByIP := make(map[string]IPSample)
	for _, sample := range s.samples {
		if sample.IP == "" {
			continue
		}
		key := sampleKey(sample.AgentID, sample.ProfileID, sample.Region, sample.IP)
		prev, ok := lastByIP[key]
		if !ok || sample.Time.After(prev.Time) {
			lastByIP[key] = sample
		}
	}

	pruneIPs := make(map[string]struct{})
	prunedList := make([]string, 0)
	for key, sample := range lastByIP {
		if s.isIPActiveInProfile(sample.ProfileID, sample.IP) || sample.Time.After(cutoff) {
			continue
		}
		pruneIPs[key] = struct{}{}
		prunedList = append(prunedList, sample.IP)
	}
	if len(pruneIPs) == 0 {
		return false
	}

	out := s.samples[:0]
	for _, sample := range s.samples {
		if _, drop := pruneIPs[sampleKey(sample.AgentID, sample.ProfileID, sample.Region, sample.IP)]; drop {
			continue
		}
		out = append(out, sample)
	}
	s.samples = out

	s.geoMu.Lock()
	for _, ip := range prunedList {
		delete(s.geoCache, ip)
		delete(s.geoPending, ip)
	}
	s.geoMu.Unlock()

	sort.Strings(prunedList)
	log.Printf("[gc] pruned %d inactive orphaned IP(s) with no refresh for more than %dh: %v", len(prunedList), ttlHours, prunedList)
	return true
}

func (s *Server) computeIPStats() []IPStat {
	assignments := s.agentAssignments()
	lifecycles := s.ipLifecycleSnapshot()
	s.samplesMu.Lock()
	defer s.samplesMu.Unlock()

	statsMap := make(map[string]*IPStat)
	latencySum := make(map[string]float64)
	jitterSum := make(map[string]float64)
	lossRateSum := make(map[string]float64)

	for _, rawSample := range s.samples {
		if !checker.IsUsableCandidateIP(rawSample.IP) {
			continue
		}
		sample := normalizeIPSampleWithAssignments(rawSample, assignments)
		key := sampleKey(sample.AgentID, sample.ProfileID, sample.Region, sample.IP)
		lifecycle := lifecycles[key]
		firstSeen := sample.Time
		if !lifecycle.FirstSeen.IsZero() && lifecycle.FirstSeen.Before(firstSeen) {
			firstSeen = lifecycle.FirstSeen
		}
		stat := statsMap[key]
		if stat == nil {
			stat = &IPStat{
				IP:           sample.IP,
				AgentID:      sample.AgentID,
				AgentName:    sample.AgentName,
				Carrier:      sample.Carrier,
				CarrierLabel: sample.CarrierLabel,
				ProbeSource:  sample.ProbeSource,
				ProfileID:    sample.ProfileID,
				ProfileName:  sample.ProfileName,
				Region:       sample.Region,
				RegionLabel:  sample.RegionLabel,
				BestLatency:  0,
				FirstSeen:    firstSeen,
			}
			statsMap[key] = stat
		}

		stat.SeenCount++
		if firstSeen.Before(stat.FirstSeen) {
			stat.FirstSeen = firstSeen
		}
		if sample.Time.Before(stat.FirstSeen) {
			stat.FirstSeen = sample.Time
		}
		if sample.Time.After(stat.LastSeen) {
			stat.LastSeen = sample.Time
			stat.LastActiveAt = sample.Time
			stat.LastLatency = sample.Latency
			stat.LastScore = sample.Score
			stat.LastError = sample.Error
		}

		lossRateSum[key] += sample.LossRate

		if !sample.Success {
			if sample.Error != "" {
				stat.LastError = sample.Error
			}
			continue
		}

		stat.SuccessCount++
		latencySum[key] += sample.Latency
		jitterSum[key] += sample.Jitter
		if stat.BestLatency == 0 || sample.Latency < stat.BestLatency {
			stat.BestLatency = sample.Latency
		}
		if stat.BestScore == 0 || (sample.Score > 0 && sample.Score < stat.BestScore) {
			stat.BestScore = sample.Score
		}
	}

	stats := make([]IPStat, 0, len(statsMap))
	missingGeo := make([]string, 0, len(statsMap))
	for _, stat := range statsMap {
		stat.Geo = s.geoLabel(stat.IP)
		if stat.Geo == "" {
			missingGeo = append(missingGeo, stat.IP)
			stat.Geo = "未知"
		}
		stat.Active = s.isIPActiveInProfile(stat.ProfileID, stat.IP)
		if stat.Active {
			stat.Status = "active"
		} else {
			stat.Status = "orphaned"
		}
		if stat.SeenCount > 0 {
			stat.SuccessRate = float64(stat.SuccessCount) / float64(stat.SeenCount) * 100
		}
		if stat.SuccessCount > 0 {
			stat.AvgLatency = latencySum[sampleKey(stat.AgentID, stat.ProfileID, stat.Region, stat.IP)] / float64(stat.SuccessCount)
			stat.AvgJitter = jitterSum[sampleKey(stat.AgentID, stat.ProfileID, stat.Region, stat.IP)] / float64(stat.SuccessCount)
		}
		if stat.SeenCount > 0 {
			stat.AvgLossRate = lossRateSum[sampleKey(stat.AgentID, stat.ProfileID, stat.Region, stat.IP)] / float64(stat.SeenCount)
		}
		stats = append(stats, *stat)
	}
	go s.ensureGeoForIPs(missingGeo)

	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Active != stats[j].Active {
			return stats[i].Active
		}
		if stats[i].SeenCount != stats[j].SeenCount {
			return stats[i].SeenCount > stats[j].SeenCount
		}
		if stats[i].AvgLatency == 0 || stats[j].AvgLatency == 0 {
			return stats[i].IP < stats[j].IP
		}
		if stats[i].AvgLatency != stats[j].AvgLatency {
			return stats[i].AvgLatency < stats[j].AvgLatency
		}
		return stats[i].IP < stats[j].IP
	})
	return stats
}

func readJSONArray[T any](path string) []T {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	var out []T
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

func writeJSONArray(path string, v interface{}) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func readJSONLines[T any](path string) []T {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var out []T
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item T
		if err := json.Unmarshal([]byte(line), &item); err == nil {
			out = append(out, item)
		}
	}
	return out
}

func rewriteJSONLines(path string, entries []LogEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	for _, entry := range entries {
		b, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("marshal log entry: %w", err)
		}
		if _, err := writer.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return writer.Flush()
}
