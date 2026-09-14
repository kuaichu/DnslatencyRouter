package agent

import (
	"fmt"
	"math"
	"time"
)

// Version identifies the Agent runtime shown by the controller.
const Version = "2026.09.14"

type ProfileJob struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Slug          string   `json:"slug"`
	TargetDomains []string `json:"targetDomains"`
	CandidateIPs  []string `json:"candidateIps,omitempty"`
	ProbeSource   string   `json:"probeSource"`
	Carrier       string   `json:"carrier"`
}

type JobResponse struct {
	ServerTime         time.Time    `json:"serverTime"`
	AgentName          string       `json:"agentName,omitempty"`
	AgentProbeSource   string       `json:"agentProbeSource,omitempty"`
	AgentCarrier       string       `json:"agentCarrier,omitempty"`
	AgentCarrierLabel  string       `json:"agentCarrierLabel,omitempty"`
	CheckInterval      int          `json:"checkInterval"`
	PingMode           string       `json:"pingMode"`
	PingPort           int          `json:"pingPort"`
	PingTimeoutSeconds int          `json:"pingTimeoutSeconds"`
	PingAttempts       int          `json:"pingAttempts"`
	LatencyWeight      float64      `json:"latencyWeight"`
	JitterWeight       float64      `json:"jitterWeight"`
	LossWeight         float64      `json:"lossWeight"`
	Profiles           []ProfileJob `json:"profiles"`
}

type Result struct {
	IP         string    `json:"ip"`
	Latency    float64   `json:"latency"`
	Jitter     float64   `json:"jitter"`
	LossRate   float64   `json:"lossRate"`
	Attempts   int       `json:"attempts"`
	Successes  int       `json:"successes"`
	Score      float64   `json:"score"`
	ObservedAt time.Time `json:"observedAt,omitempty"`
	Error      string    `json:"error,omitempty"`
}

const maxReportedAttempts = 1000

// ValidateResult checks the wire-level invariants produced by checker.PingAll.
// It deliberately does not validate Score against controller weights: the
// controller owns those weights and recomputes the score before routing.
func ValidateResult(result Result) error {
	finiteNonNegative := func(name string, value float64) error {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("%s must be finite and non-negative", name)
		}
		return nil
	}
	if err := finiteNonNegative("latency", result.Latency); err != nil {
		return err
	}
	if err := finiteNonNegative("jitter", result.Jitter); err != nil {
		return err
	}
	if result.Latency >= float64((1<<63)-1)/float64(time.Millisecond) || result.Jitter >= float64((1<<63)-1)/float64(time.Millisecond) {
		return fmt.Errorf("latency or jitter exceeds time.Duration range")
	}
	if err := finiteNonNegative("lossRate", result.LossRate); err != nil {
		return err
	}
	if result.LossRate > 100 {
		return fmt.Errorf("lossRate must be between 0 and 100")
	}
	if err := finiteNonNegative("score", result.Score); err != nil {
		return err
	}
	if result.Attempts < 1 || result.Attempts > maxReportedAttempts {
		return fmt.Errorf("attempts must be between 1 and %d", maxReportedAttempts)
	}
	if result.Successes < 0 || result.Successes > result.Attempts {
		return fmt.Errorf("successes must be between 0 and attempts")
	}
	expectedLoss := float64(result.Attempts-result.Successes) / float64(result.Attempts) * 100
	if math.Abs(result.LossRate-expectedLoss) > 1e-6 {
		return fmt.Errorf("lossRate does not match attempts and successes")
	}
	if result.Error != "" {
		if result.Successes != 0 {
			return fmt.Errorf("failed result cannot report successes")
		}
	} else if result.Successes == 0 {
		return fmt.Errorf("successful result must report at least one success")
	}
	return nil
}

type ProfileReport struct {
	ProfileID     string    `json:"profileId"`
	ProfileName   string    `json:"profileName"`
	TargetDomains []string  `json:"targetDomains"`
	ResolvedIPs   []string  `json:"resolvedIps"`
	Results       []Result  `json:"results"`
	Error         string    `json:"error,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	FinishedAt    time.Time `json:"finishedAt"`
}

type Report struct {
	AgentID      string          `json:"agentId"`
	AgentName    string          `json:"agentName"`
	Version      string          `json:"version,omitempty"`
	Carrier      string          `json:"carrier"`
	CarrierLabel string          `json:"carrierLabel"`
	ProbeSource  string          `json:"probeSource"`
	StartedAt    time.Time       `json:"startedAt"`
	FinishedAt   time.Time       `json:"finishedAt"`
	ReceivedAt   time.Time       `json:"receivedAt,omitempty"`
	Profiles     []ProfileReport `json:"profiles"`
}

// FreshnessTime uses controller receipt time; FinishedAt supports older records.
func (r Report) FreshnessTime() time.Time {
	if !r.ReceivedAt.IsZero() {
		return r.ReceivedAt
	}
	return r.FinishedAt
}
