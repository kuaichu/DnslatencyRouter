package mtr

import (
	"strings"
	"testing"
)

func TestArgumentsUseBoundedNoGeoIPMTR(t *testing.T) {
	args := Arguments(Request{IP: "1.1.1.1", Port: 12001})
	joined := " " + strings.Join(args, " ") + " "
	for _, expected := range []string{" --queries 10 ", " --max-hops 30 ", " --data-provider NextTrace-API ", " 1.1.1.1 "} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing %q in %v", expected, args)
		}
	}
	if strings.Contains(joined, " --report ") || strings.Contains(joined, " --wide ") || strings.Contains(joined, " --no-rdns ") {
		t.Fatalf("regular route output must not use MTR report flags: %v", args)
	}
}

func TestRequestValidationRejectsNonPublicTargets(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "2001:db8::1"} {
		if err := (Request{ID: "x", AgentID: "controller", IP: ip, Port: 443}).Validate(); err == nil {
			t.Fatalf("accepted %s", ip)
		}
	}
}

func TestValidOutputAcceptsHopByHopRouteAndLegacyReport(t *testing.T) {
	if !validOutput("NextTrace v1.7.3\n10.0.0.1 -> 1.1.1.1, 30 hops max") {
		t.Fatal("hop-by-hop NextTrace output was rejected")
	}
	if !validOutput("HOST: source Loss% Snt") {
		t.Fatal("legacy MTR output was rejected")
	}
	if validOutput("NextTrace failed") {
		t.Fatal("failure text was accepted as a report")
	}
}
