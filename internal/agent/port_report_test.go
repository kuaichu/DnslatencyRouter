package agent

import (
	"net"
	"testing"
	"time"

	"dns-latency-router/internal/checker"
)

func TestUnavailablePortReportPassesControllerValidation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	results := checker.PingAll([]string{"127.0.0.1"}, "icmp", port, 100*time.Millisecond, 4, 1, 0, 0)
	report := resultFromChecker(results[0])
	if report.Error == "" {
		t.Fatal("expected closed port failure")
	}
	if err := ValidateResult(report); err != nil {
		t.Fatalf("controller rejected port-check report: %v", err)
	}
}
