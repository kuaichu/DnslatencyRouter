package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"dns-latency-router/internal/config"
	"dns-latency-router/internal/mtr"
)

// MTR has a separate poller and worker, so neither tracing nor posting its
// report delays the DNS/probe loop. Only controller-selected IPs are accepted.
func startMTR(cfg *config.Config, baseDir string) func() {
	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{Timeout: 15 * time.Second}
	post := func(result mtr.Result) {
		body, err := json.Marshal(result)
		if err != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, controllerEndpoint(cfg.Agent.ControllerURL, "/api/agent/mtr"), bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+cfg.Agent.Token)
		req.Header.Set("X-Agent-ID", cfg.Agent.ID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[mtr] report delivery failed: %v", err)
			return
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("[mtr] controller returned %s", resp.Status)
		}
	}
	worker, err := mtr.NewWorker(filepath.Join(baseDir, "data", "mtr-agent.json"), func(ctx context.Context, r mtr.Request) mtr.Result { return mtr.Run(ctx, baseDir, r) }, post)
	if err != nil {
		cancel()
		log.Printf("[mtr] worker unavailable: %v", err)
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			jobs, err := fetchMTRJobs(ctx, client, cfg)
			if err == nil {
				for _, result := range worker.Sync(jobs) {
					post(result)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); worker.Close(); <-done }
}

func fetchMTRJobs(ctx context.Context, client *http.Client, cfg *config.Config) ([]mtr.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, controllerEndpoint(cfg.Agent.ControllerURL, "/api/agent/mtr"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Agent.Token)
	req.Header.Set("X-Agent-ID", cfg.Agent.ID)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MTR jobs: %s", resp.Status)
	}
	var jobs []mtr.Request
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&jobs); err != nil {
		return nil, err
	}
	if len(jobs) > 128 {
		return nil, fmt.Errorf("too many MTR jobs")
	}
	for _, r := range jobs {
		if err := r.Validate(); err != nil || r.AgentID != cfg.Agent.ID {
			return nil, fmt.Errorf("invalid MTR job")
		}
	}
	return jobs, nil
}
