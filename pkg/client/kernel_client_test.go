package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/internal/server"
	artifactstore "github.com/axiom-studio/openseal/pkg/artifact"
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
	contentStore, err := artifactstore.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	api.SetArtifactContentStore(contentStore)
	api.SetArtifactContentResolver(clientTestResolver{})
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	client := NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	scope := runtime.Scope{Kind: "local", ID: "artifacts"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "research"}
	content := []byte("report")
	stored, err := client.UploadArtifactContent(context.Background(), scope, "application/pdf", "", int64(len(content)), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.RegisterArtifact(context.Background(), runtime.RegisterArtifactRequest{Artifact: &runtime.Artifact{
		ID: "report", Version: 1, Scope: scope, Name: "report.pdf", Type: "report",
		MediaType: "application/pdf", ContentRef: stored.ContentRef, SizeBytes: stored.SizeBytes,
		Digest: stored.Digest, Classification: runtime.ArtifactClassificationConfidential,
		Provenance: runtime.ArtifactProvenance{Producer: runtime.ActivityActor{Type: "agent", ID: "analyst"}, Owner: &owner, RunID: "run-1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := client.GetArtifact(context.Background(), scope, created.Artifact.ID, 0)
	if err != nil || loaded.Fingerprint != created.Artifact.Fingerprint {
		t.Fatalf("loaded = %#v, %v", loaded, err)
	}
	listed, err := client.ListArtifacts(context.Background(), runtime.ArtifactFilter{
		Scope: scope, Owner: &owner, Types: []string{"report"}, ProducerRunID: "run-1", LatestOnly: true,
	})
	if err != nil || len(listed) != 1 || listed[0].ID != created.Artifact.ID {
		t.Fatalf("listed = %#v, %v", listed, err)
	}
	download, err := client.DownloadArtifactContent(context.Background(), scope, created.Artifact.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	loadedContent, err := io.ReadAll(download.Body)
	_ = download.Body.Close()
	if err != nil || !bytes.Equal(loadedContent, content) || download.Digest != stored.Digest {
		t.Fatalf("download = %q / %#v / %v", loadedContent, download, err)
	}
	resolution, err := client.ResolveArtifactContent(context.Background(), scope, created.Artifact.ID, 1, kernelapi.ResolveArtifactContentRequest{
		Actor: runtime.ActivityActor{Type: "user", ID: "operator"}, Purpose: "preview", TTLSeconds: 30,
	})
	if err != nil || resolution.URL != "https://delivery.example/ephemeral" {
		t.Fatalf("resolution = %#v, %v", resolution, err)
	}
}

type clientTestResolver struct{}

func (clientTestResolver) Resolve(_ context.Context, request runtime.ArtifactContentResolutionRequest) (runtime.ArtifactContentResolution, error) {
	return runtime.ArtifactContentResolution{URL: "https://delivery.example/ephemeral", ExpiresAt: time.Now().Add(request.TTL)}, nil
}
