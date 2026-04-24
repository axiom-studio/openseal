package executor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsFileObject(t *testing.T) {
	tests := []struct {
		name     string
		value    interface{}
		expected bool
	}{
		{
			name: "valid file object",
			value: map[string]interface{}{
				"_type":    "file",
				"url":      "http://example.com/file.pdf",
				"filename": "file.pdf",
				"mimeType": "application/pdf",
			},
			expected: true,
		},
		{
			name: "missing _type",
			value: map[string]interface{}{
				"url":      "http://example.com/file.pdf",
				"filename": "file.pdf",
			},
			expected: false,
		},
		{
			name: "wrong _type",
			value: map[string]interface{}{
				"_type": "document",
				"url":   "http://example.com/file.pdf",
			},
			expected: false,
		},
		{
			name:     "string value",
			value:    "not a file",
			expected: false,
		},
		{
			name:     "nil value",
			value:    nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsFileObject(tt.value)
			if result != tt.expected {
				t.Errorf("IsFileObject() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestParseFileObject(t *testing.T) {
	tests := []struct {
		name        string
		value       interface{}
		expectError bool
		checkFields func(*FileObject) bool
	}{
		{
			name: "valid file object",
			value: map[string]interface{}{
				"_type":    "file",
				"id":       "abc123",
				"url":      "http://example.com/file.pdf",
				"filename": "file.pdf",
				"mimeType": "application/pdf",
				"size":     float64(1234),
			},
			expectError: false,
			checkFields: func(f *FileObject) bool {
				return f.ID == "abc123" &&
					f.URL == "http://example.com/file.pdf" &&
					f.Filename == "file.pdf" &&
					f.MimeType == "application/pdf" &&
					f.Size == 1234
			},
		},
		{
			name: "minimal file object",
			value: map[string]interface{}{
				"_type": "file",
				"url":   "http://example.com/file.pdf",
			},
			expectError: false,
			checkFields: func(f *FileObject) bool {
				return f.URL == "http://example.com/file.pdf" &&
					f.Filename == "" &&
					f.Size == 0
			},
		},
		{
			name: "wrong type",
			value: map[string]interface{}{
				"_type": "document",
			},
			expectError: true,
		},
		{
			name:        "not a map",
			value:       "string",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseFileObject(tt.value)
			if tt.expectError {
				if err == nil {
					t.Error("Expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if result == nil {
					t.Fatal("Expected result, got nil")
				}
				if tt.checkFields != nil && !tt.checkFields(result) {
					t.Errorf("Field check failed for result: %+v", result)
				}
			}
		})
	}
}

func TestFetchFileContent(t *testing.T) {
	// Setup test server
	expectedContent := []byte("Test file content for fetching")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(expectedContent)
	}))
	defer server.Close()

	fileObj := &FileObject{
		Type:     "file",
		URL:      server.URL + "/test.pdf",
		Filename: "test.pdf",
		MimeType: "application/pdf",
	}

	content, err := FetchFileContent(fileObj)
	if err != nil {
		t.Fatalf("FetchFileContent failed: %v", err)
	}

	if string(content) != string(expectedContent) {
		t.Errorf("Content mismatch. Got %s, expected %s", string(content), string(expectedContent))
	}
}

func TestFetchFileContent_NoURL(t *testing.T) {
	fileObj := &FileObject{
		Type:     "file",
		Filename: "test.pdf",
	}

	_, err := FetchFileContent(fileObj)
	if err == nil {
		t.Error("Expected error for file without URL")
	}
}

func TestFetchFileBase64(t *testing.T) {
	content := []byte("Test content")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer server.Close()

	fileObj := &FileObject{
		Type: "file",
		URL:  server.URL + "/test.txt",
	}

	base64Str, err := FetchFileBase64(fileObj)
	if err != nil {
		t.Fatalf("FetchFileBase64 failed: %v", err)
	}

	// "Test content" in base64
	expected := "VGVzdCBjb250ZW50"
	if base64Str != expected {
		t.Errorf("Base64 mismatch. Got %s, expected %s", base64Str, expected)
	}
}

func TestExtractFilesFromMap(t *testing.T) {
	data := map[string]interface{}{
		"name": "test",
		"document": map[string]interface{}{
			"_type":    "file",
			"url":      "http://example.com/doc.pdf",
			"filename": "doc.pdf",
			"mimeType": "application/pdf",
		},
		"attachments": []interface{}{
			map[string]interface{}{
				"_type":    "file",
				"url":      "http://example.com/att1.png",
				"filename": "att1.png",
				"mimeType": "image/png",
			},
			map[string]interface{}{
				"_type":    "file",
				"url":      "http://example.com/att2.jpg",
				"filename": "att2.jpg",
				"mimeType": "image/jpeg",
			},
		},
		"nested": map[string]interface{}{
			"deep": map[string]interface{}{
				"file": map[string]interface{}{
					"_type":    "file",
					"url":      "http://example.com/deep.pdf",
					"filename": "deep.pdf",
				},
			},
		},
	}

	files := ExtractFilesFromMap(data)

	if len(files) != 4 {
		t.Errorf("Expected 4 files, got %d", len(files))
	}

	// Verify filenames are correct
	filenames := make(map[string]bool)
	for _, f := range files {
		filenames[f.Filename] = true
	}

	expectedFilenames := []string{"doc.pdf", "att1.png", "att2.jpg", "deep.pdf"}
	for _, name := range expectedFilenames {
		if !filenames[name] {
			t.Errorf("Expected to find file %s", name)
		}
	}
}

func TestIsImageMimeType(t *testing.T) {
	tests := []struct {
		mimeType string
		expected bool
	}{
		{"image/jpeg", true},
		{"image/png", true},
		{"image/gif", true},
		{"image/webp", true},
		{"application/pdf", false},
		{"text/plain", false},
		{"video/mp4", false},
	}

	for _, tt := range tests {
		t.Run(tt.mimeType, func(t *testing.T) {
			result := isImageMimeType(tt.mimeType)
			if result != tt.expected {
				t.Errorf("isImageMimeType(%s) = %v, expected %v", tt.mimeType, result, tt.expected)
			}
		})
	}
}

// TestHTTPExecutor_WithFiles tests that the HTTP executor properly sends files as multipart
func TestHTTPExecutor_FileDetection(t *testing.T) {
	// Setup a mock server that receives the multipart form
	var receivedFiles []string
	var receivedFields map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedFields = make(map[string]string)

		if err := r.ParseMultipartForm(10 << 20); err != nil {
			// Not multipart, check for JSON body
			body, _ := io.ReadAll(r.Body)
			w.Write([]byte(`{"received": "` + string(body) + `"}`))
			return
		}

		// Get files
		for key := range r.MultipartForm.File {
			receivedFiles = append(receivedFiles, key)
		}

		// Get form fields
		for key, values := range r.MultipartForm.Value {
			if len(values) > 0 {
				receivedFields[key] = values[0]
			}
		}

		w.Write([]byte(`{"status": "ok", "files": ` + string(rune(len(receivedFiles))) + `}`))
	}))
	defer server.Close()

	// Test file object detection (without actually executing - just verify config parsing)
	config := map[string]interface{}{
		"file": map[string]interface{}{
			"_type":    "file",
			"url":      "http://example.com/test.pdf",
			"filename": "test.pdf",
			"mimeType": "application/pdf",
		},
	}

	// Verify the file is detected
	if !IsFileObject(config["file"]) {
		t.Error("Should detect file object in config")
	}

	fileObj, err := ParseFileObject(config["file"])
	if err != nil {
		t.Fatalf("Failed to parse file object: %v", err)
	}

	if fileObj.Filename != "test.pdf" {
		t.Errorf("Expected filename test.pdf, got %s", fileObj.Filename)
	}
}
