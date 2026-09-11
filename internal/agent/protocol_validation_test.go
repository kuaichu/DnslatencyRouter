package agent

import (
	"math"
	"testing"
)

func TestValidateResultAcceptsProducerFailureAndPartialSuccess(t *testing.T) {
	for _, result := range []Result{
		{Latency: 0, Jitter: 0, LossRate: 100, Attempts: 4, Successes: 0, Error: "all probes failed"},
		{Latency: 12.5, Jitter: 1.5, LossRate: 50, Attempts: 4, Successes: 2, Score: 16},
	} {
		if err := ValidateResult(result); err != nil {
			t.Fatalf("valid producer result rejected: %v", err)
		}
	}
}

func TestValidateResultRejectsNonFiniteInconsistentAndOverflowValues(t *testing.T) {
	cases := []Result{
		{Latency: math.NaN(), Attempts: 1, Successes: 1},
		{Latency: math.Inf(1), Attempts: 1, Successes: 1},
		{LossRate: 101, Attempts: 1, Successes: 1},
		{Attempts: 2, Successes: 3},
		{Attempts: 4, Successes: 2, LossRate: 0},
		{Attempts: 1, Successes: 0},
		{Attempts: 1, Successes: 1, Error: "failed"},
		{Latency: 1e13, Attempts: 1, Successes: 1},
	}
	for _, result := range cases {
		if err := ValidateResult(result); err == nil {
			t.Fatalf("invalid result accepted: %+v", result)
		}
	}
}
