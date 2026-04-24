package clawhub

import (
	"archive/zip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSearch_Success(t *testing.T) {
	expected := []SkillSummary{
		{Slug: "test-skill", Name: "Test Skill", Description: "A test skill", Tags: []string{"test"}, Downloads: 100, Stars: 5, UpdatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if q := r.URL.Query().Get("q"); q != "test" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	client := NewClawHubClient(server.URL)
	results, err := client.Search("test", 10, "newest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Slug != "test-skill" {
		t.Errorf("expected slug 'test-skill', got '%s'", results[0].Slug)
	}
}

func TestSearch_EmptyResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer server.Close()

	client := NewClawHubClient(server.URL)
	results, err := client.Search("nonexistent", 10, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty slice, got %d results", len(results))
	}
}

func TestSearch_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClawHubClient(server.URL)
	_, err := client.Search("test", 10, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var clawErr *ClawHubError
	if !isClawHubError(err, &clawErr) {
		t.Fatalf("expected ClawHubError, got %T: %v", err, err)
	}
	if clawErr.StatusCode != 500 {
		t.Errorf("expected status 500, got %d", clawErr.StatusCode)
	}
}

func TestSearch_Timeout(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(60 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-done:
		}
	}))

	client := NewClawHubClient(server.URL)
	client.httpClient.Timeout = 100 * time.Millisecond

	_, err := client.Search("test", 10, "")
	close(done)
	server.Close()

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") && !strings.Contains(err.Error(), "Client.Timeout exceeded") {
		t.Errorf("expected timeout error, got: %v", err)
	}
}

func TestInspect_Success(t *testing.T) {
	expected := SkillDetail{
		SkillSummary: SkillSummary{Slug: "my-skill", Name: "My Skill", Description: "desc"},
		Version:      "1.2.3",
		Files:        []string{"SKILL.md", "index.js"},
		ManifestRaw:  `{"name":"my-skill"}`,
		Author:       "test-author",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/skills/my-skill" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	client := NewClawHubClient(server.URL)
	detail, err := client.Inspect("my-skill", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if detail.Slug != "my-skill" {
		t.Errorf("expected slug 'my-skill', got '%s'", detail.Slug)
	}
	if detail.Version != "1.2.3" {
		t.Errorf("expected version '1.2.3', got '%s'", detail.Version)
	}
	if len(detail.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(detail.Files))
	}
	if detail.Author != "test-author" {
		t.Errorf("expected author 'test-author', got '%s'", detail.Author)
	}
}

func TestInspect_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "skill not found", http.StatusNotFound)
	}))
	defer server.Close()

	client := NewClawHubClient(server.URL)
	_, err := client.Inspect("nonexistent", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsNotFoundError(err) {
		t.Errorf("expected NotFoundError, got: %v", err)
	}
}

func TestDownload_Success(t *testing.T) {
	zipData := createTestZip(t, map[string]string{
		"SKILL.md":  "# Test Skill\nDescription here",
		"index.js":  "module.exports = {}",
		"README.md": "# README",
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/download" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if slug := r.URL.Query().Get("slug"); slug != "test-skill" {
			http.Error(w, "bad slug", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipData)
	}))
	defer server.Close()

	destDir := t.TempDir()
	client := NewClawHubClient(server.URL)
	extractedPath, err := client.Download("test-skill", destDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if extractedPath == "" {
		t.Fatal("expected extracted path, got empty")
	}
	if !strings.HasPrefix(extractedPath, destDir) {
		t.Errorf("expected path under %s, got %s", destDir, extractedPath)
	}

	skillMd := filepath.Join(extractedPath, "SKILL.md")
	if _, err := os.Stat(skillMd); os.IsNotExist(err) {
		t.Error("SKILL.md not found in extracted directory")
	}
	indexJs := filepath.Join(extractedPath, "index.js")
	if _, err := os.Stat(indexJs); os.IsNotExist(err) {
		t.Error("index.js not found in extracted directory")
	}
}

func TestDownload_InvalidZip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write([]byte("this is not a valid zip file"))
	}))
	defer server.Close()

	destDir := t.TempDir()
	client := NewClawHubClient(server.URL)
	_, err := client.Download("bad-skill", destDir)
	if err == nil {
		t.Fatal("expected error for invalid zip, got nil")
	}
}

