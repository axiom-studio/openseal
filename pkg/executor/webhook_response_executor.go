/*
 * Copyright (c) 2025. Axiom Studio
 */

package executor

import (
	"context"
	"fmt"
	"strconv"
	"sync"
)

const (
	NodeTypeWebhookResponse = "webhook_response"
)

// WebhookResponseStore stores webhook responses for sync mode
var webhookResponseStore = &WebhookResponseStoreStruct{
	responses: make(map[string]*WebhookResponseData),
}

type WebhookResponseStoreStruct struct {
	mu        sync.RWMutex
	responses map[string]*WebhookResponseData
}

type WebhookResponseData struct {
	StatusCode  int
	Headers     map[string]string
	Body        interface{}
	ContentType string
}

// SetWebhookResponse stores a webhook response
func SetWebhookResponse(runId string, response *WebhookResponseData) {
	webhookResponseStore.mu.Lock()
	defer webhookResponseStore.mu.Unlock()
	webhookResponseStore.responses[runId] = response
}

// GetWebhookResponse retrieves and removes a webhook response
func GetWebhookResponse(runId string) *WebhookResponseData {
	webhookResponseStore.mu.Lock()
	defer webhookResponseStore.mu.Unlock()
	response := webhookResponseStore.responses[runId]
	delete(webhookResponseStore.responses, runId)
	return response
}

// WebhookResponseExecutor sends HTTP response back to webhook caller
// Only works in sync webhook mode
type WebhookResponseExecutor struct{}

// NewWebhookResponseExecutor creates a new webhook response executor
func NewWebhookResponseExecutor() *WebhookResponseExecutor {
	return &WebhookResponseExecutor{}
}

func (e *WebhookResponseExecutor) Type() string {
	return NodeTypeWebhookResponse
}

func (e *WebhookResponseExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("webhook_response requires config")
	}

	// Get status code
	statusCode := 200
	if sc, ok := config["statusCode"]; ok {
		switch v := sc.(type) {
		case int:
			statusCode = v
		case float64:
			statusCode = int(v)
		case string:
			if i, err := strconv.Atoi(v); err == nil {
				statusCode = i
			}
		}
	}

	// Get content type
	contentType := "application/json"
	if ct, ok := config["contentType"].(string); ok && ct != "" {
		contentType = ct
	}

	// Get body
	var body interface{}
	if b, ok := config["body"]; ok {
		// Resolve templates in body if it's a string
		if str, ok := b.(string); ok {
			body = resolver.ResolveString(str)
		} else {
			body = b
		}
	}

	// Get headers
	headers := make(map[string]string)
	if h, ok := config["headers"].(map[string]interface{}); ok {
		for k, v := range h {
			if str, ok := v.(string); ok {
				headers[k] = resolver.ResolveString(str)
			}
		}
	}

	// Get run ID from context (if available)
	runId := ""
	if cp, ok := resolver.(ContextProvider); ok {
		ctxData := cp.GetContextData()
		if run, ok := ctxData["run"].(map[string]interface{}); ok {
			if id, ok := run["runId"].(string); ok {
				runId = id
			} else if id, ok := run["runId"].(int); ok {
				runId = fmt.Sprintf("%d", id)
			}
		}
	}

	// Store the response if we have a run ID
	if runId != "" {
		SetWebhookResponse(runId, &WebhookResponseData{
			StatusCode:  statusCode,
			Headers:     headers,
			Body:        body,
			ContentType: contentType,
		})
	}

	return &StepResult{
		Output: map[string]interface{}{
			"statusCode":  statusCode,
			"contentType": contentType,
			"body":        body,
			"headers":     headers,
		},
	}, nil
}