package commands

import (
	"os"
	"path/filepath"
	"testing"

	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/outreach"
	"github.com/axiom-studio/openseal/pkg/runtime"
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
	if err != nil || scope != (runtime.Scope{Kind: "local", ID: "default"}) {
		t.Fatalf("scope=%#v err=%v", scope, err)
	}
	for _, invalid := range []string{"", "local", ":default", "local:"} {
		if _, err := parseDaemonScope(invalid); err == nil {
			t.Fatalf("invalid scope %q was accepted", invalid)
		}
	}
}

func TestCanonicalDaemonSkillRegistrationSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := opensealkernel.New(opensealkernel.WithPersistentStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureCanonicalSkill(t.Context(), engine, outreach.SkillDefinition()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := opensealkernel.New(opensealkernel.WithPersistentStore(reopened))
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureCanonicalSkill(t.Context(), restarted, outreach.SkillDefinition()); err != nil {
		t.Fatalf("idempotent registration after restart: %v", err)
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
