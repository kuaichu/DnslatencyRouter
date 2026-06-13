package agent

import "time"

type ProfileJob struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Slug          string   `json:"slug"`
	TargetDomains []string `json:"targetDomains"`
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
	IP        string  `json:"ip"`
	Latency   float64 `json:"latency"`
	Jitter    float64 `json:"jitter"`
	LossRate  float64 `json:"lossRate"`
	Attempts  int     `json:"attempts"`
	Successes int     `json:"successes"`
	Score     float64 `json:"score"`
	Error     string  `json:"error,omitempty"`
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
	Carrier      string          `json:"carrier"`
	CarrierLabel string          `json:"carrierLabel"`
	ProbeSource  string          `json:"probeSource"`
	StartedAt    time.Time       `json:"startedAt"`
	FinishedAt   time.Time       `json:"finishedAt"`
	Profiles     []ProfileReport `json:"profiles"`
}
