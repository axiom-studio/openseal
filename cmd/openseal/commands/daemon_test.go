package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"go.uber.org/zap"
)

func TestStandaloneOperatorRequiresExplicitLoopbackAddress(t *testing.T) {
	for address, expected := range map[string]bool{
		"127.0.0.1:8080": true,
		"[::1]:8080":     true,
		"localhost:8080": true,
		":8080":          false,
		"0.0.0.0:8080":   false,
	} {
		if actual := isLoopbackListenAddress(address); actual != expected {
			t.Fatalf("isLoopbackListenAddress(%q) = %t, want %t", address, actual, expected)
		}
	}
}

func TestDaemonScopeRequiresExactPortableScope(t *testing.T) {
	scope, err := parseDaemonScope(" local:default ")
	if err != nil || scope != (models.Scope{Kind: "local", ID: "default"}) {
		t.Fatalf("scope=%#v err=%v", scope, err)
	}
	for _, invalid := range []string{"", "local", ":default", "local:"} {
		if _, err := parseDaemonScope(invalid); err == nil {
			t.Fatalf("invalid scope %q was accepted", invalid)
		}
	}
}

func TestOneShotRunbookRemainsAvailableOutsideDaemonRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runbook.hcl")
	if err := os.WriteFile(path, []byte(`workflow "one_shot" {
  node "set" "assign" {
    name = "result"
    value = "portable"
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	wf, result, err := executeRunbook(t.Context(), path, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	if wf.Name != "one_shot" || result.Status != "completed" {
		t.Fatalf("workflow=%q status=%q", wf.Name, result.Status)
	}
	node := result.NodeResults["assign"]
	if node == nil {
		t.Fatal("assign node result is missing")
	}
	output, ok := node.Output.(map[string]interface{})
	if !ok || output["result"] != "portable" {
		t.Fatalf("node result = %#v", node)
	}
}

func TestDesktopOperatorRequiresAuthenticatedLocalWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name, listen, token, kind, id string
		valid                         bool
	}{
		{"local", "127.0.0.1:0", "synthetic-token", "local", "default", true},
		{"IPv6", "[::1]:0", "synthetic-token", "local", "default", true},
		{"public", "0.0.0.0:8080", "synthetic-token", "local", "default", false},
		{"missing token", "127.0.0.1:0", "", "local", "default", false},
		{"blank token", "127.0.0.1:0", "  ", "local", "default", false},
		{"tenant", "127.0.0.1:0", "synthetic-token", "tenant", "default", false},
		{"missing workspace", "127.0.0.1:0", "synthetic-token", "local", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDesktopOperator(tc.listen, tc.token, models.Scope{Kind: tc.kind, ID: tc.id})
			if (err == nil) != tc.valid {
				t.Fatalf("validation=%v, valid=%v", err, tc.valid)
			}
		})
	}
}
