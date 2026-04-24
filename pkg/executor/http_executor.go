package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// HTTPExecutor makes HTTP requests to external APIs
//
//	Config: {
//	  "url": "https://api.example.com/endpoint",
//	  "method": "POST",
//	  "headers": {"Authorization": "Bearer {{inputs.token}}"},
//	  "body": {"data": "{{step.output.previous.result}}"},
//	  "file": "{{trigger.document}}",           // optional: single file to upload
//	  "files": ["{{prev.file1}}", "{{prev.file2}}"],  // optional: multiple files
//	  "fileField": "attachment",                // optional: form field name (default: "file")
//	  "timeout": 30
//	}
type HTTPExecutor struct {
	client *http.Client
}

func NewHTTPExecutor() *HTTPExecutor {
	return &HTTPExecutor{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (e *HTTPExecutor) Type() string {
	return StepTypeHTTP
}

func (e *HTTPExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("http step requires config")
	}

	// Get URL (required)
	urlTemplate, ok := config["url"].(string)
	if !ok || urlTemplate == "" {
		return nil, fmt.Errorf("http step requires 'url'")
	}
	url := resolver.ResolveString(urlTemplate)

	// Get method (default: GET)
	method := "GET"
	if m, ok := config["method"].(string); ok {
		method = strings.ToUpper(m)
	}

	// Get timeout
	timeout := 30 * time.Second
	if t, ok := config["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	// Retries removed - HTTP executor makes single attempt only

	// Extract files from config - supports comma-delimited file references
	var files []*FileObject

	if fileTemplate, ok := config["file"].(string); ok {
		// Check if comma-delimited (multiple files)
		fileParts := strings.Split(fileTemplate, ",")
		for i, part := range fileParts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			key := fmt.Sprintf("file_%d", i)
			resolvedConfig := resolver.ResolveMap(map[string]interface{}{key: part})
			if IsFileObject(resolvedConfig[key]) {
				if fileObj, err := ParseFileObject(resolvedConfig[key]); err == nil {
					files = append(files, fileObj)
				}
			}
		}
	} else if IsFileObject(config["file"]) {
		if fileObj, err := ParseFileObject(config["file"]); err == nil {
			files = append(files, fileObj)
		}
	}

	// Get file field name (default: "file")
	fileField := "file"
	if ff, ok := config["fileField"].(string); ok && ff != "" {
		fileField = ff
	}

	// Build request body - multipart if files present, otherwise JSON
	var bodyReader io.Reader
	var contentType string

	if len(files) > 0 {
		// Multipart form data for file uploads
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		// Add files
		for i, file := range files {
			content, err := FetchFileContent(file)
			if err != nil {
				continue // Skip files that fail to fetch
			}

			fieldName := fileField
			if len(files) > 1 {
				fieldName = fmt.Sprintf("%s_%d", fileField, i)
			}

			part, err := writer.CreateFormFile(fieldName, file.Filename)
			if err != nil {
				continue
			}
			part.Write(content)
		}

		// Add other body fields as form fields
		if bodyConfig, ok := config["body"].(map[string]interface{}); ok {
			resolvedBody := resolver.ResolveMap(bodyConfig)
			for key, value := range resolvedBody {
				switch v := value.(type) {
				case string:
					writer.WriteField(key, v)
				default:
					jsonBytes, _ := json.Marshal(v)
					writer.WriteField(key, string(jsonBytes))
				}
			}
		}

		writer.Close()
		bodyReader = body
		contentType = writer.FormDataContentType()
	} else if body, ok := config["body"]; ok {
		// Standard JSON body
		var bodyBytes []byte
		var err error

		switch b := body.(type) {
		case string:
			resolved := resolver.ResolveString(b)
			bodyBytes = []byte(resolved)
		case map[string]interface{}:
			resolvedBody := resolver.ResolveMap(b)
			bodyBytes, err = json.Marshal(resolvedBody)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal body: %w", err)
			}
		default:
			bodyBytes, err = json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal body: %w", err)
			}
		}
		bodyReader = bytes.NewReader(bodyBytes)
		contentType = "application/json"
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set content type
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	// Set headers from config
	// Note: Do NOT allow Content-Type override when sending multipart form (files)
	// because the multipart writer sets Content-Type with the boundary parameter
	if headers, ok := config["headers"].(map[string]interface{}); ok {
		for key, value := range headers {
			if strVal, ok := value.(string); ok {
				// Skip Content-Type if we have files - multipart boundary is critical
				if len(files) > 0 && strings.EqualFold(key, "Content-Type") {
					continue
				}
				req.Header.Set(key, resolver.ResolveString(strVal))
			}
		}
	}

	// Execute request (single attempt, no retries)
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response
	output := map[string]interface{}{
		"statusCode": resp.StatusCode,
		"status":     resp.Status,
		"headers":    headerToMap(resp.Header),
	}

	// Try to parse as JSON
	var jsonBody interface{}
	if err := json.Unmarshal(respBody, &jsonBody); err == nil {
		output["body"] = jsonBody
	} else {
		output["body"] = string(respBody)
	}

	// Check for error status codes
	if resp.StatusCode >= 400 {
		return &StepResult{
			Output: output,
		}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	return &StepResult{
		Output: output,
	}, nil
}

func headerToMap(h http.Header) map[string]string {
	result := make(map[string]string)
	for key, values := range h {
		if len(values) > 0 {
			result[key] = values[0]
		}
	}
	return result
}
