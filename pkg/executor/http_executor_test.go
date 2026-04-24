package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// httpTestResolver implements TemplateResolver for HTTP executor testing
type httpTestResolver struct {
	data map[string]interface{}
}

func (r *httpTestResolver) ResolveString(template string) string {
	return template
}

func (r *httpTestResolver) ResolveMap(m map[string]interface{}) map[string]interface{} {
	return m
}

func (r *httpTestResolver) EvaluateCondition(condition string) bool {
	return true
}

func (r *httpTestResolver) SetVariable(name string, value interface{}) {
}

func (r *httpTestResolver) GetStepOutput(stepName string) interface{} {
	return nil
}

func (r *httpTestResolver) SetStepOutput(stepName string, output interface{}) {
}

func (r *httpTestResolver) GetContextData() map[string]interface{} {
	return r.data
}

func TestHTTPExecutor_MultipartFormWithFile(t *testing.T) {
	// Setup a test file server that will provide the file content
	fileContent := []byte("This is the content of the uploaded file")
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fileContent)
	}))
	defer fileServer.Close()

	// Setup a server that receives the multipart form
	var receivedFiles []struct {
		FieldName string
		Filename  string
		Content   []byte
	}
	var receivedFields map[string]string
	var receivedContentType string

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")

		// Parse the multipart form
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Logf("Failed to parse media type: %v", err)
			http.Error(w, "failed to parse content type", http.StatusBadRequest)
			return
		}

		if !strings.HasPrefix(mediaType, "multipart/") {
			t.Logf("Not a multipart request: %s", mediaType)
			http.Error(w, "expected multipart form", http.StatusBadRequest)
			return
		}

		mr := multipart.NewReader(r.Body, params["boundary"])
		receivedFields = make(map[string]string)

		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Logf("Error reading part: %v", err)
				http.Error(w, "failed to read part", http.StatusBadRequest)
				return
			}

			content, _ := io.ReadAll(part)

			if part.FileName() != "" {
				receivedFiles = append(receivedFiles, struct {
					FieldName string
					Filename  string
					Content   []byte
				}{
					FieldName: part.FormName(),
					Filename:  part.FileName(),
					Content:   content,
				})
			} else {
				receivedFields[part.FormName()] = string(content)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":     "ok",
			"filesCount": len(receivedFiles),
		})
	}))
	defer targetServer.Close()

	// Create the HTTP executor
	executor := NewHTTPExecutor()
	resolver := &httpTestResolver{}

	// Create step config with a file
	step := &StepDefinition{
		Config: map[string]interface{}{
			"url":    targetServer.URL,
			"method": "POST",
			"file": map[string]interface{}{
				"_type":    "file",
				"url":      fileServer.URL + "/test.pdf",
				"filename": "test.pdf",
				"mimeType": "application/pdf",
			},
			"body": map[string]interface{}{
				"description": "Test upload",
			},
		},
	}

	// Execute
	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Check result
	output := result.Output
	statusCode := output["statusCode"].(int)
	if statusCode != 200 {
		t.Errorf("Expected status 200, got %d", statusCode)
		t.Logf("Response body: %v", output["body"])
	}

	// Verify the content type had a boundary
	if !strings.Contains(receivedContentType, "multipart/form-data") {
		t.Errorf("Expected multipart/form-data content type, got: %s", receivedContentType)
	}
	if !strings.Contains(receivedContentType, "boundary=") {
		t.Errorf("Expected boundary in content type, got: %s", receivedContentType)
	}

	// Verify file was received
	if len(receivedFiles) != 1 {
		t.Errorf("Expected 1 file, got %d", len(receivedFiles))
	} else {
		if receivedFiles[0].FieldName != "file" {
			t.Errorf("Expected field name 'file', got %s", receivedFiles[0].FieldName)
		}
		if receivedFiles[0].Filename != "test.pdf" {
			t.Errorf("Expected filename 'test.pdf', got %s", receivedFiles[0].Filename)
		}
		if !bytes.Equal(receivedFiles[0].Content, fileContent) {
			t.Errorf("File content mismatch")
		}
	}

	// Verify form fields
	if receivedFields["description"] != "Test upload" {
		t.Errorf("Expected description field, got: %v", receivedFields)
	}
}

func TestHTTPExecutor_MultipartFormWithCustomFileField(t *testing.T) {
	fileContent := []byte("Custom field file content")
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fileContent)
	}))
	defer fileServer.Close()

	var receivedFiles []struct {
		FieldName string
		Filename  string
	}

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			http.Error(w, "expected multipart", http.StatusBadRequest)
			return
		}

		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, "read error", http.StatusBadRequest)
				return
			}
			if part.FileName() != "" {
				receivedFiles = append(receivedFiles, struct {
					FieldName string
					Filename  string
				}{
					FieldName: part.FormName(),
					Filename:  part.FileName(),
				})
			}
			io.ReadAll(part)
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer targetServer.Close()

	executor := NewHTTPExecutor()
	resolver := &httpTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"url":       targetServer.URL,
			"method":    "POST",
			"fileField": "attachment",
			"file": map[string]interface{}{
				"_type":    "file",
				"url":      fileServer.URL + "/doc.pdf",
				"filename": "document.pdf",
				"mimeType": "application/pdf",
			},
		},
	}

	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.Output
	if output["statusCode"].(int) != 200 {
		t.Errorf("Expected status 200, got %v", output["statusCode"])
	}

	if len(receivedFiles) != 1 {
		t.Fatalf("Expected 1 file, got %d", len(receivedFiles))
	}

	if receivedFiles[0].FieldName != "attachment" {
		t.Errorf("Expected field name 'attachment', got %s", receivedFiles[0].FieldName)
	}
}

