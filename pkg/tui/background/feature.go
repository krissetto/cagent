// Package background provides background agents feature support.
package background

import (
	"os"
)

const (
	// EnvBackgroundAgents is the environment variable to enable background agents.
	EnvBackgroundAgents = "CAGENT_EXPERIMENTAL_BACKGROUND_AGENTS"
)

// IsEnabled returns true if the background agents feature is enabled.
func IsEnabled() bool {
	return os.Getenv(EnvBackgroundAgents) == "1"
}
