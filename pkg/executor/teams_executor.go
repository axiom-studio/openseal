package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const NodeTypeTeams = "teams"

// TeamsExecutor sends messages to Microsoft Teams via webhook
type TeamsExecutor struct {
	client *http.Client
}

func NewTeamsExecutor() *TeamsExecutor {
	return &TeamsExecutor{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (e *TeamsExecutor) Type() string {
	return NodeTypeTeams
}

func (e *TeamsExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("teams step requires config")
	}

	webhookURL, _ := config["webhookUrl"].(string)
	if webhookURL == "" {
		return nil, fmt.Errorf("teams step requires 'webhookUrl'")
	}
	webhookURL = resolver.ResolveString(webhookURL)

	// Build Teams Adaptive Card or simple message
	payload := make(map[string]interface{})

	// Check if using Adaptive Card
	if card, ok := config["card"].(map[string]interface{}); ok {
		payload = resolver.ResolveMap(card)
	} else {
		// Simple MessageCard format
		payload["@type"] = "MessageCard"
		payload["@context"] = "http://schema.org/extensions"

		if title, ok := config["title"].(string); ok && title != "" {
			payload["title"] = resolver.ResolveString(title)
		}

		if text, ok := config["text"].(string); ok && text != "" {
			payload["text"] = resolver.ResolveString(text)
		}

		if themeColor, ok := config["themeColor"].(string); ok && themeColor != "" {
			payload["themeColor"] = resolver.ResolveString(themeColor)
		}

		if summary, ok := config["summary"].(string); ok && summary != "" {
			payload["summary"] = resolver.ResolveString(summary)
		}

		if sections, ok := config["sections"].([]interface{}); ok && len(sections) > 0 {
			resolvedSections := resolver.ResolveMap(map[string]interface{}{"sections": sections})
			payload["sections"] = resolvedSections["sections"]
		}

		if actions, ok := config["actions"].([]interface{}); ok && len(actions) > 0 {
			resolvedActions := resolver.ResolveMap(map[string]interface{}{"potentialAction": actions})
			payload["potentialAction"] = resolvedActions["potentialAction"]
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal teams payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create teams request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("teams request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	success := resp.StatusCode >= 200 && resp.StatusCode < 300

	return &StepResult{
		Output: map[string]interface{}{
			"success":    success,
			"statusCode": resp.StatusCode,
			"response":   string(respBody),
		},
	}, nil
}
