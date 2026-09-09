package main

import (
	"strings"
	"time"

	"dns-latency-router/internal/config"
	"dns-latency-router/internal/web"
)

// Updating settings does not constitute a probe or replace airport results.
func applyRuntimeConfigStatus(st *web.Status, cfg *config.Config, nextCheck *time.Time, now time.Time) {
	lastCheck := st.LastCheck
	applyConfigStatus(st, cfg, nextCheck, now)
	st.LastCheck = lastCheck
	if cfg.HasAirportProfiles() {
		applyProfileSummary(st, st.Profiles)
	} else {
		st.Profiles = nil
	}
}

// queueConfigUpdate keeps the most recent complete configuration without
// blocking a settings request on a long-running probe cycle.
func queueConfigUpdate(updates chan *config.Config, next *config.Config) {
	for {
		select {
		case updates <- next:
			return
		default:
			select {
			case <-updates:
			default:
			}
		}
	}
}

// A failed discovery interrupts the observation window for this profile only.
// Preserve outage/alert state and the observations of unrelated profiles.
func (c *switchController) resetProfileCandidates(profileID string) {
	for key := range c.routes {
		if strings.HasPrefix(key, profileID+"|") {
			c.resetRoute(key)
		}
	}
}

// Missing/expired agent reports and failed record reads cannot extend an
// observation window. Keep a window only if this cycle actually evaluated it.
func (c *switchController) resetUnobservedRoutes(profileID string, statuses []web.RegionStatus) {
	observed := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		if status.Status != "read_failed" {
			observed[profileID+"|"+status.Region] = true
		}
	}
	for key := range c.routes {
		if strings.HasPrefix(key, profileID+"|") && !observed[key] {
			c.resetRoute(key)
		}
	}
}