func TestHTTPExecutor_MultipartFormWithMultipleFiles(t *testing.T) {
	fileContents := map[string][]byte{
		"/file1.pdf": []byte("Content of file 1"),
		"/file2.pdf": []byte("Content of file 2"),
	}
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if content, ok := fileContents[r.URL.Path]; ok {
			w.Write(content)
		} else {
			http.NotFound(w, r)
		}
	}))
	defer fileServer.Close()

	var receivedFiles []struct {
		FieldName string
		Filename  string
		Content   []byte
	}

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			http.Error(w, "expected multipart", http.StatusBadRequest)
			return
		}

		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, "read error", http.StatusBadRequest)
				return
			}
			content, _ := io.ReadAll(part)
			if part.FileName() != "" {
				receivedFiles = append(receivedFiles, struct {
					FieldName string
					Filename  string
					Content   []byte
				}{
					FieldName: part.FormName(),
					Filename:  part.FileName(),
					Content:   content,
				})
			}
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]int{"files": len(receivedFiles)})
	}))
	defer targetServer.Close()

	executor := NewHTTPExecutor()
	resolver := &httpTestResolver{}

	// Test comma-separated file references
	step := &StepDefinition{
		Config: map[string]interface{}{
			"url":    targetServer.URL,
			"method": "POST",
			"file":   fileServer.URL + "/file1.pdf, " + fileServer.URL + "/file2.pdf",
		},
	}

	// Note: The current implementation expects file objects, not URLs
	// This test documents expected behavior
	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.Output
	t.Logf("Status: %v, Body: %v", output["statusCode"], output["body"])
}

func TestHTTPExecutor_HeadersShouldNotOverrideMultipartContentType(t *testing.T) {
	fileContent := []byte("Test file content")
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fileContent)
	}))
	defer fileServer.Close()

	var receivedContentType string

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")

		// Try to parse as multipart
		mediaType, params, err := mime.ParseMediaType(receivedContentType)
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Logf("Not multipart: mediaType=%s, err=%v", mediaType, err)
			http.Error(w, "expected multipart", http.StatusBadRequest)
			return
		}

		if params["boundary"] == "" {
			t.Log("No boundary found")
			http.Error(w, "missing boundary", http.StatusBadRequest)
			return
		}

		// Read the body to verify it's valid multipart
		mr := multipart.NewReader(r.Body, params["boundary"])
		partCount := 0
		for {
			_, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Logf("Multipart read error: %v", err)
				http.Error(w, "multipart read error", http.StatusBadRequest)
				return
			}
			partCount++
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]int{"parts": partCount})
	}))
	defer targetServer.Close()

	executor := NewHTTPExecutor()
	resolver := &httpTestResolver{}

	// User tries to set Content-Type header manually (which would break multipart)
	step := &StepDefinition{
		Config: map[string]interface{}{
			"url":    targetServer.URL,
			"method": "POST",
			"headers": map[string]interface{}{
				"Content-Type":  "application/json", // This should be ignored when sending files
				"Authorization": "Bearer token123",
			},
			"file": map[string]interface{}{
				"_type":    "file",
				"url":      fileServer.URL + "/test.pdf",
				"filename": "test.pdf",
				"mimeType": "application/pdf",
			},
		},
	}

	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.Output
	statusCode := output["statusCode"].(int)

	// This test currently FAILS because the Content-Type header is being overridden
	// The fix should prevent this from happening
	if statusCode != 200 {
		t.Errorf("Expected status 200, got %d - Content-Type was likely overridden", statusCode)
		t.Logf("Received Content-Type: %s", receivedContentType)
		t.Logf("Response: %v", output["body"])
	}
}

func TestHTTPExecutor_JSONBodyWithoutFiles(t *testing.T) {
	var receivedBody map[string]interface{}
	var receivedContentType string

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer targetServer.Close()

	executor := NewHTTPExecutor()
	resolver := &httpTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"url":    targetServer.URL,
			"method": "POST",
			"body": map[string]interface{}{
				"name":  "test",
				"value": 123,
			},
		},
	}

	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.Output
	if output["statusCode"].(int) != 200 {
		t.Errorf("Expected status 200, got %v", output["statusCode"])
	}

	if receivedContentType != "application/json" {
		t.Errorf("Expected application/json content type, got %s", receivedContentType)
	}

	if receivedBody["name"] != "test" {
		t.Errorf("Expected name=test, got %v", receivedBody["name"])
	}
}

func TestHTTPExecutor_GetRequest(t *testing.T) {
	var receivedMethod string

	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"method": r.Method})
	}))
	defer targetServer.Close()

	executor := NewHTTPExecutor()
	resolver := &httpTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"url": targetServer.URL,
		},
	}

	result, err := executor.Execute(context.Background(), step, resolver)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.Output
	if output["statusCode"].(int) != 200 {
		t.Errorf("Expected status 200, got %v", output["statusCode"])
	}

	if receivedMethod != "GET" {
		t.Errorf("Expected GET method, got %s", receivedMethod)
	}
}
