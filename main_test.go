package main

import (
	"testing"

	"dns-latency-router/internal/config"
)

func TestCarrierEntryRecordForGeneratesCarrierScopedEntryDomain(t *testing.T) {
	cfg := &config.Config{BaseDomain: "ziher.eu.org"}
	profile := config.AirportProfile{
		ID:          "sntp",
		Slug:        "sntp",
		ProbeSource: "宁波联通",
		Carrier:     "auto",
	}

	rec := carrierEntryRecordFor(cfg, profile, "telecom")

	if got, want := rec.CustomDomain, "ct-sntp-entry.ziher.eu.org"; got != want {
		t.Fatalf("custom domain = %q, want %q", got, want)
	}
	if got, want := rec.Label, "中国电信"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
}

func TestCarrierEntryRecordForCompletesConfiguredRecord(t *testing.T) {
	cfg := &config.Config{BaseDomain: "ziher.eu.org"}
	profile := config.AirportProfile{
		ID:   "sntp",
		Slug: "sntp",
		CarrierRecords: map[string]config.RegionRecord{
			"unicom": {
				RecordID: "record-1",
			},
		},
	}

	rec := carrierEntryRecordFor(cfg, profile, "unicom")

	if got, want := rec.CustomDomain, "cu-sntp-entry.ziher.eu.org"; got != want {
		t.Fatalf("custom domain = %q, want %q", got, want)
	}
	if got, want := rec.Label, "中国联通"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
	if got, want := rec.RecordID, "record-1"; got != want {
		t.Fatalf("record id = %q, want %q", got, want)
	}
}
