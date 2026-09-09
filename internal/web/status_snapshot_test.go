package web

import (
	"testing"
)

func TestStatusSnapshotsDoNotAliasPublishedState(t *testing.T) {
	s := &Server{}
	input := &Status{Agents: []AgentStatus{{ID: "agent-a"}}, Profiles: []ProfileStatus{{
		ID: "airport", TargetDomains: []string{"entry.example"},
		Regions: []RegionStatus{{CurrentIP: "1.1.1.1"}},
	}}}
	s.UpdateStatus(input)
	input.Profiles[0].Regions[0].CurrentIP = "8.8.8.8"
	copy := s.GetStatus()
	copy.Agents[0].ID = "changed"
	copy.Profiles[0].TargetDomains[0] = "changed.example"
	copy.Profiles[0].Regions[0].CurrentIP = "9.9.9.9"
	current := s.GetStatus()
	if current.Agents[0].ID != "agent-a" || current.Profiles[0].TargetDomains[0] != "entry.example" || current.Profiles[0].Regions[0].CurrentIP != "1.1.1.1" {
		t.Fatalf("snapshot aliases published state: %+v", current)
	}
}