func TestExplore_Success(t *testing.T) {
	expected := []SkillSummary{
		{Slug: "skill-a", Name: "Skill A", Downloads: 500, Stars: 10},
		{Slug: "skill-b", Name: "Skill B", Downloads: 300, Stars: 7},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/skills" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	client := NewClawHubClient(server.URL)
	results, err := client.Explore(20, "newest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Slug != "skill-a" {
		t.Errorf("expected first slug 'skill-a', got '%s'", results[0].Slug)
	}
}

func TestRateLimit_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewClawHubClient(server.URL)
	_, err := client.Search("test", 10, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsRateLimitError(err) {
		t.Errorf("expected RateLimitError, got: %v", err)
	}
	var clawErr *ClawHubError
	if isClawHubError(err, &clawErr) && clawErr.RetryAfter != 30 {
		t.Errorf("expected RetryAfter=30, got %d", clawErr.RetryAfter)
	}
}

func TestServerError_RetryAndFail(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClawHubClient(server.URL)
	client.httpClient.Timeout = 5 * time.Second
	_, err := client.Search("test", 10, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts (1 + 2 retries), got %d", attempts)
	}
	if !IsServerError(err) {
		t.Errorf("expected ServerError, got: %v", err)
	}
}

func TestDefaultBaseURL_FromEnv(t *testing.T) {
	orig := os.Getenv(envRegistryURL)
	defer os.Setenv(envRegistryURL, orig)

	os.Setenv(envRegistryURL, "https://custom-registry.example.com/api/v1")
	client := NewClawHubClient("")
	if client.baseURL != "https://custom-registry.example.com/api/v1" {
		t.Errorf("expected env baseURL, got %s", client.baseURL)
	}
}

func TestDefaultBaseURL_Parameter(t *testing.T) {
	client := NewClawHubClient("https://explicit.example.com/api/v1")
	if client.baseURL != "https://explicit.example.com/api/v1" {
		t.Errorf("expected explicit baseURL, got %s", client.baseURL)
	}
}

func TestDefaultBaseURL_TrailingSlash(t *testing.T) {
	client := NewClawHubClient("https://example.com/api/v1/")
	if client.baseURL != "https://example.com/api/v1" {
		t.Errorf("expected trimmed baseURL, got %s", client.baseURL)
	}
}

func TestDefaultBaseURL_Default(t *testing.T) {
	orig := os.Getenv(envRegistryURL)
	defer os.Setenv(envRegistryURL, orig)
	os.Setenv(envRegistryURL, "")

	client := NewClawHubClient("")
	if client.baseURL != defaultBaseURL {
		t.Errorf("expected default baseURL %s, got %s", defaultBaseURL, client.baseURL)
	}
}

func TestErrorHelpers(t *testing.T) {
	notFound := &ClawHubError{StatusCode: 404, Message: "not found"}
	rateLimit := &ClawHubError{StatusCode: 429, Message: "rate limited", RetryAfter: 60}
	serverErr := &ClawHubError{StatusCode: 503, Message: "unavailable"}

	if !IsNotFoundError(notFound) {
		t.Error("IsNotFoundError should return true for 404")
	}
	if IsNotFoundError(rateLimit) {
		t.Error("IsNotFoundError should return false for 429")
	}

	if !IsRateLimitError(rateLimit) {
		t.Error("IsRateLimitError should return true for 429")
	}
	if IsRateLimitError(notFound) {
		t.Error("IsRateLimitError should return false for 404")
	}

	if !IsServerError(serverErr) {
		t.Error("IsServerError should return true for 503")
	}
	if IsServerError(notFound) {
		t.Error("IsServerError should return false for 404")
	}
}

func TestClawHubError_Error(t *testing.T) {
	err := &ClawHubError{StatusCode: 429, Message: "too many", RetryAfter: 30}
	msg := err.Error()
	if !strings.Contains(msg, "429") {
		t.Errorf("error message should contain status code: %s", msg)
	}
	if !strings.Contains(msg, "30s") {
		t.Errorf("error message should contain retry-after: %s", msg)
	}

	errNoRetry := &ClawHubError{StatusCode: 404, Message: "not found"}
	msg2 := errNoRetry.Error()
	if strings.Contains(msg2, "retry after") {
		t.Errorf("error message should not contain retry-after when RetryAfter=0: %s", msg2)
	}
}

func createTestZip(t *testing.T, files map[string]string) []byte {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "test-*.zip")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	w := zip.NewWriter(tmpFile)
	for name, content := range files {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	w.Close()
	tmpFile.Close()

	data, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatalf("read temp zip: %v", err)
	}
	return data
}

func isClawHubError(err error, target **ClawHubError) bool {
	if err != nil && strings.Contains(err.Error(), "clawhub API error") {
		for {
			if ce, ok := err.(*ClawHubError); ok {
				*target = ce
				return true
			}
			if unwrapper, ok := err.(interface{ Unwrap() error }); ok {
				err = unwrapper.Unwrap()
			} else {
				break
			}
		}
	}
	return false
}
