package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

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

func TestUpdateYAMLFieldEscapesRootValueAndIgnoresNestedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := "cloudflare:\n  api_token: nested\ncarrier: auto\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateYAMLField(path, "carrier", `a"b: c`, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `carrier: "a\"b: c"`) {
		t.Fatalf("root value was not escaped: %s", text)
	}
	if strings.Contains(text, "api_token: a") {
		t.Fatalf("nested field was modified: %s", text)
	}
	cfg := struct {
		Cloudflare struct {
			APIToken string `yaml:"api_token"`
		} `yaml:"cloudflare"`
		Carrier string `yaml:"carrier"`
	}{}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Cloudflare.APIToken != "nested" || cfg.Carrier != `a"b: c` {
		t.Fatalf("decoded config = %#v", cfg)
	}
}

func TestSaveLeavesOriginalWhenValidationFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := "sentinel\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Cloudflare: CloudflareConfig{APIToken: "token"}, TimePenaltyStartHour: 99}
	if err := Save(path, cfg); err == nil {
		t.Fatal("Save unexpectedly accepted invalid config")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("original file changed after failed save: %q", data)
	}
}

func TestSavePreservesExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file mode changes")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{TargetDomain: "entry.example.com", Cloudflare: CloudflareConfig{APIToken: "token", ZoneID: "zone", RecordID: "record"}}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.TargetDomain = "entry.example.com"
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("permissions = %o, want 600", got)
	}
}
