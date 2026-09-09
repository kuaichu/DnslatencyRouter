package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dns-latency-router/internal/agent"

	_ "modernc.org/sqlite"
)

type runtimeStore struct {
	db *sql.DB
}

func openRuntimeStore(path string) (*runtimeStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &runtimeStore{db: db}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *runtimeStore) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS runtime_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time TEXT NOT NULL,
			line TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS check_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time TEXT NOT NULL,
			profile_id TEXT NOT NULL DEFAULT '',
			region TEXT NOT NULL DEFAULT '',
			ip TEXT NOT NULL DEFAULT '',
			latency REAL NOT NULL DEFAULT 0,
			success INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS ip_samples (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time TEXT NOT NULL,
			agent_id TEXT NOT NULL DEFAULT '',
			agent_name TEXT NOT NULL DEFAULT '',
			carrier TEXT NOT NULL DEFAULT '',
			carrier_label TEXT NOT NULL DEFAULT '',
			probe_source TEXT NOT NULL DEFAULT '',
			profile_id TEXT NOT NULL DEFAULT '',
			profile_name TEXT NOT NULL DEFAULT '',
			region TEXT NOT NULL DEFAULT '',
			region_label TEXT NOT NULL DEFAULT '',
			ip TEXT NOT NULL,
			latency REAL NOT NULL DEFAULT 0,
			jitter REAL NOT NULL DEFAULT 0,
			loss_rate REAL NOT NULL DEFAULT 0,
			score REAL NOT NULL DEFAULT 0,
			success INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS agent_reports (
			agent_id TEXT PRIMARY KEY,
			finished_at TEXT NOT NULL,
			report_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS controller_candidates (
			cache_key TEXT PRIMARY KEY,
			profile_id TEXT NOT NULL DEFAULT '',
			expires_at TEXT NOT NULL,
			ips_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS active_ips (
			profile_id TEXT NOT NULL DEFAULT '',
			ip TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (profile_id, ip)
		)`,
		`CREATE TABLE IF NOT EXISTS ip_lifecycles (
			agent_id TEXT NOT NULL DEFAULT '',
			profile_id TEXT NOT NULL DEFAULT '',
			region TEXT NOT NULL DEFAULT '',
			ip TEXT NOT NULL,
			first_seen TEXT NOT NULL,
			last_seen TEXT NOT NULL,
			last_active_at TEXT NOT NULL,
			PRIMARY KEY (agent_id, profile_id, region, ip)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_logs_time ON runtime_logs(time)`,
		`CREATE INDEX IF NOT EXISTS idx_check_history_time ON check_history(time)`,
		`CREATE INDEX IF NOT EXISTS idx_ip_samples_time ON ip_samples(time)`,
		`CREATE INDEX IF NOT EXISTS idx_ip_samples_profile_time ON ip_samples(profile_id, time)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_reports_finished ON agent_reports(finished_at)`,
		`CREATE INDEX IF NOT EXISTS idx_ip_lifecycles_first_seen ON ip_lifecycles(first_seen)`,
	}
	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *runtimeStore) close() {
	if s != nil && s.db != nil {
		_ = s.db.Close()
	}
}

func timeText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func parseTimeText(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return t
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func intBool(value int) bool {
	return value != 0
}

func (s *runtimeStore) replaceLogs(logs []LogEntry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM runtime_logs`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO runtime_logs(time, line) VALUES(?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, entry := range logs {
		if _, err := stmt.Exec(timeText(entry.Time), entry.Line); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *runtimeStore) loadLogs(limit int) ([]LogEntry, error) {
	rows, err := s.db.Query(`SELECT time, line FROM (
		SELECT id, time, line FROM runtime_logs ORDER BY id DESC LIMIT ?
	) ORDER BY id ASC`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logs []LogEntry
	for rows.Next() {
		var ts, line string
		if err := rows.Scan(&ts, &line); err != nil {
			return nil, err
		}
		logs = append(logs, LogEntry{Time: parseTimeText(ts), Line: line})
	}
	return logs, rows.Err()
}

func (s *runtimeStore) replaceHistory(history []CheckRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM check_history`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO check_history(time, profile_id, region, ip, latency, success, error) VALUES(?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, rec := range history {
		if _, err := stmt.Exec(timeText(rec.Time), rec.ProfileID, rec.Region, rec.IP, rec.Latency, boolInt(rec.Success), rec.Error); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *runtimeStore) appendHistory(rec CheckRecord) error {
	_, err := s.db.Exec(`INSERT INTO check_history(time, profile_id, region, ip, latency, success, error) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		timeText(rec.Time), rec.ProfileID, rec.Region, rec.IP, rec.Latency, boolInt(rec.Success), rec.Error)
	return err
}

func (s *runtimeStore) loadHistory(limit int) ([]CheckRecord, error) {
	query := `SELECT time, profile_id, region, ip, latency, success, error FROM check_history ORDER BY id ASC`
	args := []interface{}{}
	if limit > 0 {
		query = `SELECT time, profile_id, region, ip, latency, success, error FROM (
			SELECT id, time, profile_id, region, ip, latency, success, error FROM check_history ORDER BY id DESC LIMIT ?
		) ORDER BY id ASC`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []CheckRecord
	for rows.Next() {
		var rec CheckRecord
		var ts string
		var success int
		if err := rows.Scan(&ts, &rec.ProfileID, &rec.Region, &rec.IP, &rec.Latency, &success, &rec.Error); err != nil {
			return nil, err
		}
		rec.Time = parseTimeText(ts)
		rec.Success = intBool(success)
		history = append(history, rec)
	}
	return history, rows.Err()
}

func (s *runtimeStore) replaceSamples(samples []IPSample) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM ip_samples`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO ip_samples(time, agent_id, agent_name, carrier, carrier_label, probe_source, profile_id, profile_name, region, region_label, ip, latency, jitter, loss_rate, score, success, error) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, sample := range samples {
		if _, err := stmt.Exec(timeText(sample.Time), sample.AgentID, sample.AgentName, sample.Carrier, sample.CarrierLabel, sample.ProbeSource, sample.ProfileID, sample.ProfileName, sample.Region, sample.RegionLabel, sample.IP, sample.Latency, sample.Jitter, sample.LossRate, sample.Score, boolInt(sample.Success), sample.Error); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *runtimeStore) appendSamples(samples []IPSample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO ip_samples(time, agent_id, agent_name, carrier, carrier_label, probe_source, profile_id, profile_name, region, region_label, ip, latency, jitter, loss_rate, score, success, error) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, sample := range samples {
		if _, err := stmt.Exec(timeText(sample.Time), sample.AgentID, sample.AgentName, sample.Carrier, sample.CarrierLabel, sample.ProbeSource, sample.ProfileID, sample.ProfileName, sample.Region, sample.RegionLabel, sample.IP, sample.Latency, sample.Jitter, sample.LossRate, sample.Score, boolInt(sample.Success), sample.Error); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *runtimeStore) loadSamples(limit int) ([]IPSample, error) {
	query := `SELECT time, agent_id, agent_name, carrier, carrier_label, probe_source, profile_id, profile_name, region, region_label, ip, latency, jitter, loss_rate, score, success, error FROM ip_samples ORDER BY id ASC`
	args := []interface{}{}
	if limit > 0 {
		query = `SELECT time, agent_id, agent_name, carrier, carrier_label, probe_source, profile_id, profile_name, region, region_label, ip, latency, jitter, loss_rate, score, success, error FROM (
			SELECT id, time, agent_id, agent_name, carrier, carrier_label, probe_source, profile_id, profile_name, region, region_label, ip, latency, jitter, loss_rate, score, success, error FROM ip_samples ORDER BY id DESC LIMIT ?
		) ORDER BY id ASC`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var samples []IPSample
	for rows.Next() {
		var sample IPSample
		var ts string
		var success int
		if err := rows.Scan(&ts, &sample.AgentID, &sample.AgentName, &sample.Carrier, &sample.CarrierLabel, &sample.ProbeSource, &sample.ProfileID, &sample.ProfileName, &sample.Region, &sample.RegionLabel, &sample.IP, &sample.Latency, &sample.Jitter, &sample.LossRate, &sample.Score, &success, &sample.Error); err != nil {
			return nil, err
		}
		sample.Time = parseTimeText(ts)
		sample.Success = intBool(success)
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}

func (s *runtimeStore) upsertAgentReport(report agent.Report) error {
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO agent_reports(agent_id, finished_at, report_json) VALUES(?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET finished_at = excluded.finished_at, report_json = excluded.report_json`,
		report.AgentID, timeText(report.FinishedAt), string(data))
	return err
}

func (s *runtimeStore) loadAgentReports(ttl time.Duration) ([]agent.Report, error) {
	query := `SELECT report_json FROM agent_reports`
	args := []interface{}{}
	if ttl > 0 {
		query += ` WHERE finished_at >= ?`
		args = append(args, timeText(time.Now().Add(-ttl)))
	}
	query += ` ORDER BY finished_at DESC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reports []agent.Report
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var report agent.Report
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			continue
		}
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

func (s *runtimeStore) upsertControllerCandidates(profileID, key string, entry controllerCandidateCacheEntry) error {
	data, err := json.Marshal(entry.IPs)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO controller_candidates(cache_key, profile_id, expires_at, ips_json) VALUES(?, ?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET profile_id = excluded.profile_id, expires_at = excluded.expires_at, ips_json = excluded.ips_json`,
		key, profileID, timeText(entry.ExpiresAt), string(data))
	return err
}

func (s *runtimeStore) loadControllerCandidate(key string) (controllerCandidateCacheEntry, bool, error) {
	var expiresAt, raw string
	err := s.db.QueryRow(`SELECT expires_at, ips_json FROM controller_candidates WHERE cache_key = ?`, key).Scan(&expiresAt, &raw)
	if err == sql.ErrNoRows {
		return controllerCandidateCacheEntry{}, false, nil
	}
	if err != nil {
		return controllerCandidateCacheEntry{}, false, err
	}
	var ips []string
	if err := json.Unmarshal([]byte(raw), &ips); err != nil {
		return controllerCandidateCacheEntry{}, false, err
	}
	return controllerCandidateCacheEntry{IPs: ips, ExpiresAt: parseTimeText(expiresAt)}, true, nil
}

func (s *runtimeStore) loadControllerCandidates() (map[string]controllerCandidateCacheEntry, error) {
	rows, err := s.db.Query(`SELECT cache_key, expires_at, ips_json FROM controller_candidates`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]controllerCandidateCacheEntry)
	for rows.Next() {
		var key, expiresAt, raw string
		if err := rows.Scan(&key, &expiresAt, &raw); err != nil {
			return nil, err
		}
		var ips []string
		if err := json.Unmarshal([]byte(raw), &ips); err != nil {
			continue
		}
		out[key] = controllerCandidateCacheEntry{IPs: ips, ExpiresAt: parseTimeText(expiresAt)}
	}
	return out, rows.Err()
}

func (s *runtimeStore) replaceActiveIPs(profileID string, ips map[string]bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM active_ips WHERE profile_id = ?`, profileID); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO active_ips(profile_id, ip, updated_at) VALUES(?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := timeText(time.Now())
	for ip, active := range ips {
		if !active {
			continue
		}
		if _, err := stmt.Exec(profileID, ip, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *runtimeStore) loadActiveIPs() (map[string]bool, map[string]map[string]bool, error) {
	rows, err := s.db.Query(`SELECT profile_id, ip FROM active_ips`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	active := make(map[string]bool)
	byProfile := make(map[string]map[string]bool)
	for rows.Next() {
		var profileID, ip string
		if err := rows.Scan(&profileID, &ip); err != nil {
			return nil, nil, err
		}
		active[ip] = true
		if profileID != "" {
			if byProfile[profileID] == nil {
				byProfile[profileID] = make(map[string]bool)
			}
			byProfile[profileID][ip] = true
		}
	}
	return active, byProfile, rows.Err()
}

func (s *runtimeStore) replaceIPLifecycles(records map[string]IPLifecycle) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM ip_lifecycles`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO ip_lifecycles(agent_id, profile_id, region, ip, first_seen, last_seen, last_active_at) VALUES(?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, rec := range records {
		if rec.IP == "" || rec.FirstSeen.IsZero() {
			continue
		}
		if rec.LastSeen.IsZero() {
			rec.LastSeen = rec.FirstSeen
		}
		if rec.LastActiveAt.IsZero() {
			rec.LastActiveAt = rec.LastSeen
		}
		if _, err := stmt.Exec(rec.AgentID, rec.ProfileID, rec.Region, rec.IP, timeText(rec.FirstSeen), timeText(rec.LastSeen), timeText(rec.LastActiveAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *runtimeStore) loadIPLifecycles() (map[string]IPLifecycle, error) {
	rows, err := s.db.Query(`SELECT agent_id, profile_id, region, ip, first_seen, last_seen, last_active_at FROM ip_lifecycles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make(map[string]IPLifecycle)
	for rows.Next() {
		var rec IPLifecycle
		var firstSeen, lastSeen, lastActiveAt string
		if err := rows.Scan(&rec.AgentID, &rec.ProfileID, &rec.Region, &rec.IP, &firstSeen, &lastSeen, &lastActiveAt); err != nil {
			return nil, err
		}
		rec.FirstSeen = parseTimeText(firstSeen)
		rec.LastSeen = parseTimeText(lastSeen)
		rec.LastActiveAt = parseTimeText(lastActiveAt)
		records[sampleKey(rec.AgentID, rec.ProfileID, rec.Region, rec.IP)] = rec
	}
	return records, rows.Err()
}

func (s *runtimeStore) prune() error {
	cutoff := timeText(time.Now().Add(-retentionWindow))
	queries := []string{
		`DELETE FROM runtime_logs WHERE time < ?`,
		`DELETE FROM check_history WHERE time < ?`,
		`DELETE FROM ip_samples WHERE time < ?`,
	}
	for _, query := range queries {
		if _, err := s.db.Exec(query, cutoff); err != nil {
			return fmt.Errorf("prune runtime db: %w", err)
		}
	}
	return nil
}
