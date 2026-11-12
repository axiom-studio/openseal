package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
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
	scope := models.Scope{Kind: "local", ID: "default"}
	created, err := agentrun.NewRunCommandService(store).CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "operator"},
		AssignedAgentID: "operator", Goal: "Keep working after restart", Source: models.RunSourceManual,
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
	loaded, err := portfolio.NewService(reopened).GetAgentRun(context.Background(), scope, created.Run.ID)
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

func TestOpenArtifactContentStoreResolvesRelativeToConfig(t *testing.T) {
	configDir := t.TempDir()
	store, resolved, err := OpenArtifactContentStore(StorageConfig{ArtifactsPath: "state/artifacts"}, configDir)
	if err != nil {
		t.Fatal(err)
	}
	if store == nil || resolved != filepath.Join(configDir, "state", "artifacts") {
		t.Fatalf("store/resolved = %#v / %q", store, resolved)
	}
}
