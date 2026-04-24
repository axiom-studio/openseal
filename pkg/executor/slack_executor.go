package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

var slackLogger *zap.SugaredLogger

func init() {
	logger, _ := zap.NewProduction()
	slackLogger = logger.Sugar().Named("slack-executor")
}

const NodeTypeSlack = "slack"

// SlackExecutor sends messages to Slack via webhook
type SlackExecutor struct {
	client *http.Client
}

func NewSlackExecutor() *SlackExecutor {
	return &SlackExecutor{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (e *SlackExecutor) Type() string {
	return NodeTypeSlack
}

func (e *SlackExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	slackLogger.Debugw("executing slack step", "stepName", step.Name)

	config := step.Config
	if config == nil {
		slackLogger.Debug("slack step missing config")
		return nil, fmt.Errorf("slack step requires config")
	}

	// Resolve webhook URL
	webhookURL, _ := config["webhookUrl"].(string)
	if webhookURL == "" {
		slackLogger.Debug("slack step missing webhookUrl")
		return nil, fmt.Errorf("slack step requires 'webhookUrl'")
	}
	webhookURL = resolver.ResolveString(webhookURL)
	slackLogger.Debugw("resolved webhook URL", "urlMasked", maskWebhookURL(webhookURL))

	// Build payload
	payload := make(map[string]interface{})

	// Text message
	if text, ok := config["text"].(string); ok && text != "" {
		payload["text"] = resolver.ResolveString(text)
	}

	// Channel override
	if channel, ok := config["channel"].(string); ok && channel != "" {
		payload["channel"] = resolver.ResolveString(channel)
		slackLogger.Debugw("channel override", "channel", payload["channel"])
	}

	// Username override
	if username, ok := config["username"].(string); ok && username != "" {
		payload["username"] = resolver.ResolveString(username)
	}

	// Icon emoji
	if iconEmoji, ok := config["iconEmoji"].(string); ok && iconEmoji != "" {
		payload["icon_emoji"] = resolver.ResolveString(iconEmoji)
	}

	// Blocks for rich formatting
	if blocks, ok := config["blocks"].([]interface{}); ok && len(blocks) > 0 {
		resolvedBlocks := resolver.ResolveMap(map[string]interface{}{"blocks": blocks})
		payload["blocks"] = resolvedBlocks["blocks"]
		slackLogger.Debugw("using blocks", "blockCount", len(blocks))
	}

	// Attachments
	if attachments, ok := config["attachments"].([]interface{}); ok && len(attachments) > 0 {
		resolvedAttachments := resolver.ResolveMap(map[string]interface{}{"attachments": attachments})
		payload["attachments"] = resolvedAttachments["attachments"]
		slackLogger.Debugw("using attachments", "attachmentCount", len(attachments))
	}

	// Send request
	body, err := json.Marshal(payload)
	if err != nil {
		slackLogger.Errorw("failed to marshal slack payload", "err", err)
		return nil, fmt.Errorf("failed to marshal slack payload: %w", err)
	}
	slackLogger.Debugw("sending slack message", "payloadSize", len(body))

	req, err := http.NewRequestWithContext(ctx, "POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		slackLogger.Errorw("failed to create slack request", "err", err)
		return nil, fmt.Errorf("failed to create slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		slackLogger.Errorw("slack request failed", "err", err)
		return nil, fmt.Errorf("slack request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	slackLogger.Debugw("slack response received",
		"statusCode", resp.StatusCode,
		"success", success,
		"responseBody", string(respBody),
	)

	if !success {
		slackLogger.Warnw("slack webhook returned non-success status",
			"statusCode", resp.StatusCode,
			"response", string(respBody),
		)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"success":    success,
			"statusCode": resp.StatusCode,
			"response":   string(respBody),
		},
	}, nil
}

// maskWebhookURL masks the webhook URL for safe logging
func maskWebhookURL(url string) string {
	if len(url) < 20 {
		return "***"
	}
	// Show first 30 chars and last 10 chars
	if strings.Contains(url, "hooks.slack.com") {
		parts := strings.Split(url, "/")
		if len(parts) > 3 {
			return strings.Join(parts[:4], "/") + "/***"
		}
	}
	return url[:30] + "***" + url[len(url)-10:]
}
