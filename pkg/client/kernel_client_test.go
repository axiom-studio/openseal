package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/internal/server"
	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	artifactstore "github.com/axiom-studio/openseal/pkg/artifact"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
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
	objectiveCapability, ok := document.Find(kernelapi.ObjectivesCapabilityID, kernelapi.ObjectivesCapabilityVersion)
	if !ok || !objectiveCapability.Supports(kernelapi.OperationUpdate) {
		t.Fatalf("objective capabilities: %#v", document)
	}

	scope := runtime.Scope{Kind: "local", ID: "default"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "researcher"}
	objective, err := client.CreateObjective(ctx, kernelapi.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Research", Goal: "Monitor product feedback", Status: runtime.ObjectiveStatusActive,
		Budget: &runtime.BudgetPolicy{MaxTurns: 40},
	}, "research-objective")
	if err != nil {
		t.Fatal(err)
	}
	if objective.Budget == nil || objective.Budget.MaxTurns != 40 {
		t.Fatalf("objective = %#v", objective)
	}
	created, err := client.CreateAgentRun(ctx, kernelapi.CreateAgentRunRequest{
		Scope: scope, Kind: runtime.RunKindAgentWork, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Monitor product feedback", Source: runtime.RunSourceManual, Budget: &runtime.BudgetPolicy{MaxTurns: 24},
	}, "stable-request")
	if err != nil {
		t.Fatal(err)
	}
	if created.Run == nil || created.Run.Revision != 1 || created.Run.Budget == nil || created.Run.Budget.MaxTurns != 24 {
		t.Fatalf("unexpected create result: %#v", created)
	}

	replayed, err := client.CreateAgentRun(ctx, kernelapi.CreateAgentRunRequest{
		Scope: scope, Kind: runtime.RunKindAgentWork, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "Monitor product feedback", Source: runtime.RunSourceManual, Budget: &runtime.BudgetPolicy{MaxTurns: 24},
	}, "stable-request")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Event != nil || replayed.Run.ID != created.Run.ID {
		t.Fatalf("idempotent replay = %#v", replayed)
	}
	detail, err := client.GetObjective(ctx, scope, objective.ID)
	if err != nil || detail.Objective == nil || len(detail.Runs) != 1 {
		t.Fatalf("objective detail = %#v, %v", detail, err)
	}
	pausedStatus := runtime.ObjectiveStatusPaused
	updatedObjective, err := client.UpdateObjective(ctx, scope, objective.ID, kernelapi.UpdateObjectiveRequest{
		ExpectedRevision: detail.Objective.Revision, Status: &pausedStatus,
	})
	if err != nil || updatedObjective.Status != runtime.ObjectiveStatusPaused {
		t.Fatalf("updated objective = %#v, %v", updatedObjective, err)
	}
	objectives, err := client.ListObjectives(ctx, runtime.ObjectiveFilter{Scope: scope, Owner: &owner, Statuses: []runtime.ObjectiveStatus{runtime.ObjectiveStatusPaused}})
	if err != nil || len(objectives) != 1 || objectives[0].ID != objective.ID {
		t.Fatalf("objectives = %#v, %v", objectives, err)
	}

	runs, err := client.ListAgentRuns(ctx, runtime.AgentRunFilter{Scope: scope, Kind: runtime.RunKindAgentWork, Owner: &owner, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != created.Run.ID || runs[0].Kind != runtime.RunKindAgentWork {
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

func TestKernelHTTPClientUsesGovernedWorkforceLifecycleContract(t *testing.T) {
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	requests := make([]string, 0, 4)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI()+" key="+r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/capabilities" {
			_ = json.NewEncoder(w).Encode(kernelapi.NewCapabilityDocument(kernelapi.WorkforceAuthoringCapability(true, true)))
			return
		}
		_ = json.NewEncoder(w).Encode(authoring.ChangeSet{ID: "change/one", Scope: scope, Revision: 2})
	}))
	defer httpServer.Close()
	client := NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	ctx := context.Background()
	if _, err := client.WorkforceChangeSetCapabilities(ctx, scope, "change/one"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EvaluateWorkforceChangeSet(ctx, authoring.SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: "change/one"}, "evaluate-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ResolveWorkforceChangeSetApproval(ctx, authoring.ResolveChangeSetApprovalRequest{Scope: scope, ChangeSetID: "change/one"}, "approve-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ApplyWorkforceChangeSet(ctx, authoring.ApplyChangeSetRequest{Scope: scope, ChangeSetID: "change/one"}, "apply-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /api/v1/capabilities?changeSetId=change%2Fone&scopeId=one&scopeKind=tenant key=",
		"POST /api/v1/authoring/workforce/change-sets/change%2Fone/evaluations key=evaluate-1",
		"POST /api/v1/authoring/workforce/change-sets/change%2Fone/approvals key=approve-1",
		"POST /api/v1/authoring/workforce/change-sets/change%2Fone/apply key=apply-1",
	}
	if len(requests) != len(want) {
		t.Fatalf("requests = %#v", requests)
	}
	for i := range want {
		if requests[i] != want[i] {
			t.Fatalf("request[%d] = %q, want %q", i, requests[i], want[i])
		}
	}
}

