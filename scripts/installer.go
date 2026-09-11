package scripts

import _ "embed"

// AgentInstaller is the same script used by the repository installer URL.
//
//go:embed install-agent.sh
var AgentInstaller []byte
