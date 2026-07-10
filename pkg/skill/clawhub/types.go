package clawhub

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

func (s *SkillSummary) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Slug = textField(raw, "slug")
	s.Name = firstTextField(raw, "name", "displayName")
	s.Description = firstTextField(raw, "description", "summary")
	s.Icon = textField(raw, "icon")
	s.Tags = stringFields(raw["tags"])
	s.Downloads = intField(raw, "downloads")
	s.Stars = intField(raw, "stars")
	s.UpdatedAt = timeField(raw, "updatedAt")
	if latest, ok := raw["latestVersion"].(map[string]interface{}); ok && s.Name == "" {
		s.Name = textField(latest, "displayName")
	}
	if stats, ok := raw["stats"].(map[string]interface{}); ok {
		if s.Downloads == 0 {
			s.Downloads = intField(stats, "downloads")
		}
		if s.Stars == 0 {
			s.Stars = intField(stats, "stars")
		}
	}
	return nil
}

type SkillDetail struct {
	SkillSummary
	Version     string   `json:"version"`
	Files       []string `json:"files"`
	ManifestRaw string   `json:"manifestRaw"`
	Author      string   `json:"author"`
	Owner       string   `json:"owner,omitempty"`
}

func (d *SkillDetail) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	summaryRaw := raw
	if wrapped, ok := raw["skill"].(map[string]interface{}); ok {
		summaryRaw = wrapped
	}
	encoded, _ := json.Marshal(summaryRaw)
	if err := json.Unmarshal(encoded, &d.SkillSummary); err != nil {
		return err
	}
	d.Version = textField(raw, "version")
	d.Files = stringFields(raw["files"])
	d.ManifestRaw = textField(raw, "manifestRaw")
	d.Author = textField(raw, "author")
	if latest, ok := raw["latestVersion"].(map[string]interface{}); ok && d.Version == "" {
		d.Version = textField(latest, "version")
	}
	if owner, ok := raw["owner"].(map[string]interface{}); ok {
		d.Owner = textField(owner, "handle")
		if d.Author == "" {
			d.Author = firstTextField(owner, "displayName", "handle")
		}
	}
	return nil
}

func textField(values map[string]interface{}, key string) string {
	value, _ := values[key].(string)
	return value
}

func firstTextField(values map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value := textField(values, key); value != "" {
			return value
		}
	}
	return ""
}

func intField(values map[string]interface{}, key string) int {
	switch value := values[key].(type) {
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	case string:
		parsed, _ := strconv.Atoi(value)
		return parsed
	default:
		return 0
	}
}

func stringFields(value interface{}) []string {
	switch typed := value.(type) {
	case []interface{}:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	case map[string]interface{}:
		result := make([]string, 0, len(typed))
		for key, value := range typed {
			if enabled, ok := value.(bool); ok && enabled {
				result = append(result, key)
			}
		}
		return result
	default:
		return nil
	}
}

func timeField(values map[string]interface{}, key string) time.Time {
	switch value := values[key].(type) {
	case string:
		parsed, _ := time.Parse(time.RFC3339, value)
		return parsed
	case float64:
		return time.UnixMilli(int64(value))
	default:
		return time.Time{}
	}
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
