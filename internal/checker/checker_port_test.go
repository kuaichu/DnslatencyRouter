package checker

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestPingAllICMPRejectsUnavailableServicePort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	results := PingAll([]string{"127.0.0.1"}, "icmp", port, 50*time.Millisecond, 2, 1, 0, 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	result := results[0]
	if result.Err == nil {
		t.Fatal("expected unavailable port to fail the result")
	}
	if !strings.Contains(result.Err.Error(), "port ") || !strings.Contains(result.Err.Error(), " unavailable") {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Attempts != 1 || result.Successes != 0 {
		t.Fatalf("unexpected failed probe counts: attempts=%d successes=%d", result.Attempts, result.Successes)
	}
}