func TestKernelHTTPClientUsesFirstClassTeamAPI(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "teams.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "workspace", ID: "local"}
	agents := kernelagent.NewRegistryWithStore(store)
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "researcher", Version: "1", DisplayName: "Researcher", Purpose: "Find evidence", SystemPrompt: "Find evidence.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "researcher-one", Scope: scope, DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "Team roster")
	if err != nil {
		t.Fatal(err)
	}
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	client := NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	for _, version := range []string{"1", "2"} {
		registered, err := client.RegisterTeamDefinition(ctx, &kernelteam.Definition{
			ID: "research-team", Version: version, DisplayName: "Research Team", Purpose: "Produce findings",
			Roles:        []kernelteam.RoleSlot{{ID: "researcher", DisplayName: "Researcher", Purpose: "Find evidence", MinimumMembers: 1}},
			Coordination: kernelteam.CoordinationPolicy{Mode: kernelteam.CoordinationDynamic, QuietByDefault: true},
			Approvals:    kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead, ApproverRoleIDs: []string{"researcher"}},
		})
		if err != nil || registered.Digest == "" {
			t.Fatalf("registered Team definition = %#v, err = %v", registered, err)
		}
	}
	versions, err := client.ListTeamDefinitionVersions(ctx, "research-team")
	if err != nil || len(versions) != 2 {
		t.Fatalf("Team versions = %#v, err = %v", versions, err)
	}
	created, err := client.CreateTeamDeployment(ctx, kernelapi.CreateTeamDeploymentRequest{
		Deployment: &kernelteam.Deployment{
			ID: "research-team-one", Scope: scope, DefinitionID: "research-team", ActiveVersion: "1", Status: kernelteam.DeploymentActive,
			Roster: []kernelteam.RosterAssignment{{ID: "researcher", RoleID: "researcher", AgentDeploymentID: agentDeployment.ID}},
		},
		ActorType: "user", ActorID: "operator", Reason: "initial",
	})
	if err != nil || created.Activation.ToVersion != "1" {
		t.Fatalf("created Team = %#v, err = %v", created, err)
	}
	proposed := *created.Deployment
	proposed.Status = kernelteam.DeploymentPaused
	proposed.Roster = append([]kernelteam.RosterAssignment(nil), created.Deployment.Roster...)
	proposed.Roster[0].DisplayName = "Evidence lead"
	updated, err := client.UpdateTeamDeployment(ctx, proposed.ID, kernelapi.UpdateTeamDeploymentRequest{
		Deployment: &proposed, ExpectedRevision: proposed.Revision, ActorType: "user", ActorID: "operator", Reason: "pause for review",
	})
	if err != nil || updated.Deployment.Status != kernelteam.DeploymentPaused || updated.Deployment.Revision != 2 || updated.Activation.FromVersion != updated.Activation.ToVersion {
		t.Fatalf("updated Team = %#v, err = %v", updated, err)
	}
	activated, err := client.ActivateTeamDefinition(ctx, created.Deployment.ID, kernelapi.ActivateTeamDefinitionRequest{
		Scope: scope, Version: "2", ExpectedRevision: updated.Deployment.Revision, ActorType: "user", ActorID: "operator", Reason: "reviewed",
	})
	if err != nil || activated.Deployment.ActiveVersion != "2" || activated.Activation.FromVersion != "1" {
		t.Fatalf("activated Team = %#v, err = %v", activated, err)
	}
	loaded, err := client.GetTeamDeployment(ctx, scope, created.Deployment.ID)
	if err != nil || loaded.Revision != 3 || loaded.Roster[0].DisplayName != "Evidence lead" || loaded.Roster[0].AgentDeploymentID != agentDeployment.ID {
		t.Fatalf("loaded Team = %#v, err = %v", loaded, err)
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
