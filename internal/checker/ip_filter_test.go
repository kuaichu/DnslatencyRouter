package checker

import "testing"

func TestIsUsableCandidateIPRejectsFakeAndReservedRanges(t *testing.T) {
	rejected := []string{
		"198.18.0.63",
		"198.19.255.254",
		"10.0.0.1",
		"100.64.1.1",
		"127.0.0.1",
		"169.254.1.1",
		"172.16.0.1",
		"192.168.1.1",
		"192.0.2.10",
		"203.0.113.10",
		"224.0.0.1",
		"240.0.0.1",
		"2001:db8::1",
		"",
		"not-an-ip",
	}

	for _, ip := range rejected {
		if IsUsableCandidateIP(ip) {
			t.Fatalf("expected %s to be rejected", ip)
		}
	}
}

func TestFilterUsableCandidateIPsKeepsPublicIPv4(t *testing.T) {
	got := FilterUsableCandidateIPs([]string{
		"198.18.0.63",
		"34.96.159.37",
		"34.96.159.37",
		"56.69.116.30",
		"10.0.0.1",
	})

	want := []string{"34.96.159.37", "56.69.116.30"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
