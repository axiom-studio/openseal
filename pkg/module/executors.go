package module

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Core executor implementations

func ifExecute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("if step requires config")
	}

	condition, ok := config["condition"].(string)
	if !ok {
		return nil, fmt.Errorf("if step requires 'condition' string")
	}

	thenStep, _ := config["then"].(string)
	elseStep, _ := config["else"].(string)

	result := resolver.EvaluateCondition(condition)

	output := map[string]interface{}{
		"condition": condition,
		"result":    result,
	}

	var nextStep string
	if result {
		nextStep = thenStep
		output["branch"] = "then"
	} else {
		nextStep = elseStep
		output["branch"] = "else"
	}

	return &StepResult{Output: output, NextStep: nextStep}, nil
}

func switchExecute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("switch step requires config")
	}

	expression, ok := config["expression"].(string)
	if !ok {
		return nil, fmt.Errorf("switch step requires 'expression' string")
	}

	cases, _ := config["cases"].(map[string]interface{})
	defaultStep, _ := config["default"].(string)

	value := resolver.ResolveString(expression)
	output := map[string]interface{}{"expression": expression, "value": value}

	var nextStep string
	if cases != nil {
		if caseStep, ok := cases[value].(string); ok {
			nextStep = caseStep
			output["matched"] = value
		}
	}
	if nextStep == "" {
		nextStep = defaultStep
		output["matched"] = "default"
	}

	return &StepResult{Output: output, NextStep: nextStep}, nil
}

func transformExecute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("transform step requires config")
	}

	template, ok := config["template"]
	if !ok {
		return nil, fmt.Errorf("transform step requires 'template'")
	}

	var output map[string]interface{}
	switch t := template.(type) {
	case map[string]interface{}:
		output = resolver.ResolveMap(t)
	case string:
		output = map[string]interface{}{"result": resolver.ResolveString(t)}
	default:
		return nil, fmt.Errorf("transform template must be object or string")
	}

	return &StepResult{Output: output}, nil
}

func setExecute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("set step requires config")
	}

	name, ok := config["name"].(string)
	if !ok {
		return nil, fmt.Errorf("set step requires 'name' string")
	}

	value, ok := config["value"]
	if !ok {
		return nil, fmt.Errorf("set step requires 'value'")
	}

	var resolvedValue interface{}
	switch v := value.(type) {
	case string:
		resolvedValue = resolver.ResolveString(v)
	case map[string]interface{}:
		resolvedValue = resolver.ResolveMap(v)
	default:
		resolvedValue = value
	}

	resolver.SetVariable(name, resolvedValue)
	return &StepResult{Output: map[string]interface{}{"name": name, "value": resolvedValue}}, nil
}

func mergeExecute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("merge step requires config")
	}

	sources, ok := config["sources"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("merge step requires 'sources' array")
	}

	strategy, _ := config["strategy"].(string)
	if strategy == "" {
		strategy = "merge"
	}

	if strategy == "concat" {
		var result []interface{}
		for _, source := range sources {
			switch s := source.(type) {
			case string:
				result = append(result, resolver.ResolveString(s))
			case []interface{}:
				for _, item := range s {
					if str, ok := item.(string); ok {
						result = append(result, resolver.ResolveString(str))
					} else {
						result = append(result, item)
					}
				}
			default:
				result = append(result, source)
			}
		}
		return &StepResult{Output: map[string]interface{}{"result": result}}, nil
	}

	// Default: merge objects
	result := make(map[string]interface{})
	for _, source := range sources {
		var resolved interface{}
		switch s := source.(type) {
		case string:
			resolved = resolver.ResolveString(s)
		case map[string]interface{}:
			resolved = resolver.ResolveMap(s)
		default:
			resolved = source
		}
		if m, ok := resolved.(map[string]interface{}); ok {
			for k, v := range m {
				result[k] = v
			}
		}
	}

	return &StepResult{Output: result}, nil
}

func delayExecute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("delay step requires config")
	}

	var duration time.Duration
	if seconds, ok := config["seconds"]; ok {
		secs, err := toFloat(seconds, resolver)
		if err != nil {
			return nil, fmt.Errorf("invalid seconds: %w", err)
		}
		duration = time.Duration(secs * float64(time.Second))
	} else if ms, ok := config["milliseconds"]; ok {
		msVal, err := toFloat(ms, resolver)
		if err != nil {
			return nil, fmt.Errorf("invalid milliseconds: %w", err)
		}
		duration = time.Duration(msVal * float64(time.Millisecond))
	} else {
		return nil, fmt.Errorf("delay requires 'seconds' or 'milliseconds'")
	}

	if duration > 5*time.Minute {
		duration = 5 * time.Minute
	}

	select {
	case <-time.After(duration):
		return &StepResult{Output: map[string]interface{}{"delayed": true, "duration": duration.Milliseconds()}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func httpExecute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("http step requires config")
	}

	urlTemplate, ok := config["url"].(string)
	if !ok || urlTemplate == "" {
		return nil, fmt.Errorf("http step requires 'url'")
	}
	url := resolver.ResolveString(urlTemplate)

	method := "GET"
	if m, ok := config["method"].(string); ok {
		method = strings.ToUpper(m)
	}

	timeout := 30 * time.Second
	if t, ok := config["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	retries := 0
	if r, ok := config["retries"].(float64); ok && r > 0 && r <= 5 {
		retries = int(r)
	}

	var bodyReader io.Reader
	var bodyBytes []byte
	if body, ok := config["body"]; ok {
		switch b := body.(type) {
		case string:
			bodyBytes = []byte(resolver.ResolveString(b))
		case map[string]interface{}:
			bodyBytes, _ = json.Marshal(resolver.ResolveMap(b))
		default:
			bodyBytes, _ = json.Marshal(body)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if headers, ok := config["headers"].(map[string]interface{}); ok {
		for key, value := range headers {
			if strVal, ok := value.(string); ok {
				req.Header.Set(key, resolver.ResolveString(strVal))
			}
		}
	}

	if bodyReader != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: timeout}
	var resp *http.Response
	var lastErr error

	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<uint(attempt-1)) * time.Second)
			if bodyBytes != nil {
				req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
		}
		resp, lastErr = client.Do(req)
		if lastErr == nil && resp.StatusCode < 500 {
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("request failed: %w", lastErr)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	output := map[string]interface{}{
		"statusCode": resp.StatusCode,
		"status":     resp.Status,
	}

	var jsonBody interface{}
	if json.Unmarshal(respBody, &jsonBody) == nil {
		output["body"] = jsonBody
	} else {
		output["body"] = string(respBody)
	}

	if resp.StatusCode >= 400 {
		output["error"] = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}

	return &StepResult{Output: output}, nil
}

func toFloat(v interface{}, resolver TemplateResolver) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case int:
		return float64(val), nil
	case string:
		return strconv.ParseFloat(resolver.ResolveString(val), 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float", v)
	}
}
