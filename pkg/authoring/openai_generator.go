package authoring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const authoringSystemPrompt = `You compile workforce intent into one strict JSON object.
Return exactly: {"candidate":{"agents":[AgentDefinition...],"team":TeamDefinition,"assignments":[{"id":"...","roleId":"...","agentDefinitionId":"...","displayName":"..."}]},"assumptions":["..."],"questions":["..."]}.
Use only the supplied portable OpenSeal schemas and catalog identifiers. Never invent credentials or claim unavailable Skills exist. Never include credential values, API keys, hidden reasoning, markdown, or unknown fields. Use questions for authority, identity, destination, budget, or approval ambiguity. Definitions are immutable: amend mode keeps ids and uses new versions. Use conservative risk, bounded concurrency, calm dynamic coordination, evidence-preserving objectives, and explicit approval policy for external or production effects.`

type OpenAICompatibleGenerator struct {
	endpoint   string
	apiKey     string
	model      string
	httpClient *http.Client
}

func NewOpenAICompatibleGenerator(endpoint, apiKey, model string, httpClient *http.Client) (*OpenAICompatibleGenerator, error) {
	endpoint, model = strings.TrimSpace(endpoint), strings.TrimSpace(model)
	if endpoint == "" || strings.TrimSpace(apiKey) == "" || model == "" {
		return nil, errors.New("authoring endpoint, API key, and model are required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 90 * time.Second}
	}
	return &OpenAICompatibleGenerator{endpoint: endpoint, apiKey: apiKey, model: model, httpClient: httpClient}, nil
}

func (g *OpenAICompatibleGenerator) Generate(ctx context.Context, request GenerateRequest) ([]byte, error) {
	input, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]interface{}{
		"model": g.model,
		"messages": []map[string]string{
			{"role": "system", "content": authoringSystemPrompt},
			{"role": "user", "content": string(input)},
		},
		"response_format": map[string]string{"type": "json_object"},
		"temperature":     0,
	})
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+g.apiKey)
	response, err := g.httpClient.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maximumGenerationBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(responseBody) > maximumGenerationBytes {
		return nil, errors.New("authoring provider response exceeds 1 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("authoring provider returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return nil, fmt.Errorf("decode authoring provider response: %w", err)
	}
	if len(envelope.Choices) != 1 || strings.TrimSpace(envelope.Choices[0].Message.Content) == "" {
		return nil, errors.New("authoring provider must return exactly one non-empty choice")
	}
	return []byte(strings.TrimSpace(envelope.Choices[0].Message.Content)), nil
}
