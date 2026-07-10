package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestOpenKernelStorePersistsCanonicalRunsAcrossRestart(t *testing.T) {
	configDir := t.TempDir()
	config := StorageConfig{Driver: "sqlite", Path: "state/kernel.db"}
	store, resolved, err := OpenKernelStore(config, configDir)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(configDir, "state", "kernel.db")
	if resolved != wantPath {
		t.Fatalf("resolved path = %q, want %q", resolved, wantPath)
	}
	scope := runtime.Scope{Kind: "local", ID: "default"}
	created, err := runtime.NewRunCommandService(store).CreateAgentRun(context.Background(), runtime.CreateAgentRunRequest{
		Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"},
		AssignedAgentID: "operator", Goal: "Keep working after restart", Source: runtime.RunSourceManual,
		IdempotencyKey: "restart-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := OpenKernelStore(config, configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := runtime.NewPortfolioService(reopened).GetAgentRun(context.Background(), scope, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Goal != "Keep working after restart" || loaded.Revision != 1 {
		t.Fatalf("loaded run = %#v", loaded)
	}

	document := kernelapi.Capabilities()
	if _, ok := document.Find(kernelapi.AgentRunsCapabilityID, kernelapi.AgentRunsCapabilityVersion); !ok {
		t.Fatal("durable run API capability missing")
	}
}
