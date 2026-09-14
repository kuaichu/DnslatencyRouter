// Package mtr runs bounded NextTrace reports independently of routing probes.
package mtr

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"dns-latency-router/internal/checker"
)

const MaxOutput = 32 * 1024
const RunTimeout = 90 * time.Second

type Request struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	ProfileID string `json:"profileId"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
}

type Result struct {
	Request
	Status     string    `json:"status"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

func (r Request) Validate() error {
	if r.ID == "" || len(r.ID) > 100 || r.AgentID == "" || len(r.AgentID) > 128 || len(r.ProfileID) > 128 {
		return fmt.Errorf("invalid trace identity")
	}
	if !checker.IsUsableCandidateIP(r.IP) || r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("invalid trace target")
	}
	return nil
}

func BinaryName(platform string) string {
	switch platform {
	case "linux-amd64", "linux-arm64", "darwin-arm64":
		return "nexttrace_" + strings.ReplaceAll(platform, "-", "_")
	case "windows-amd64":
		return "nexttrace_windows_amd64.exe"
	}
	return ""
}

func FindBinary(baseDir string) (string, error) {
	name := BinaryName(runtime.GOOS + "-" + runtime.GOARCH)
	nativeName := "nexttrace"
	if runtime.GOOS == "windows" {
		nativeName += ".exe"
	}
	for _, dir := range []string{baseDir, executableDir(), "."} {
		for _, candidate := range []string{filepath.Join(dir, "tools", "nexttrace", name), filepath.Join(dir, nativeName)} {
			if name != "" {
				if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
					return filepath.Abs(candidate)
				}
			}
		}
	}
	if path, err := exec.LookPath(nativeName); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("NextTrace is not installed; place %s in tools/nexttrace", name)
}

func executableDir() string {
	path, _ := os.Executable()
	return filepath.Dir(path)
}

func Arguments(r Request) []string {
	// Use NextTrace's regular traceroute renderer so the dashboard shows the
	// hop-by-hop route (the same form users get from `nexttrace <ip>`), while
	// keeping a bounded number of samples and hops for background jobs.
	return []string{"--ipv4", "--tcp", "--port", strconv.Itoa(r.Port),
		"--queries", "10", "--max-hops", "30", "--ttl-time", "1000", "--timeout", "1500",
		"--no-color", "--map", "--data-provider", "NextTrace-API", r.IP}
}

// limitWriter drains the child even when the retained output limit is reached.
type limitWriter struct{ text []byte }

func (w *limitWriter) Write(p []byte) (int, error) {
	n := len(p)
	remaining := MaxOutput - len(w.text)
	if len(p) > remaining {
		p = p[:remaining]
	}
	w.text = append(w.text, p...)
	return n, nil
}

func Run(ctx context.Context, baseDir string, request Request) Result {
	r := Result{Request: request, Status: "failed", StartedAt: time.Now()}
	if err := request.Validate(); err != nil {
		r.Error = err.Error()
		r.FinishedAt = time.Now()
		return r
	}
	path, err := FindBinary(baseDir)
	if err != nil {
		r.Error = err.Error()
		r.FinishedAt = time.Now()
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, Arguments(request)...)
	cmd.Dir = baseDir
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "TERM=dumb")
	hideWindow(cmd)
	var stdout, stderr limitWriter
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	r.FinishedAt = time.Now()
	r.Output = strings.TrimSpace(strings.ToValidUTF8(string(stdout.text), ""))
	// NextTrace can return exit code zero on CLI or privilege errors. Accept
	// both the current hop-by-hop renderer and legacy cached MTR reports while
	// old results are being replaced by the trace-key format bump.
	validReport := validOutput(r.Output)
	if ctx.Err() != nil {
		r.Error = "MTR exceeded the 90-second limit or was cancelled"
	} else if err != nil {
		r.Error = fmt.Sprintf("NextTrace failed: %v", err)
	} else if !validReport {
		r.Error = "NextTrace produced no MTR report; check raw-socket privileges and installation"
	} else {
		r.Status = "completed"
	}
	if r.Status == "failed" && len(stderr.text) > 0 {
		r.Error += ": " + strings.TrimSpace(strings.ToValidUTF8(string(stderr.text), ""))
	}
	if len(r.Error) > 1000 {
		r.Error = r.Error[:1000]
	}
	return r
}

func validOutput(output string) bool {
	return (strings.Contains(output, "NextTrace v") && strings.Contains(output, "->") && strings.Contains(output, "hops max")) ||
		(strings.Contains(output, "Loss%") && strings.Contains(output, "Snt") && strings.Contains(output, "HOST:"))
}

var _ io.Writer = (*limitWriter)(nil)
