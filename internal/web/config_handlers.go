package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"dns-latency-router/internal/config"
)

func (s *Server) handleAPIAirportProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		s.configMu.Lock()
		cfg, err := config.Load(s.cfgPath)
		s.configMu.Unlock()
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
	s.configMu.Lock()
	defer s.configMu.Unlock()
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
		id, slug, name := strings.TrimSpace(incoming.ID), strings.TrimSpace(incoming.Slug), strings.TrimSpace(incoming.Name)
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
		profile := config.AirportProfile{ID: id, Name: name, Slug: slug, TargetDomains: append([]string(nil), incoming.TargetDomains...), ProbeSource: strings.TrimSpace(incoming.ProbeSource), Carrier: config.NormalizeCarrier(incoming.Carrier), EntryRecord: entry, RegionRecords: prev.RegionRecords, CarrierRecords: prev.CarrierRecords}
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
	st := s.GetStatus()
	st.Profiles = buildProfileStatuses(&next)
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
	if s.onConfigApplied != nil {
		s.onConfigApplied(&next)
	}
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
			regions = append(regions, RegionStatus{Region: "entry", Label: label, CustomDomain: profile.EntryRecord.CustomDomain, Status: "no_candidate"})
		}
		for carrier, rec := range profile.CarrierRecords {
			label := rec.Label
			if label == "" {
				label = config.CarrierLabel(carrier)
			}
			regions = append(regions, RegionStatus{Region: "carrier-" + carrier, Label: label, CustomDomain: rec.CustomDomain, Status: "no_candidate"})
		}
		for region, rec := range profile.RegionRecords {
			label := rec.Label
			if label == "" {
				label = config.RegionLabel(region)
			}
			regions = append(regions, RegionStatus{Region: region, Label: label, CustomDomain: rec.CustomDomain, Status: "no_candidate"})
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

type configRequest struct {
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

func (s *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		s.configMu.Lock()
		cfg, err := config.Load(s.cfgPath)
		s.configMu.Unlock()
		if err != nil {
			writeJSON(w, map[string]string{"error": "load config: " + err.Error()})
			return
		}
		payload := map[string]interface{}{
			"target_domain":              cfg.TargetDomain,
			"custom_domain":              cfg.CustomDomain,
			"probe_source":               cfg.ProbeSource,
			"carrier":                    cfg.Carrier,
			"carrier_label":              cfg.EffectiveCarrierLabel(),
			"ping_mode":                  cfg.PingMode,
			"ping_port":                  cfg.PingPort,
			"check_interval":             cfg.CheckIntervalSec,
			"ping_attempts":              cfg.PingAttempts,
			"selection_latency_weight":   cfg.LatencyWeight,
			"selection_jitter_weight":    cfg.JitterWeight,
			"selection_loss_weight":      cfg.LossWeight,
			"switch_improvement_percent": cfg.SwitchImprovement,
			"switch_stable_seconds":      cfg.SwitchStableSec,
			"failed_orphan_ttl_hours":    cfg.FailedOrphanTTLHours,
			"fallback_baseline_ip":       cfg.FallbackBaselineIP,
			"alert_webhook_url":          cfg.AlertWebhookURL,
			"time_penalty_start_hour":    cfg.TimePenaltyStartHour,
			"time_penalty_end_hour":      cfg.TimePenaltyEndHour,
			"time_penalty_score":         cfg.TimePenaltyScore,
			"time_penalty_org_keywords":  cfg.TimePenaltyOrgKeywords,
			"cloudflare_api_token_set":   strings.TrimSpace(cfg.Cloudflare.APIToken) != "",
			"cloudflare_zone_id":         cfg.Cloudflare.ZoneID,
			"cloudflare_record_id":       cfg.Cloudflare.RecordID,
			"agent_controller_url":       cfg.Agent.ControllerURL,
			"agent_token_set":            strings.TrimSpace(cfg.Agent.Token) != "",
			"agent_report_ttl_seconds":   cfg.Agent.ReportTTLSeconds,
			"agents":                     agentPeersToPayload(cfg.Agents),
			"agent_statuses":             s.AgentStatuses(0),
		}
		writeJSON(w, payload)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body configRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	current, err := config.Load(s.cfgPath)
	if err != nil {
		writeJSON(w, map[string]string{"error": "load config: " + err.Error()})
		return
	}
	next := *current
	multi := len(current.AirportProfiles) > 0
	changed := false
	if !multi {
		if body.TargetDomain != nil && strings.TrimSpace(*body.TargetDomain) != "" && strings.TrimSpace(*body.TargetDomain) != current.TargetDomain {
			next.TargetDomain = strings.TrimSpace(*body.TargetDomain)
			changed = true
		}
		if body.CustomDomain != nil && strings.TrimSpace(*body.CustomDomain) != "" && strings.TrimSpace(*body.CustomDomain) != current.CustomDomain {
			next.CustomDomain = strings.TrimSpace(*body.CustomDomain)
			changed = true
		}
		if body.ProbeSource != nil && strings.TrimSpace(*body.ProbeSource) != "" && strings.TrimSpace(*body.ProbeSource) != current.ProbeSource {
			next.ProbeSource = strings.TrimSpace(*body.ProbeSource)
			changed = true
		}
		if body.Carrier != nil {
			v := config.NormalizeCarrier(*body.Carrier)
			if v != current.Carrier {
				next.Carrier = v
				changed = true
			}
		}
	}
	if body.PingMode != nil && strings.TrimSpace(*body.PingMode) != "" {
		v := strings.ToLower(strings.TrimSpace(*body.PingMode))
		if v != "icmp" && v != "tcp" {
			writeJSON(w, map[string]string{"error": "ping_mode must be icmp or tcp"})
			return
		}
		if v != current.PingMode {
			next.PingMode = v
			changed = true
		}
	}
	if body.PingPort != nil && *body.PingPort > 65535 {
		writeJSON(w, map[string]string{"error": "ping_port must be between 1 and 65535"})
		return
	}
	if body.PingPort != nil && *body.PingPort > 0 && *body.PingPort != current.PingPort {
		next.PingPort = *body.PingPort
		changed = true
	}
	if body.CheckInterval != nil && *body.CheckInterval > 0 && *body.CheckInterval != current.CheckIntervalSec {
		next.CheckIntervalSec = *body.CheckInterval
		changed = true
	}
	if body.PingAttempts != nil && *body.PingAttempts > 0 && *body.PingAttempts != current.PingAttempts {
		next.PingAttempts = *body.PingAttempts
		changed = true
	}
	if body.LatencyWeight != nil && *body.LatencyWeight > 0 && *body.LatencyWeight != current.LatencyWeight {
		next.LatencyWeight = *body.LatencyWeight
		changed = true
	}
	if body.JitterWeight != nil && *body.JitterWeight >= 0 && *body.JitterWeight != current.JitterWeight {
		next.JitterWeight = *body.JitterWeight
		changed = true
	}
	if body.LossWeight != nil && *body.LossWeight >= 0 && *body.LossWeight != current.LossWeight {
		next.LossWeight = *body.LossWeight
		changed = true
	}
	if body.SwitchImprovement != nil && *body.SwitchImprovement >= 0 && *body.SwitchImprovement != current.SwitchImprovement {
		next.SwitchImprovement = *body.SwitchImprovement
		changed = true
	}
	if body.SwitchStableSec != nil && *body.SwitchStableSec >= 0 && *body.SwitchStableSec != current.SwitchStableSec {
		next.SwitchStableSec = *body.SwitchStableSec
		changed = true
	}
	if body.FailedOrphanTTLHours != nil && *body.FailedOrphanTTLHours >= 0 && *body.FailedOrphanTTLHours != current.FailedOrphanTTLHours {
		next.FailedOrphanTTLHours = *body.FailedOrphanTTLHours
		changed = true
	}
	if body.FallbackBaselineIP != nil {
		v := strings.TrimSpace(*body.FallbackBaselineIP)
		if v != "" && net.ParseIP(v) == nil {
			writeJSON(w, map[string]string{"error": "fallback_baseline_ip must be a valid IP"})
			return
		}
		if v != current.FallbackBaselineIP {
			next.FallbackBaselineIP = v
			changed = true
		}
	}
	if body.AlertWebhookURL != nil {
		v := strings.TrimSpace(*body.AlertWebhookURL)
		if v != "" {
			if _, err := url.ParseRequestURI(v); err != nil {
				writeJSON(w, map[string]string{"error": "alert_webhook_url is invalid: " + err.Error()})
				return
			}
		}
		if v != current.AlertWebhookURL {
			next.AlertWebhookURL = v
			changed = true
		}
	}
	if body.TimePenaltyStartHour != nil && *body.TimePenaltyStartHour != current.TimePenaltyStartHour {
		next.TimePenaltyStartHour = *body.TimePenaltyStartHour
		changed = true
	}
	if body.TimePenaltyEndHour != nil && *body.TimePenaltyEndHour != current.TimePenaltyEndHour {
		next.TimePenaltyEndHour = *body.TimePenaltyEndHour
		changed = true
	}
	if body.TimePenaltyScore != nil && *body.TimePenaltyScore != current.TimePenaltyScore {
		next.TimePenaltyScore = *body.TimePenaltyScore
		changed = true
	}
	if body.TimePenaltyKeywords != nil {
		v := strings.TrimSpace(*body.TimePenaltyKeywords)
		if v != current.TimePenaltyOrgKeywords {
			next.TimePenaltyOrgKeywords = v
			changed = true
		}
	}
	if body.CloudflareAPIToken != nil && strings.TrimSpace(*body.CloudflareAPIToken) != "" && strings.TrimSpace(*body.CloudflareAPIToken) != current.Cloudflare.APIToken {
		next.Cloudflare.APIToken = strings.TrimSpace(*body.CloudflareAPIToken)
		changed = true
	}
	if body.CloudflareZoneID != nil && strings.TrimSpace(*body.CloudflareZoneID) != "" && strings.TrimSpace(*body.CloudflareZoneID) != current.Cloudflare.ZoneID {
		next.Cloudflare.ZoneID = strings.TrimSpace(*body.CloudflareZoneID)
		changed = true
	}
	if body.CloudflareRecordID != nil && strings.TrimSpace(*body.CloudflareRecordID) != "" && strings.TrimSpace(*body.CloudflareRecordID) != current.Cloudflare.RecordID {
		next.Cloudflare.RecordID = strings.TrimSpace(*body.CloudflareRecordID)
		changed = true
	}
	if body.AgentToken != nil && strings.TrimSpace(*body.AgentToken) != "" && strings.TrimSpace(*body.AgentToken) != current.Agent.Token {
		next.Agent.Token = strings.TrimSpace(*body.AgentToken)
		changed = true
	}
	if body.AgentControllerURL != nil {
		v := strings.TrimRight(strings.TrimSpace(*body.AgentControllerURL), "/")
		if v != "" {
			if _, err := url.ParseRequestURI(v); err != nil {
				writeJSON(w, map[string]string{"error": "agent_controller_url is invalid: " + err.Error()})
				return
			}
		}
		if v != strings.TrimRight(strings.TrimSpace(current.Agent.ControllerURL), "/") {
			next.Agent.ControllerURL = v
			changed = true
		}
	}
	if body.AgentReportTTL != nil {
		if *body.AgentReportTTL < 30 {
			writeJSON(w, map[string]string{"error": "agent_report_ttl_seconds must be at least 30"})
			return
		}
		if *body.AgentReportTTL != current.Agent.ReportTTLSeconds {
			next.Agent.ReportTTLSeconds = *body.AgentReportTTL
			changed = true
		}
	}
	if body.Agents != nil {
		peers := make([]config.AgentPeerConfig, 0, len(*body.Agents))
		for _, p := range *body.Agents {
			peers = append(peers, config.AgentPeerConfig{ID: strings.TrimSpace(p.ID), Name: strings.TrimSpace(p.Name), ProbeSource: strings.TrimSpace(p.ProbeSource), Carrier: config.NormalizeCarrier(p.Carrier)})
		}
		next.Agents = peers
		changed = true
	}
	if !changed {
		writeJSON(w, map[string]string{"error": "no changes or empty values"})
		return
	}
	if err := next.Normalize(); err != nil {
		writeJSON(w, map[string]string{"error": "配置校验失败: " + err.Error()})
		return
	}
	if err := config.Save(s.cfgPath, &next); err != nil {
		writeJSON(w, map[string]string{"error": "保存失败: " + err.Error()})
		return
	}
	s.SetSafeguards(next.FailedOrphanTTLHours, next.FallbackBaselineIP, next.AlertWebhookURL)
	s.SetTimePenaltyConfig(next.TimePenaltyStartHour, next.TimePenaltyEndHour, next.TimePenaltyScore, next.TimePenaltyOrgKeywords)
	st := s.GetStatus()
	if !multi {
		st.TargetDomain = next.TargetDomain
		st.CustomDomain = next.CustomDomain
		st.ProbeSource = next.ProbeSource
		st.Carrier = next.Carrier
		st.CarrierLabel = next.EffectiveCarrierLabel()
	}
	// Probe and selection settings are global in both legacy and multi-profile
	// modes, so their status must follow the saved config in either case.
	st.PingMode = next.PingMode
	st.PingPort = next.PingPort
	st.CheckIntervalSec = next.CheckIntervalSec
	st.PingAttempts = next.PingAttempts
	st.LatencyWeight = next.LatencyWeight
	st.JitterWeight = next.JitterWeight
	st.LossWeight = next.LossWeight
	st.SwitchImprovement = next.SwitchImprovement
	st.SwitchStableSec = next.SwitchStableSec
	if body.AgentToken != nil || body.AgentReportTTL != nil || body.Agents != nil {
		st.Agents = s.AgentStatuses(0)
	}
	s.UpdateStatus(st)
	if s.onConfigApplied != nil {
		s.onConfigApplied(&next)
	}
	log.Printf("[config] updated: target_domain=%q custom_domain=%q probe_source=%q carrier=%q ping_mode=%q", next.TargetDomain, next.CustomDomain, next.ProbeSource, next.Carrier, next.PingMode)
	writeJSON(w, map[string]bool{"ok": true})
}
