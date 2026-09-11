package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstallerServedWithoutGitHubRedirect(t *testing.T) {
	r := httptest.NewRecorder()
	(&Server{}).handleAPIAgentInstallScript(r, httptest.NewRequest(http.MethodGet, agentInstallerPath, nil))
	if r.Code != http.StatusOK || r.Header().Get("Location") != "" {
		t.Fatalf("installer must be served locally: status=%d location=%q", r.Code, r.Header().Get("Location"))
	}
	body := strings.ReplaceAll(r.Body.String(), "\r\n", "\n")
	if !strings.HasPrefix(body, "#!/usr/bin/env bash\n") || !strings.Contains(body, "CONTROLLER_DOWNLOAD_URL=") {
		t.Fatal("response is not the complete installer")
	}
}

func TestInstallCommandDownloadsScriptFromController(t *testing.T) {
	command := buildAgentInstallCommand("http://172.23.93.195:19198/", "test-token", 300)
	if !strings.Contains(command, "'http://172.23.93.195:19198/api/agent/install.sh'") || strings.Contains(command, "github") {
		t.Fatalf("unexpected installer source: %s", command)
	}
}
