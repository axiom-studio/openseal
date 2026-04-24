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

const NodeTypeDiscord = "discord"

// DiscordExecutor sends messages to Discord via webhook
type DiscordExecutor struct {
	client *http.Client
}

func NewDiscordExecutor() *DiscordExecutor {
	return &DiscordExecutor{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (e *DiscordExecutor) Type() string {
	return NodeTypeDiscord
}

func (e *DiscordExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("discord step requires config")
	}

	webhookURL, _ := config["webhookUrl"].(string)
	if webhookURL == "" {
		return nil, fmt.Errorf("discord step requires 'webhookUrl'")
	}
	webhookURL = resolver.ResolveString(webhookURL)

	payload := make(map[string]interface{})

	if content, ok := config["content"].(string); ok && content != "" {
		payload["content"] = resolver.ResolveString(content)
	}

	if username, ok := config["username"].(string); ok && username != "" {
		payload["username"] = resolver.ResolveString(username)
	}

	if avatarURL, ok := config["avatarUrl"].(string); ok && avatarURL != "" {
		payload["avatar_url"] = resolver.ResolveString(avatarURL)
	}

	if embeds, ok := config["embeds"].([]interface{}); ok && len(embeds) > 0 {
		resolvedEmbeds := resolver.ResolveMap(map[string]interface{}{"embeds": embeds})
		payload["embeds"] = resolvedEmbeds["embeds"]
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal discord payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discord request failed: %w", err)
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
