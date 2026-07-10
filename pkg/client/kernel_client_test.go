package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/axiom-studio/openseal/internal/server"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestKernelHTTPClientUsesCanonicalRunAPI(t *testing.T) {
	store := runtime.NewMemoryStore(100)
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()

	client := NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	ctx := context.Background()
	document, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	capability, ok := document.Find(kernelapi.AgentRunsCapabilityID, kernelapi.AgentRunsCapabilityVersion)
	if !ok || !capability.Supports(kernelapi.OperationIntervene) {
		t.Fatalf("unexpected capabilities: %#v", document)
	}

	scope := runtime.Scope{Kind: "local", ID: "default"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "researcher"}
	created, err := client.CreateAgentRun(ctx, kernelapi.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Monitor product feedback", Source: runtime.RunSourceManual,
	}, "stable-request")
	if err != nil {
		t.Fatal(err)
	}
	if created.Run == nil || created.Run.Revision != 1 {
		t.Fatalf("unexpected create result: %#v", created)
	}

	replayed, err := client.CreateAgentRun(ctx, kernelapi.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Monitor product feedback", Source: runtime.RunSourceManual,
	}, "stable-request")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Event != nil || replayed.Run.ID != created.Run.ID {
		t.Fatalf("idempotent replay = %#v", replayed)
	}

	runs, err := client.ListAgentRuns(ctx, runtime.AgentRunFilter{Scope: scope, Owner: &owner, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != created.Run.ID {
		t.Fatalf("runs = %#v", runs)
	}

	paused, err := client.CommandAgentRun(ctx, scope, created.Run.ID, kernelapi.AgentRunCommandRequest{
		ExpectedRevision: created.Run.Revision, Kind: runtime.AgentRunCommandPause,
	})
	if err != nil {
		t.Fatal(err)
	}
	if paused.Run.Status != runtime.AgentRunStatusPaused {
		t.Fatalf("status = %s", paused.Run.Status)
	}

	loaded, err := client.GetAgentRun(ctx, scope, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != paused.Run.Revision {
		t.Fatalf("loaded revision = %d, want %d", loaded.Revision, paused.Run.Revision)
	}
}

func TestKernelHTTPClientReturnsTypedAPIErrors(t *testing.T) {
	store := runtime.NewMemoryStore(100)
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()

	client := NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	_, err := client.GetAgentRun(context.Background(), runtime.Scope{Kind: "local", ID: "default"}, "missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
		t.Fatalf("error = %#v", err)
	}
}

func TestKernelHTTPClientUsesArtifactCatalogAPI(t *testing.T) {
	store := runtime.NewMemoryStore(100)
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	client := NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	scope := runtime.Scope{Kind: "local", ID: "artifacts"}
	digest := sha256.Sum256([]byte("report"))
	created, err := client.RegisterArtifact(context.Background(), runtime.RegisterArtifactRequest{Artifact: &runtime.Artifact{
		ID: "report", Version: 1, Scope: scope, Name: "report.pdf", Type: "report",
		MediaType: "application/pdf", ContentRef: "object-store:report",
		Digest: "sha256:" + hex.EncodeToString(digest[:]), Classification: runtime.ArtifactClassificationConfidential,
		Provenance: runtime.ArtifactProvenance{Producer: runtime.ActivityActor{Type: "agent", ID: "analyst"}, RunID: "run-1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := client.GetArtifact(context.Background(), scope, created.Artifact.ID, 0)
	if err != nil || loaded.Fingerprint != created.Artifact.Fingerprint {
		t.Fatalf("loaded = %#v, %v", loaded, err)
	}
	listed, err := client.ListArtifacts(context.Background(), runtime.ArtifactFilter{
		Scope: scope, Types: []string{"report"}, ProducerRunID: "run-1", LatestOnly: true,
	})
	if err != nil || len(listed) != 1 || listed[0].ID != created.Artifact.ID {
		t.Fatalf("listed = %#v, %v", listed, err)
	}
}
