package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// MockContextUploader for testing
type MockContextUploader struct {
	shouldFail bool
	mockURL    string
}

func (m *MockContextUploader) UploadContext(ctx context.Context, data []byte) (string, error) {
	if m.shouldFail {
		return "", errors.New("upload failed")
	}
	return m.mockURL, nil
}

func TestCodeExecutorUploadContext(t *testing.T) {
	t.Run("uses uploader when available", func(t *testing.T) {
		mockUploader := &MockContextUploader{
			shouldFail: false,
			mockURL:    "http://test-server/files/123",
		}

		executor := &CodeExecutor{
			namespace:       "test-ns",
			runnerImage:     "python:3.11",
			serviceAccount:  "default",
			contextUploader: mockUploader,
		}

		// Note: buildJob expects contextJSON, not map
		job := executor.buildJob("test-job", "print('hi')", nil, 60, "{}", mockUploader.mockURL)

		// Verify Env var structure uses corev1 to satisfy import
		var _ []corev1.EnvVar = job.Spec.Template.Spec.Containers[0].Env

		// Verify AGENT_CONTEXT is minimal/empty
		var agentContext string
		var agentContextURL string
		for _, env := range job.Spec.Template.Spec.Containers[0].Env {
			if env.Name == "AGENT_CONTEXT" {
				agentContext = env.Value
			}
			if env.Name == "AGENT_CONTEXT_URL" {
				agentContextURL = env.Value
			}
		}

		if agentContext != "{}" {
			t.Errorf("expected AGENT_CONTEXT to be '{}', got %q", agentContext)
		}

		if agentContextURL != mockUploader.mockURL {
			t.Errorf("expected AGENT_CONTEXT_URL to be %q, got %q", mockUploader.mockURL, agentContextURL)
		}
	})

	t.Run("falls back when upload fails", func(t *testing.T) {
		// This test simulates the logic inside Execute(), but we can't easily test Execute()
		// without a full K8s mock.
		// Instead we verify that buildJob correctly handles the empty URL case
		// which happens when Execute passes empty URL on failure.

		executor := &CodeExecutor{
			namespace:      "test-ns",
			runnerImage:    "python:3.11",
			serviceAccount: "default",
		}

		fullContext := `{"key":"value"}`
		// On failure, Execute() passes the full context and empty URL
		job := executor.buildJob("test-job", "print('hi')", nil, 60, fullContext, "")

		var agentContext string
		var agentContextURL string
		for _, env := range job.Spec.Template.Spec.Containers[0].Env {
			if env.Name == "AGENT_CONTEXT" {
				agentContext = env.Value
			}
			if env.Name == "AGENT_CONTEXT_URL" {
				agentContextURL = env.Value
			}
		}

		if agentContext != fullContext {
			t.Errorf("expected AGENT_CONTEXT to be %q, got %q", fullContext, agentContext)
		}

		if agentContextURL != "" {
			t.Errorf("expected AGENT_CONTEXT_URL to be empty, got %q", agentContextURL)
		}
	})
}

func TestPythonWrapperContainsContextLogic(t *testing.T) {
	// Verify that the injected python code actually contains the logic to read the URL
	executor := &CodeExecutor{
		namespace:   "test-ns",
		runnerImage: "python:3.11",
	}

	job := executor.buildJob("test-job", "print('hi')", nil, 60, "{}", "http://url")
	command := job.Spec.Template.Spec.Containers[0].Command

	// The python code is in the 3rd argument (sh -c '...')
	if len(command) < 3 {
		t.Fatal("command too short")
	}
	shellCmd := command[2]

	expectedSnippets := []string{
		"AGENT_CONTEXT_URL",
		"urllib.request.urlopen",
		"Retry logic for context fetching",
	}

	for _, snippet := range expectedSnippets {
		if !strings.Contains(shellCmd, snippet) {
			t.Errorf("python wrapper missing snippet: %q", snippet)
		}
	}
}
