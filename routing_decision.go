package main

import (
	"dns-latency-router/internal/checker"
	"time"
)

type failureObservation struct {
	count int
	last  time.Time
}

func failureConfirmationCount(configured int) int {
	if configured <= 0 {
		return 3
	}
	return configured
}

func (f *failureObservation) confirm(results []checker.Result, required int) bool {
	if !allProbesFailed(results) {
		*f = failureObservation{}
		return false
	}
	var observed time.Time
	for _, result := range results {
		if result.FinishedAt.After(observed) {
			observed = result.FinishedAt
		}
	}
	// Reading the same report again is not another measurement. Reports without
	// observation timestamps cannot establish consecutive failure.
	if observed.IsZero() {
		return false
	}
	if observed.After(f.last) {
		f.last = observed
		f.count++
	}
	return f.count >= failureConfirmationCount(required)
}

func allProbesFailed(results []checker.Result) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		if result.Err == nil {
			return false
		}
	}
	return true
}

func includeCurrentProbeIP(ips []string, current string) []string {
	current, ok := checker.NormalizeCandidateIP(current)
	if !ok {
		return ips
	}
	for _, ip := range ips {
		if ip == current {
			return ips
		}
	}
	return append(ips, current)
}

// DNS record I/O is separated from decisions so error paths can be verified
// without making changes to live Cloudflare records.
type dnsRecordClient interface {
	CurrentIP() (string, error)
	CurrentIPByName(string) (string, error)
	UpdateRecord(string) error
	UpdateRecordByName(string, string) error
	DeleteRecord() error
	DeleteRecordByName(string) error
}

type namedDNSRecordClient struct {
	dnsRecordClient
	name string
}

func (c namedDNSRecordClient) CurrentIP() (string, error)   { return c.CurrentIPByName(c.name) }
func (c namedDNSRecordClient) UpdateRecord(ip string) error { return c.UpdateRecordByName(c.name, ip) }
func (c namedDNSRecordClient) DeleteRecord() error          { return c.DeleteRecordByName(c.name) }
