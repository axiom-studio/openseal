package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxProviderRequestBytes bounds one request body. A step re-sends the
	// whole session path, so it is larger than a single old per-Turn form.
	maxProviderRequestBytes  = 8 << 20
	maxProviderResponseBytes = 8 << 20
	// providerStepTimeout bounds one model call (D13 bounds a step, not a Run).
	providerStepTimeout = 5 * time.Minute
	// providerErrorBodyBytes is how much of a non-2xx body is kept for
	// classification. It is never shown to people.
	providerErrorBodyBytes = 64 << 10
	// defaultContextWindowTokens is the compaction window when the operator
	// configures none (DI-6). It is not an output cap.
	defaultContextWindowTokens = 128000
)

// OpenAIProviderConfig is the single desktop/standalone model provider.
type OpenAIProviderConfig struct {
	Endpoint            string // base URL; "/chat/completions" appended when missing
	Key                 string
	Model               string
	ContextWindowTokens int64 // 0 => defaultContextWindowTokens
}

// openAIClient is the transport the step host and the Team router share:
// one OpenAI-compatible Chat Completions endpoint with a bearer credential.
type openAIClient struct {
	endpoint, key, model string
	window               int64
	http                 *http.Client // no client timeout; each call has a deadline
}

var errInvalidProviderConfig = errors.New("task provider requires a valid HTTP API URL, credential, and model")

// errProviderRequestTooLarge is returned by post, before sending, for a body
// over maxProviderRequestBytes.
var errProviderRequestTooLarge = errors.New("provider request exceeds the size limit")

// errProviderResponseTooLarge is returned by post for a 2xx body over
// maxProviderResponseBytes.
var errProviderResponseTooLarge = errors.New("provider response exceeds the size limit")

// providerHTTPError is a non-2xx provider answer. Body is the first
// providerErrorBodyBytes, for classification only.
type providerHTTPError struct {
	Status int
	Body   []byte
}

func (e *providerHTTPError) Error() string {
	return fmt.Sprintf("provider returned HTTP %d", e.Status)
}

func newOpenAIClient(cfg OpenAIProviderConfig) (*openAIClient, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.TrimSpace(cfg.Key) == "" || strings.TrimSpace(cfg.Model) == "" || cfg.ContextWindowTokens < 0 {
		return nil, errInvalidProviderConfig
	}
	endpoint := strings.TrimRight(u.String(), "/")
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	window := cfg.ContextWindowTokens
	if window == 0 {
		window = defaultContextWindowTokens
	}
	return &openAIClient{
		endpoint: endpoint, key: cfg.Key, model: strings.TrimSpace(cfg.Model), window: window,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

// post sends one JSON body and returns the raw 2xx body. A non-2xx answer is
// a *providerHTTPError; a redirect is never followed (the credential stays
// with the configured host) and comes back as a *providerHTTPError too.
// Transport errors are returned as they are.
func (c *openAIClient) post(ctx context.Context, body []byte) ([]byte, error) {
	if len(body) > maxProviderRequestBytes {
		return nil, errProviderRequestTooLarge
	}
	ctx, cancel := context.WithTimeout(ctx, providerStepTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, providerErrorBodyBytes))
		return nil, &providerHTTPError{Status: res.StatusCode, Body: raw}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxProviderResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxProviderResponseBytes {
		return nil, errProviderResponseTooLarge
	}
	return raw, nil
}
