package config

import "testing"

func TestProfileRecordDomainUsesCarrierSlugRegion(t *testing.T) {
	profile := AirportProfile{
		ID:          "sntp",
		Slug:        "sntp",
		ProbeSource: "宁波电信",
		Carrier:     "auto",
	}
	got := ProfileRecordDomain("ziher.eu.org", profile, "hk")
	want := "ct-sntp-hk.ziher.eu.org"
	if got != want {
		t.Fatalf("ProfileRecordDomain() = %q, want %q", got, want)
	}
}

func TestNormalizeAirportProfilesMigratesLegacyGeneratedDomains(t *testing.T) {
	cfg := &Config{
		Cloudflare: CloudflareConfig{APIToken: "test-token"},
		BaseDomain: "ziher.eu.org",
		AirportProfiles: []AirportProfile{{
			ID:   "sntp",
			Slug: "sntp",
			TargetDomains: []string{
				"entry.example.com",
			},
			ProbeSource: "宁波联通",
			Carrier:     "auto",
			EntryRecord: RegionRecord{
				CustomDomain: "sntp-entry.ziher.eu.org",
			},
			RegionRecords: map[string]RegionRecord{
				"hk": {CustomDomain: "sntp-hk.ziher.eu.org"},
			},
		}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	profile := cfg.AirportProfiles[0]
	if got, want := profile.EntryRecord.CustomDomain, "cu-sntp-entry.ziher.eu.org"; got != want {
		t.Fatalf("entry custom domain = %q, want %q", got, want)
	}
	if got, want := profile.RegionRecords["hk"].CustomDomain, "cu-sntp-hk.ziher.eu.org"; got != want {
		t.Fatalf("hk custom domain = %q, want %q", got, want)
	}
}

func TestNormalizeAirportProfilesPreservesExplicitCustomDomain(t *testing.T) {
	cfg := &Config{
		Cloudflare: CloudflareConfig{APIToken: "test-token"},
		BaseDomain: "ziher.eu.org",
		AirportProfiles: []AirportProfile{{
			ID:   "sntp",
			Slug: "sntp",
			TargetDomains: []string{
				"entry.example.com",
			},
			ProbeSource: "宁波电信",
			Carrier:     "auto",
			RegionRecords: map[string]RegionRecord{
				"hk": {CustomDomain: "manual.example.net"},
			},
		}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if got, want := cfg.AirportProfiles[0].RegionRecords["hk"].CustomDomain, "manual.example.net"; got != want {
		t.Fatalf("explicit custom domain = %q, want %q", got, want)
	}
}
