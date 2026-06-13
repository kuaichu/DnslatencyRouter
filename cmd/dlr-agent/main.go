package main

import (
	"log"
	"os"

	"dns-latency-router/internal/agent"
	"dns-latency-router/internal/config"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("")

	configPath := "agent.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if !cfg.IsAgentMode() {
		log.Fatalf("agent binary requires node_role: agent in %s", configPath)
	}

	agent.Run(cfg)
}
