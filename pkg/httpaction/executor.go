package httpaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20

// RequestPolicy is the embedding host's mandatory tenant-aware egress gate.
// OpenSeal validates the immutable request envelope; the host decides whether
// this deployment may contact that origin.
type RequestPolicy interface {
	AuthorizeHTTPRequest(context.Context, Invocation) error
}

type RequestPolicyFunc func(context.Context, Invocation) error

func (f RequestPolicyFunc) AuthorizeHTTPRequest(ctx context.Context, invocation Invocation) error {
	return f(ctx, invocation)
}

// Executor performs one bounded, non-redirecting read request after explicit
// host authorization. Credential values are injected only into the wire
// request and are scrubbed from returned payloads and errors.
type Executor struct {
	client *http.Client
	policy RequestPolicy
	now    func() time.Time
}

func NewExecutor(transport http.RoundTripper, policy RequestPolicy) (*Executor, error) {
	if policy == nil {
		return nil, errors.New("HTTP action request policy is required")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Executor{
		client: &http.Client{
			Transport: transport, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		policy: policy, now: time.Now,
	}, nil
}

func (e *Executor) Execute(ctx context.Context, invocation Invocation, credential string) (map[string]interface{}, error) {
	if e == nil || e.client == nil || e.policy == nil {
		return nil, errors.New("HTTP action executor is not configured")
	}
	if strings.TrimSpace(credential) == "" {
		return nil, errors.New("HTTP action credential is required")
	}
	decoded, err := DecodeInvocation(map[string]interface{}{
		BaseURLKey: invocation.BaseURL, MethodKey: invocation.Method, PathKey: invocation.Path,
		ParametersKey: invocation.Parameters, ParameterContractKey: invocation.ParameterContract,
		CredentialNameKey: invocation.CredentialName, CredentialParameterKey: invocation.CredentialParameter,
	})
	if err != nil {
		return nil, err
	}
	if err := e.policy.AuthorizeHTTPRequest(ctx, *decoded); err != nil {
		return nil, fmt.Errorf("HTTP action request was not authorized: %w", err)
	}
	requestURL, parameterNames, err := materializeURL(*decoded, credential)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, decoded.Method, requestURL.String(), nil)
	if err != nil {
		return nil, errors.New("build governed HTTP action request")
	}
	request.Header.Set("Accept", "application/json")
	response, err := e.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("governed HTTP action request failed: %s", redactString(err.Error(), credential))
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, errors.New("governed HTTP action redirects are not allowed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return nil, errors.New("governed HTTP action response is unavailable or exceeds 4 MiB")
	}
	var decodedBody interface{}
	if err := json.Unmarshal(body, &decodedBody); err != nil {
		return nil, errors.New("governed HTTP action returned a non-JSON response")
	}
	decodedBody = redactValue(decodedBody, credential)
	result := map[string]interface{}{
		"statusCode": response.StatusCode,
		"body":       decodedBody,
		"provenance": map[string]interface{}{
			"origin": requestURL.Scheme + "://" + requestURL.Host, "method": decoded.Method,
			"pathTemplate": decoded.Path, "parameterNames": parameterNames, "observedAt": e.now().UTC().Format(time.RFC3339Nano),
		},
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("governed HTTP action returned status %d", response.StatusCode)
	}
	return result, nil
}

func materializeURL(invocation Invocation, credential string) (*url.URL, []string, error) {
	base, _ := url.Parse(invocation.BaseURL)
	path := invocation.Path
	query := url.Values{}
	names := make([]string, 0, len(invocation.ParameterContract))
	for _, parameter := range invocation.ParameterContract {
		value, ok := invocation.Parameters[parameter.Name]
		if !ok {
			value, ok = parameter.Default, parameter.Default != nil
		}
		if !ok {
			continue
		}
		names = append(names, parameter.Name)
		switch parameter.Location {
		case "path":
			marker := "{" + parameter.Name + "}"
			if !strings.Contains(path, marker) {
				return nil, nil, fmt.Errorf("HTTP action path does not contain declared parameter %s", parameter.Name)
			}
			path = strings.ReplaceAll(path, marker, url.PathEscape(fmt.Sprint(value)))
		case "query":
			appendQueryValue(query, parameter.Name, value)
		default:
			return nil, nil, fmt.Errorf("HTTP action parameter %s has unsupported location", parameter.Name)
		}
	}
	if strings.Contains(path, "{") || strings.Contains(path, "}") {
		return nil, nil, errors.New("HTTP action path contains unresolved parameters")
	}
	query.Set(invocation.CredentialParameter, credential)
	base.Path = path
	base.RawQuery = query.Encode()
	sort.Strings(names)
	return base, names, nil
}

func appendQueryValue(query url.Values, name string, value interface{}) {
	switch typed := value.(type) {
	case []interface{}:
		for _, item := range typed {
			appendQueryValue(query, name, item)
		}
	case []string:
		for _, item := range typed {
			query.Add(name, item)
		}
	case map[string]interface{}:
		encoded, _ := json.Marshal(typed)
		query.Add(name, string(encoded))
	default:
		query.Add(name, fmt.Sprint(value))
	}
}

func redactValue(value interface{}, credential string) interface{} {
	switch typed := value.(type) {
	case string:
		return redactString(typed, credential)
	case []interface{}:
		result := make([]interface{}, len(typed))
		for index, child := range typed {
			result[index] = redactValue(child, credential)
		}
		return result
	case map[string]interface{}:
		result := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			result[key] = redactValue(child, credential)
		}
		return result
	default:
		return typed
	}
}

func redactString(value, credential string) string {
	if credential == "" {
		return value
	}
	return strings.ReplaceAll(value, credential, "[REDACTED]")
}
