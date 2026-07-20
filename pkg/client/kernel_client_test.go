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
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/internal/server"
	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	artifactstore "github.com/axiom-studio/openseal/pkg/artifact"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"go.uber.org/zap"
)

func TestKernelHTTPClientUsesCanonicalRunAPI(t *testing.T) {
	store := runtime.NewMemoryStore(100)
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
	api.SetAgentRunCreationDispatcher(runtime.NewRunCommandService(store).CreateAgentRun)
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()

	client := NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	ctx := context.Background()
	document, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	capability, ok := document.Find(kernelapi.AgentRunsCapabilityID, kernelapi.AgentRunsCapabilityVersion)
	if !ok || !capability.Supports(kernelapi.OperationCreate) || !capability.Supports(kernelapi.OperationIntervene) {
		t.Fatalf("unexpected capabilities: %#v", document)
	}
	objectiveCapability, ok := document.Find(kernelapi.ObjectivesCapabilityID, kernelapi.ObjectivesCapabilityVersion)
	if !ok || !objectiveCapability.Supports(kernelapi.OperationUpdate) {
		t.Fatalf("objective capabilities: %#v", document)
	}
	initiativeCapability, ok := document.Find(kernelapi.InitiativesCapabilityID, kernelapi.InitiativesCapabilityVersion)
	if !ok || !initiativeCapability.Supports(kernelapi.OperationPatch) {
		t.Fatalf("initiative capabilities: %#v", document)
	}
	activityCapability, ok := document.Find(kernelapi.ActivityCapabilityID, kernelapi.ActivityCapabilityVersion)
	if !ok || !activityCapability.Supports(kernelapi.OperationList) {
		t.Fatalf("activity capabilities: %#v", document)
	}
	eventCapability, ok := document.Find(kernelapi.EventRoutingCapabilityID, kernelapi.EventRoutingCapabilityVersion)
	if !ok || !eventCapability.Supports(kernelapi.OperationRoute) {
		t.Fatalf("event routing capabilities: %#v", document)
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
	if err := store.CreateInitiative(ctx, &runtime.Initiative{
		ID: "initiative-client", Scope: scope, Owner: owner, Title: "Client research", Purpose: "Collect evidence", Status: runtime.InitiativeStatusActive,
		ObjectiveRefs: []string{objective.ID}, Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.NewRunActivityService(store, store).AppendActivity(ctx, &runtime.ActivityEvent{
		ID: "source-policy-client", Scope: scope, InitiativeID: "initiative-client", RunID: created.Run.ID,
		EventType: "source_policy.authorized", Severity: runtime.ActivitySeverityInfo,
		Actor: runtime.ActivityActor{Type: "system", ID: "source-policy"}, Summary: "Source access authorized by policy",
		Payload:    map[string]interface{}{"monitorId": "monitor-client", "sourceHost": "www.reddit.com", "maximumItems": 5},
		Visibility: runtime.ActivityVisibilityScope, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	activity, err := client.ListActivity(ctx, runtime.ActivityFeedRequest{
		Scope: scope, RunID: created.Run.ID, EventTypes: []string{"source_policy.authorized"},
		Severities: []runtime.ActivitySeverity{runtime.ActivitySeverityInfo}, Visibilities: []runtime.ActivityVisibility{runtime.ActivityVisibilityScope},
		Limit: 5, IncludeDetails: true,
	})
	if err != nil || len(activity.Items) == 0 || activity.Items[0].InitiativeID != "initiative-client" || activity.Items[0].Payload["sourceHost"] != "www.reddit.com" {
		t.Fatalf("activity = %#v, %v", activity, err)
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
	eventObjective, err := client.CreateObjective(ctx, kernelapi.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Event monitor", Goal: "Investigate warnings", Status: runtime.ObjectiveStatusActive,
		EventRules: map[string]interface{}{"version": "1", "rules": []interface{}{map[string]interface{}{
			"id": "warning", "eventType": "service.warning", "assignedAgentId": owner.ID,
		}}},
	}, "event-objective")
	if err != nil {
		t.Fatal(err)
	}
	routed, err := client.RouteEvent(ctx, runtime.EventEnvelope{
		ID: "client-event-1", Scope: scope, Type: "service.warning", Source: "monitor:test",
		OccurredAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC), Payload: map[string]interface{}{"message": "latency high"},
	})
	if err != nil || len(routed.Routes) != 1 || !routed.Routes[0].Created || routed.Routes[0].Run.ObjectiveID != eventObjective.ID {
		t.Fatalf("routed event = %#v, %v", routed, err)
	}
	initiative, err := client.CreateInitiative(ctx, kernelapi.CreateInitiativeRequest{
		Scope: scope, Owner: owner, Title: "Research initiative", Purpose: "Deliver cited findings",
		Status: runtime.InitiativeStatusActive, ObjectiveRefs: []string{objective.ID},
	}, "research-initiative")
	if err != nil || initiative.Revision != 1 {
		t.Fatalf("initiative = %#v, %v", initiative, err)
	}
	loadedInitiative, err := client.GetInitiative(ctx, scope, initiative.ID)
	if err != nil || loadedInitiative.ID != initiative.ID {
		t.Fatalf("loaded initiative = %#v, %v", loadedInitiative, err)
	}
	initiativeStatus := runtime.InitiativeStatusPaused
	patchedInitiative, err := client.PatchInitiative(ctx, scope, initiative.ID, kernelapi.UpdateInitiativeRequest{
		ExpectedRevision: initiative.Revision, Status: &initiativeStatus,
	})
	if err != nil || patchedInitiative.Status != runtime.InitiativeStatusPaused {
		t.Fatalf("patched initiative = %#v, %v", patchedInitiative, err)
	}
	initiatives, err := client.ListInitiatives(ctx, runtime.InitiativeFilter{
		Scope: scope, Owner: &owner, ObjectiveID: objective.ID, Statuses: []runtime.InitiativeStatus{runtime.InitiativeStatusPaused}, Limit: 20,
	})
	if err != nil || len(initiatives) != 1 || initiatives[0].ID != initiative.ID {
		t.Fatalf("initiatives = %#v, %v", initiatives, err)
	}

	runs, err := client.ListAgentRuns(ctx, runtime.AgentRunFilter{Scope: scope, Kind: runtime.RunKindAgentWork, Owner: &owner, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	runIDs := map[string]bool{}
	for _, run := range runs {
		runIDs[run.ID] = run.Kind == runtime.RunKindAgentWork
	}
	if len(runs) != 2 || !runIDs[created.Run.ID] || !runIDs[routed.Routes[0].Run.ID] {
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

func TestKernelHTTPClientUpdatesAgentDeploymentThroughCanonicalAPI(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "agent-update.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry := kernelagent.NewRegistryWithStore(store)
	definition, err := registry.RegisterDefinition(t.Context(), &kernelagent.AgentDefinition{
		ID: "operator", Version: "1", DisplayName: "Operator", Purpose: "Operate", SystemPrompt: "Inspect first.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelProduction, MaxConcurrentRuns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	deployment, _, err := registry.CreateDeployment(t.Context(), &kernelagent.AgentDeployment{
		ID: "operator-live", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "production", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "initial")
	if err != nil {
		t.Fatal(err)
	}
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	client := NewKernelHTTPClient(httpServer.URL, httpServer.Client())

	proposed := *deployment
	proposed.RolloutStatus = kernelagent.RolloutPaused
	proposed.Credentials = map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "model-provider", ID: "vault://tenant-one/provider"}}
	result, err := client.UpdateAgentDeployment(t.Context(), deployment.ID, kernelapi.UpdateAgentDeploymentRequest{
		Deployment: &proposed, ExpectedRevision: deployment.Revision, ActorType: "system", ActorID: "reconciler", Reason: "place provider",
	})
	if err != nil || result.Deployment == nil || result.Audit == nil || result.Deployment.RolloutStatus != kernelagent.RolloutPaused ||
		result.Deployment.Credentials["MODEL_PROVIDER"].ID != "vault://tenant-one/provider" || result.Audit.ChangeKind != workforce.DeploymentChangeConfigurationUpdated {
		t.Fatalf("update result = %#v, %v", result, err)
	}
}

func TestKernelHTTPClientConsumesHostedRootAndEnvelope(t *testing.T) {
	scope := capability.ScopeReference{Kind: "tenant", ID: "7"}
	changeSet := &authoring.ChangeSet{
		ID: "change-hosted", Scope: scope, Status: authoring.ChangeSetReview, Revision: 4,
		Placement: authoring.ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
			"sre": {"kubernetes-cluster": {Kind: "kubernetes-cluster", ID: "cluster://7"}},
		}},
	}
	capabilityDocument := kernelapi.WorkforceAuthoringCapability(kernelapi.WorkforceAuthoringCapabilityFeatures{ChangeSets: true})
	capabilityDocument.Operations = append(capabilityDocument.Operations, kernelapi.OperationPatch)
	capabilityDocument.Context = &kernelapi.CapabilityContext{
		ChangeSetID: changeSet.ID, Revision: changeSet.Revision,
		CredentialBindings: []capability.CredentialBindingChoice{{
			Reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://7"}, DisplayName: "Development",
		}},
	}
	document := kernelapi.NewCapabilityDocument(capabilityDocument)
	var placement authoring.UpdateChangeSetPlacementRequest
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Workspace-Scope") != "tenant:7" {
			t.Errorf("scope header = %q", request.Header.Get("X-Workspace-Scope"))
		}
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/host/kernel/v1/capabilities":
			if request.URL.Query().Get("changeSetId") != changeSet.ID || request.URL.Query().Get("scopeKind") != scope.Kind || request.URL.Query().Get("scopeId") != scope.ID {
				t.Errorf("capability query = %s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{"code": http.StatusOK, "status": "OK", "result": document})
		case request.Method == http.MethodPatch && request.URL.Path == "/host/kernel/v1/authoring/workforce/change-sets/change-hosted/placement":
			if request.Header.Get("Idempotency-Key") != "placement-hosted" {
				t.Errorf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
			}
			if err := json.NewDecoder(request.Body).Decode(&placement); err != nil {
				t.Errorf("decode placement: %v", err)
			}
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{"code": http.StatusAccepted, "status": "Accepted", "result": changeSet})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer httpServer.Close()

	client := NewKernelHTTPClient(httpServer.URL+"/host/kernel/v1", httpServer.Client(), WithRequestHeaders(http.Header{
		"X-Workspace-Scope": {"tenant:7"},
		// Protocol-owned headers must remain request-specific.
		"Idempotency-Key": {"caller-must-not-override"},
	}))
	gotDocument, err := client.WorkforceChangeSetCapabilities(t.Context(), scope, changeSet.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotCapability, ok := gotDocument.Find(kernelapi.WorkforceAuthoringCapabilityID, kernelapi.WorkforceAuthoringCapabilityVersion)
	if !ok || gotCapability.Context == nil || len(gotCapability.Context.CredentialBindings) != 1 || gotCapability.Context.CredentialBindings[0].DisplayName != "Development" {
		t.Fatalf("hosted capability = %#v", gotDocument)
	}
	updated, err := client.UpdateWorkforceChangeSetPlacement(t.Context(), authoring.UpdateChangeSetPlacementRequest{
		ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision, Placement: changeSet.Placement,
	}, "placement-hosted")
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != changeSet.ID || placement.Placement.CredentialReferences["sre"]["kubernetes-cluster"].ID != "cluster://7" {
		t.Fatalf("updated=%#v placement=%#v", updated, placement)
	}
}

func TestKernelHTTPClientStreamsArtifactsThroughHostedRoot(t *testing.T) {
	scope := runtime.Scope{Kind: "tenant", ID: "7"}
	content := []byte("cited findings")
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Workspace-Scope") != "tenant:7" {
			t.Errorf("scope header = %q", request.Header.Get("X-Workspace-Scope"))
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/host/kernel/v1/artifact-content":
			if request.URL.Query().Get("scopeKind") != scope.Kind || request.URL.Query().Get("scopeId") != scope.ID {
				t.Errorf("upload query = %s", request.URL.RawQuery)
			}
			if request.Header.Get("Content-Type") != "application/pdf" || request.Header.Get("X-Content-SHA256") != "sha256:report" {
				t.Errorf("upload headers = %#v", request.Header)
			}
			body, err := io.ReadAll(request.Body)
			if err != nil || !bytes.Equal(body, content) {
				t.Errorf("upload body = %q, %v", body, err)
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{"code": http.StatusCreated, "result": runtime.ArtifactStoredContent{ContentRef: "content:report", Digest: "sha256:report", SizeBytes: int64(len(content))}})
		case request.Method == http.MethodGet && request.URL.Path == "/host/kernel/v1/artifacts/report/content":
			if request.URL.Query().Get("scopeKind") != scope.Kind || request.URL.Query().Get("scopeId") != scope.ID || request.URL.Query().Get("version") != "2" {
				t.Errorf("download query = %s", request.URL.RawQuery)
			}
			writer.Header().Set("Content-Type", "application/pdf")
			writer.Header().Set("Content-Disposition", `attachment; filename="report.pdf"`)
			writer.Header().Set("X-Content-SHA256", "sha256:report")
			_, _ = writer.Write(content)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer httpServer.Close()

	client := NewKernelHTTPClient(httpServer.URL+"/host/kernel/v1", httpServer.Client(), WithRequestHeaders(http.Header{"X-Workspace-Scope": {"tenant:7"}}))
	stored, err := client.UploadArtifactContent(t.Context(), scope, "application/pdf", "sha256:report", int64(len(content)), bytes.NewReader(content))
	if err != nil || stored.ContentRef != "content:report" || stored.SizeBytes != int64(len(content)) {
		t.Fatalf("stored content = %#v, %v", stored, err)
	}
	download, err := client.DownloadArtifactContent(t.Context(), scope, "report", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Body.Close()
	downloaded, err := io.ReadAll(download.Body)
	if err != nil || !bytes.Equal(downloaded, content) || download.MediaType != "application/pdf" || download.Digest != "sha256:report" {
		t.Fatalf("download = %q %#v, %v", downloaded, download, err)
	}
}

func TestKernelHTTPClientListsAgentDefinitionCompilations(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "compilations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	ctx := context.Background()
	client := NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	document, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	definitionCapability, ok := document.Find(kernelapi.AgentDefinitionsCapabilityID, kernelapi.AgentDefinitionsCapabilityVersion)
	if !ok || !definitionCapability.Supports(kernelapi.OperationListCompilations) {
		t.Fatalf("Agent definition capabilities = %#v", document)
	}
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	registry := kernelagent.NewRegistryWithStore(store)
	definition, err := registry.RegisterDefinition(ctx, &kernelagent.AgentDefinition{ID: "researcher", Version: "1", DisplayName: "Researcher", Purpose: "Research", SystemPrompt: "Research safely.", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{ID: "researcher", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "local", "test"); err != nil {
		t.Fatal(err)
	}
	listed, err := client.ListAgentDeployments(ctx, scope)
	if err != nil || len(listed.Items) != 1 || listed.Items[0].Deployment.ID != "researcher" || listed.Items[0].Definition.DisplayName != "Researcher" {
		t.Fatalf("Agent catalog = %#v, %v", listed, err)
	}
	detail, err := client.GetAgentDeployment(ctx, scope, "researcher")
	if err != nil || detail.Deployment.ID != "researcher" || detail.Definition.Version != "1" {
		t.Fatalf("Agent detail = %#v, %v", detail, err)
	}
	if _, err := registry.RecordCompilation(ctx, &kernelagent.DefinitionCompilation{ID: "researcher-1", Scope: scope, DeploymentID: "researcher", DefinitionID: definition.ID, CandidateVersion: "1", Source: kernelagent.CompilationSource{Kind: "prompt", ID: "source", Version: "1", Digest: "sha256:source"}, TargetDigest: definition.Digest, Status: kernelagent.CompilationClean}); err != nil {
		t.Fatal(err)
	}
	compilations, err := client.ListAgentDefinitionCompilations(ctx, scope, "researcher")
	if err != nil || len(compilations) != 1 || compilations[0].ID != "researcher-1" {
		t.Fatalf("Agent compilations = %#v, %v", compilations, err)
	}
}

func TestKernelHTTPClientListsExactAgentSkillActions(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "skills.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := skill.NewCatalogWithStore(store)
	definition := &skill.Definition{
		ID: "community", Version: "1", Name: "Community", Transport: skill.TransportReference{Kind: "remote-node"},
		Actions: map[string]skill.Action{"reply": {
			Name: "reply", Description: "Reply to a community thread", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal,
			Idempotency: skill.IdempotencyRequired, SemanticArguments: map[string]string{"target": "thread", "body": "message"},
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{
				"thread": map[string]interface{}{"type": "string"}, "message": map[string]interface{}{"type": "string"},
			}, "required": []interface{}{"thread", "message"}},
		}},
	}
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	if err := catalog.Bind(context.Background(), &skill.Binding{
		ID: "community-account", Scope: scope, DeploymentID: "researcher", SkillID: definition.ID, SkillVersion: definition.Version,
		AllowedActions: []string{"reply"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()

	result, err := NewKernelHTTPClient(httpServer.URL, httpServer.Client()).ListAgentSkillActions(
		context.Background(), capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher", []string{"target", "body"}, capability.SideEffectExternal,
	)
	if err != nil || result.DeploymentID != "researcher" || len(result.Actions) != 1 {
		t.Fatalf("actions = %#v, %v", result, err)
	}
	action := result.Actions[0]
	if action.BindingID != "community-account" || action.BindingRevision != 1 || action.SemanticArguments["target"] != "thread" {
		t.Fatalf("action = %#v", action)
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
			capability := kernelapi.WorkforceAuthoringCapability(kernelapi.WorkforceAuthoringCapabilityFeatures{ChangeSets: true})
			capability.Operations = append(capability.Operations, kernelapi.OperationApply)
			_ = json.NewEncoder(w).Encode(kernelapi.NewCapabilityDocument(capability))
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
	if _, err := client.UpdateWorkforceChangeSetPlacement(ctx, authoring.UpdateChangeSetPlacementRequest{Scope: scope, ChangeSetID: "change/one"}, "placement-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AnswerWorkforceChangeSetRefinement(ctx, authoring.AnswerChangeSetRefinementRequest{Scope: scope, ChangeSetID: "change/one", QuestionID: "sources"}, "refine-1"); err != nil {
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
		"PATCH /api/v1/authoring/workforce/change-sets/change%2Fone/placement key=placement-1",
		"POST /api/v1/authoring/workforce/change-sets/change%2Fone/refinements key=refine-1",
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
			Evaluations:  []workforce.EvaluationCriterion{{ID: "evidence", Description: "Evidence remains attributable", Required: true}},
			Amendments:   workforce.AmendmentPolicy{AllowedFields: []string{"purpose"}, RequiresApproval: true, ApproverPrincipals: []string{"user:operator"}},
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
	listed, err := client.ListTeamDeployments(ctx, scope)
	if err != nil || len(listed.Items) != 1 || listed.Items[0].Deployment.ID != created.Deployment.ID || listed.Items[0].Definition.ID != "research-team" {
		t.Fatalf("listed Teams = %#v, err = %v", listed, err)
	}
	skillCatalog := skill.NewCatalogWithStore(store)
	if err := skillCatalog.Register(ctx, &skill.Definition{
		ID: "reader", Version: "1", Name: "Reader", Transport: capability.TransportReference{Kind: "local"},
		Actions: map[string]capability.Action{"read": {Name: "read", Description: "Read", Risk: capability.RiskLevelRead, SideEffect: capability.SideEffectRead, Idempotency: capability.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}}},
	}); err != nil {
		t.Fatal(err)
	}
	agentOwner := SkillBindingOwner{Type: runtime.OwnerTypeAgent, DeploymentID: agentDeployment.ID}
	agentBinding, err := client.UpsertSkillBinding(ctx, agentOwner, skill.UpsertBindingRequest{
		Binding: &skill.Binding{ID: "reader", Scope: scope, DeploymentID: agentDeployment.ID, SkillID: "reader", SkillVersion: "1", AllowedActions: []string{"read"}, MaximumRisk: capability.RiskLevelRead},
		Actor:   skill.BindingActor{Type: "user", ID: "operator"}, Reason: "attach exact reader to Agent",
	})
	if err != nil || agentBinding.Binding == nil || agentBinding.Binding.DeploymentID != agentDeployment.ID {
		t.Fatalf("created Agent Skill binding = %#v, err = %v", agentBinding, err)
	}
	agentBindings, err := client.ListSkillBindings(ctx, scope, agentOwner)
	if err != nil || len(agentBindings.Items) != 1 {
		t.Fatalf("listed Agent Skill bindings = %#v, err = %v", agentBindings, err)
	}
	teamBinding, err := client.UpsertTeamSkillBinding(ctx, created.Deployment.ID, skill.UpsertBindingRequest{
		Binding: &skill.Binding{ID: "reader", Scope: scope, DeploymentID: created.Deployment.ID, SkillID: "reader", SkillVersion: "1", AllowedActions: []string{"read"}, MaximumRisk: capability.RiskLevelRead},
		Actor:   skill.BindingActor{Type: "user", ID: "operator"}, Reason: "share reader with Team",
	})
	if err != nil || teamBinding.Binding == nil || teamBinding.Binding.Revision != 1 {
		t.Fatalf("created Team Skill binding = %#v, err = %v", teamBinding, err)
	}
	teamBindings, err := client.ListTeamSkillBindings(ctx, scope, created.Deployment.ID)
	if err != nil || len(teamBindings.Items) != 1 || teamBindings.Items[0].DeploymentID != created.Deployment.ID {
		t.Fatalf("listed Team Skill bindings = %#v, err = %v", teamBindings, err)
	}
	disabledTeamBinding, err := client.DisableTeamSkillBinding(ctx, created.Deployment.ID, skill.DisableBindingRequest{
		Scope: scope, DeploymentID: created.Deployment.ID, BindingID: "reader", ExpectedRevision: 1,
		Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "retire reader",
	})
	if err != nil || disabledTeamBinding.Binding == nil || !disabledTeamBinding.Binding.Disabled || disabledTeamBinding.Binding.Revision != 2 {
		t.Fatalf("disabled Team Skill binding = %#v, err = %v", disabledTeamBinding, err)
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
	base, err := client.GetTeamDefinition(ctx, "research-team", "2")
	if err != nil {
		t.Fatal(err)
	}
	candidate := *base
	candidate.Version, candidate.Purpose, candidate.Digest = "3", "Produce attributable findings and syntheses", ""
	amendment, err := client.ProposeTeamDefinitionAmendment(ctx, kernelteam.ProposeAmendmentRequest{
		Scope: scope, DeploymentID: loaded.ID, Candidate: &candidate, ProposerType: "user", ProposerID: "operator", Rationale: "Include synthesis",
	})
	if err != nil || amendment.Status != kernelteam.AmendmentEvaluating || amendment.Revision != 1 {
		t.Fatalf("proposed amendment = %#v, err = %v", amendment, err)
	}
	amendments, err := client.ListTeamDefinitionAmendments(ctx, scope, loaded.ID)
	if err != nil || len(amendments.Items) != 1 || amendments.Items[0].ID != amendment.ID {
		t.Fatalf("listed amendments = %#v, err = %v", amendments, err)
	}
	inspected, err := client.GetTeamDefinitionAmendment(ctx, scope, loaded.ID, amendment.ID)
	if err != nil || inspected.Revision != amendment.Revision {
		t.Fatalf("inspected amendment = %#v, err = %v", inspected, err)
	}
	evaluated, err := client.SubmitTeamDefinitionAmendmentEvaluation(ctx, loaded.ID, kernelteam.SubmitAmendmentEvaluationRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision,
		Evaluations: []kernelteam.AmendmentEvaluation{{CriterionID: "evidence", Passed: true, Summary: "Sources remain attributable"}},
	})
	if err != nil || evaluated.Status != kernelteam.AmendmentAwaitingApproval || evaluated.Revision != 2 {
		t.Fatalf("evaluated amendment = %#v, err = %v", evaluated, err)
	}
	approved, err := client.ResolveTeamDefinitionAmendment(ctx, loaded.ID, kernelteam.ResolveAmendmentRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: evaluated.Revision, Approved: true,
		ActorType: "user", ActorID: "operator", Reason: "Evidence gate passed",
	})
	if err != nil || approved.Status != kernelteam.AmendmentApproved || approved.Revision != 3 {
		t.Fatalf("approved amendment = %#v, err = %v", approved, err)
	}
	amendmentActivation, err := client.ActivateTeamDefinitionAmendment(ctx, loaded.ID, amendment.ID, kernelapi.ActivateTeamDefinitionAmendmentRequest{
		Scope: scope, ExpectedRevision: approved.Revision, ActorType: "user", ActorID: "operator", Reason: "Reviewed",
	})
	if err != nil || amendmentActivation.Amendment.Status != kernelteam.AmendmentActivated || amendmentActivation.Deployment.ActiveVersion != "3" || amendmentActivation.Activation.ToVersion != "3" {
		t.Fatalf("activated amendment = %#v, err = %v", amendmentActivation, err)
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

func TestKernelHTTPClientMapsClawHubLifecycleContract(t *testing.T) {
	requests := make([]string, 0)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/installed"):
			_ = json.NewEncoder(w).Encode([]clawhub.InstalledState{{APIVersion: clawhub.LifecycleAPIVersion, SourceIdentity: "source", Reference: clawhub.SkillReference{Owner: "acme", Slug: "research"}, Version: "1.0.0"}})
		case strings.HasSuffix(r.URL.Path, "/update-all"):
			_ = json.NewEncoder(w).Encode(clawhub.LifecycleBatchResult{APIVersion: clawhub.LifecycleAPIVersion, Operation: clawhub.LifecycleUpdateAll})
		default:
			_ = json.NewEncoder(w).Encode(clawhub.LifecycleResult{APIVersion: clawhub.LifecycleAPIVersion, Operation: clawhub.LifecycleInstall, SourceIdentity: "source", Version: "1.0.0", Changed: true})
		}
	}))
	defer api.Close()
	client := NewKernelHTTPClient(api.URL, api.Client())
	ctx, reference := context.Background(), clawhub.SkillReference{Owner: "acme", Slug: "research"}
	if result, err := client.InstallClawHubSkill(ctx, reference, kernelapi.ClawHubVersionRequest{Version: "1.0.0"}); err != nil || !result.Changed {
		t.Fatalf("install=%#v err=%v", result, err)
	}
	if states, err := client.ListInstalledClawHubSkills(ctx); err != nil || len(states) != 1 {
		t.Fatalf("states=%#v err=%v", states, err)
	}
	if _, err := client.PinClawHubSkill(ctx, reference.String(), "reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateAllClawHubSkills(ctx); err != nil {
		t.Fatal(err)
	}
	wantPath := "POST /api/v1/clawhub/catalog/@acme%2Fresearch/install"
	if len(requests) != 4 || requests[0] != wantPath {
		t.Fatalf("requests=%#v want first %q", requests, wantPath)
	}
}
