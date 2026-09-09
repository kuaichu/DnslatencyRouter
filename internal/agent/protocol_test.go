package agent

import (
	"testing"
	"time"
)

func TestReportFreshnessUsesControllerReceiptTime(t *testing.T) {
	now := time.Now()
	r := Report{ReceivedAt: now, FinishedAt: now.Add(24 * time.Hour)}
	if !r.FreshnessTime().Equal(now) {
		t.Fatal("agent clock must not control report freshness")
	}
	r.ReceivedAt = time.Time{}
	if !r.FreshnessTime().Equal(r.FinishedAt) {
		t.Fatal("legacy reports must retain their timestamp for compatibility")
	}
}
