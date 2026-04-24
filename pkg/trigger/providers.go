package trigger

import (
	"os"
)

// GetBaseURL returns the base URL for webhooks from environment
func GetBaseURL() string {
	baseURL := os.Getenv("AGENT_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	return baseURL
}
