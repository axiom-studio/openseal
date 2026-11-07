//go:build integration

package openseal

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func assertLiveE2ESecretAbsent(t *testing.T, secret string, values ...interface{}) {
	t.Helper()
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal live E2E state for credential scan: %v", err)
		}
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("model credential leaked into durable OpenSeal state")
		}
	}
}

func assertLiveE2EWorkspaceSecretAbsent(t *testing.T, workspace, secret string) {
	t.Helper()
	err := filepath.Walk(workspace, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte(secret)) {
			t.Fatalf("model credential leaked into live E2E workspace file %q", filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan live E2E workspace for credentials: %v", err)
	}
}
