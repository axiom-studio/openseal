package executor

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"time"
)

// FileObject represents a file reference in the workflow context
// Format: {"_type": "file", "url": "http://...", "filename": "doc.pdf", "mimeType": "application/pdf", "size": 1234}
type FileObject struct {
	Type     string `json:"_type"`
	ID       string `json:"id"`
	URL      string `json:"url"`
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}

// IsFileObject checks if a value is a file object
func IsFileObject(value interface{}) bool {
	if m, ok := value.(map[string]interface{}); ok {
		if typeVal, ok := m["_type"].(string); ok && typeVal == "file" {
			return true
		}
	}
	return false
}

// ParseFileObject extracts file information from a map
func ParseFileObject(value interface{}) (*FileObject, error) {
	m, ok := value.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("not a map")
	}

	typeVal, _ := m["_type"].(string)
	if typeVal != "file" {
		return nil, fmt.Errorf("not a file object")
	}

	return &FileObject{
		Type:     typeVal,
		ID:       getString(m, "id"),
		URL:      getString(m, "url"),
		Filename: getString(m, "filename"),
		MimeType: getString(m, "mimeType"),
		Size:     getInt64(m, "size"),
	}, nil
}

// FetchFileContent fetches file content from a file object's URL
func FetchFileContent(fileObj *FileObject) ([]byte, error) {
	if fileObj.URL == "" {
		return nil, fmt.Errorf("file object has no URL")
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(fileObj.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch file: HTTP %d", resp.StatusCode)
	}

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read file content: %w", err)
	}

	return content, nil
}

// FetchFileBase64 fetches file content and returns as base64 string
func FetchFileBase64(fileObj *FileObject) (string, error) {
	content, err := FetchFileContent(fileObj)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(content), nil
}

// ExtractFilesFromMap recursively finds all file objects in a map
func ExtractFilesFromMap(data map[string]interface{}) []*FileObject {
	var files []*FileObject
	extractFilesRecursive(data, &files)
	return files
}

func extractFilesRecursive(value interface{}, files *[]*FileObject) {
	switch v := value.(type) {
	case map[string]interface{}:
		if IsFileObject(v) {
			if fileObj, err := ParseFileObject(v); err == nil {
				*files = append(*files, fileObj)
			}
		} else {
			for _, val := range v {
				extractFilesRecursive(val, files)
			}
		}
	case []interface{}:
		for _, val := range v {
			extractFilesRecursive(val, files)
		}
	}
}

// Helper functions
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getInt64(m map[string]interface{}, key string) int64 {
	switch v := m[key].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case int:
		return int64(v)
	}
	return 0
}
