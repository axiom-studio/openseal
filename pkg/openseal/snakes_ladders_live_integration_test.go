//go:build integration

package openseal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type openAICompatibleMoveCommentator struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
	mu       sync.Mutex
	calls    int
}

func newOpenAICompatibleMoveCommentator(endpoint, apiKey, model string, client *http.Client) (*openAICompatibleMoveCommentator, error) {
	endpoint, apiKey, model = strings.TrimSpace(endpoint), strings.TrimSpace(apiKey), strings.TrimSpace(model)
	if endpoint == "" || apiKey == "" || model == "" {
		return nil, errors.New("live move commentator requires endpoint, API key, and model")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errors.New("live move commentator endpoint must be an absolute HTTPS URL")
	}
	if !strings.HasSuffix(strings.TrimRight(endpoint, "/"), "/chat/completions") {
		endpoint = strings.TrimRight(endpoint, "/") + "/chat/completions"
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &openAICompatibleMoveCommentator{endpoint: endpoint, apiKey: apiKey, model: model, client: client}, nil
}

func (c *openAICompatibleMoveCommentator) Identity() (string, string) {
	return "openai-compatible", c.model
}

func (c *openAICompatibleMoveCommentator) SensitiveValues() []string { return []string{c.apiKey} }

func (c *openAICompatibleMoveCommentator) AcceptanceTimeout() time.Duration { return 3 * time.Minute }

func (c *openAICompatibleMoveCommentator) Comment(ctx context.Context, move gameMove) (string, TurnUsage, error) {
	prompt := fmt.Sprintf("You are %s in a governed Snakes and Ladders Team. The authoritative referee says move %d rolled %d and moved from %d to %d", move.Player, move.Number, move.Roll, move.From, move.To)
	if move.Effect != "" {
		prompt += " via " + move.Effect
	}
	prompt += ". Reply with one calm first-person sentence acknowledging only these supplied facts. Do not choose dice, alter the board, claim victory unless position 30 was reached, or add hidden reasoning."
	payload, err := json.Marshal(map[string]interface{}{
		"model": c.model, "temperature": 0, "max_tokens": 2048,
		"messages": []map[string]string{
			{"role": "system", "content": "You communicate concise outcomes from authoritative deterministic tools. Tool state is final."},
			{"role": "user", "content": prompt},
		},
	})
	if err != nil {
		return "", TurnUsage{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", TurnUsage{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	started := time.Now()
	response, err := c.client.Do(request)
	if err != nil {
		return "", TurnUsage{}, errors.New("live model commentary request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", TurnUsage{}, errors.New("live model commentary response could not be read")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", TurnUsage{}, fmt.Errorf("live model commentary returned HTTP %d", response.StatusCode)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", TurnUsage{}, errors.New("live model commentary returned invalid JSON")
	}
	if len(decoded.Choices) != 1 {
		return "", TurnUsage{}, fmt.Errorf("live model commentary returned %d choices", len(decoded.Choices))
	}
	if strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return "", TurnUsage{}, errors.New("live model commentary returned empty content")
	}
	comment := strings.TrimSpace(decoded.Choices[0].Message.Content)
	if strings.Contains(comment, c.apiKey) {
		return "", TurnUsage{}, errors.New("live model commentary exposed a transport credential")
	}
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return comment, TurnUsage{
		InputTokens: decoded.Usage.PromptTokens, OutputTokens: decoded.Usage.CompletionTokens,
		DurationMS: time.Since(started).Milliseconds(),
	}, nil
}

func TestLiveThreeAgentSnakesAndLaddersUsesBoundedModelCommentary(t *testing.T) {
	endpoint := os.Getenv("OPENSEAL_LLM_BASE_URL")
	apiKey := os.Getenv("OPENAI_API_KEY")
	model := os.Getenv("OPENSEAL_LLM_MODEL")
	if endpoint == "" || apiKey == "" || model == "" {
		t.Skip("OpenSeal live provider is not configured")
	}
	commentator, err := newOpenAICompatibleMoveCommentator(endpoint, apiKey, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	runSnakesAndLaddersAcceptance(t, commentator)
	commentator.mu.Lock()
	calls := commentator.calls
	commentator.mu.Unlock()
	if calls != len(gameDice) {
		t.Fatalf("live commentary calls = %d, want %d", calls, len(gameDice))
	}
}
