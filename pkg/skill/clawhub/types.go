package clawhub

import (
	"errors"
	"fmt"
	"time"
)

type SkillSummary struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Icon        string    `json:"icon,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	Downloads   int       `json:"downloads"`
	Stars       int       `json:"stars"`
	UpdatedAt   time.Time `json:"updatedAt"`
	IsStale     bool      `json:"isStale,omitempty"`
}

type SkillDetail struct {
	SkillSummary
	Version     string   `json:"version"`
	Files       []string `json:"files"`
	ManifestRaw string   `json:"manifestRaw"`
	Author      string   `json:"author"`
}

type ClawHubError struct {
	StatusCode int
	Message    string
	RetryAfter int
}

func (e *ClawHubError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("clawhub API error %d: %s (retry after %ds)", e.StatusCode, e.Message, e.RetryAfter)
	}
	return fmt.Sprintf("clawhub API error %d: %s", e.StatusCode, e.Message)
}

var (
	ErrNotFound    = &ClawHubError{StatusCode: 404, Message: "not found"}
	ErrRateLimit   = &ClawHubError{StatusCode: 429, Message: "rate limited"}
	ErrServerError = &ClawHubError{StatusCode: 500, Message: "server error"}
)

func IsNotFoundError(err error) bool {
	var clawErr *ClawHubError
	if errors.As(err, &clawErr) {
		return clawErr.StatusCode == 404
	}
	return false
}

func IsRateLimitError(err error) bool {
	var clawErr *ClawHubError
	if errors.As(err, &clawErr) {
		return clawErr.StatusCode == 429
	}
	return false
}

func IsServerError(err error) bool {
	var clawErr *ClawHubError
	if errors.As(err, &clawErr) {
		return clawErr.StatusCode >= 500
	}
	return false
}
