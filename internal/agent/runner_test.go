package agent

import "testing"

func TestMergeCandidateIPsFiltersReservedAndDedupes(t *testing.T) {
	got := mergeCandidateIPs(
		[]string{"198.18.0.63", "34.96.159.37"},
		[]string{"34.96.159.37", "56.69.116.30", "10.0.0.1"},
	)
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
