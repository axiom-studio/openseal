package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"go.uber.org/zap"
)

func TestStandaloneOutreachRoutesMatchAdvertisedLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "outreach.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	scope := runtime.Scope{Kind: "local", ID: "research"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "research-team"}
	objective, err := runtime.NewPortfolioService(store).CreateObjective(ctx, runtime.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Research", Goal: "Collect feedback", Status: runtime.ObjectiveStatusActive,
		Cadence: &runtime.ObjectiveCadence{Type: runtime.ObjectiveCadenceInterval, IntervalSeconds: 300, AssignedAgentID: "research-agent", RunTemplate: &runtime.ObjectiveRunTemplate{
			Context: map[string]interface{}{"initiativeId": "initiative-1", "sourceMonitorId": "monitor-1"},
			Policy:  map[string]interface{}{"sourcePolicyRef": "public-forum@1"}, Capability: &runtime.ObjectiveCapabilityInvocation{SkillID: "source", SkillVersion: "1.0.0", Action: "observe"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	initiative, _, err := runtime.NewInitiativeService(store, store).Create(ctx, runtime.CreateInitiativeRequest{Initiative: &runtime.Initiative{
		ID: "initiative-1", Scope: scope, Owner: owner, Title: "Research Initiative", Purpose: "Understand user pain", Status: runtime.InitiativeStatusActive,
		ObjectiveRefs: []string{objective.ID}, SourceMonitors: []runtime.SourceMonitorReference{{
			ID: "monitor-1", ObjectiveID: objective.ID, AssignedAgentID: "research-agent", SkillID: "source", SkillVersion: "1.0.0", Action: "observe",
			SourcePolicyRef: "public-forum@1", Deduplication: runtime.SourceMonitorDeduplicateStableSourceAndContent,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sourceRun, err := runtime.NewPortfolioService(store).CreateAgentRun(ctx, runtime.CreateAgentRunRequest{
		Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: "research-agent", Goal: "Monitor source", Source: runtime.RunSourceSchedule,
		Context: map[string]interface{}{"initiativeId": initiative.ID, "sourceMonitorId": "monitor-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("setup is difficult"))
	ingested, err := runtime.NewSourceMonitorService(store, store, store, store).Ingest(ctx, runtime.IngestSourceObservationRequest{
		Scope: scope, InitiativeID: initiative.ID, MonitorID: "monitor-1", Cursor: "cursor-1", StableSourceID: "thread-1",
		SourceURI: "https://forum.example/thread/1", ContentDigest: "sha256:" + hex.EncodeToString(digest[:]), Summary: "Setup is difficult.", ObservedAt: time.Now().UTC(),
		RunID: sourceRun.ID, AgentID: "research-agent", SkillID: "source", SkillVersion: "1.0.0", Action: "observe", ActionCallID: "source-call-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "outreach", Version: "1.0.0", Name: "Outreach", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://forum.example"},
		Actions: map[string]skill.Action{"reply": {
			Name: "reply", Description: "Post reviewed reply", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal,
			SemanticArguments: map[string]string{"target": "targetUri", "body": "body"},
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
				"targetUri": map[string]interface{}{"type": "string"}, "body": map[string]interface{}{"type": "string"},
			}, "required": []interface{}{"targetUri", "body"}}, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencyRequired,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "outreach-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "research-agent",
		SkillID: "outreach", SkillVersion: "1.0.0", AllowedActions: []string{"reply"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}

	api := NewServer(store, zap.NewNop().Sugar())
	withoutDispatcher := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if withoutDispatcher.Code != http.StatusOK || !strings.Contains(withoutDispatcher.Body.String(), `"id":"outreach"`) || strings.Contains(withoutDispatcher.Body.String(), `"deliver"`) {
		t.Fatalf("capabilities without dispatcher = %d %s", withoutDispatcher.Code, withoutDispatcher.Body.String())
	}
	api.SetOutreachDeliveryDispatcher(func(ctx context.Context, request runtime.CreateAgentRunRequest) (*runtime.AgentRunCommandResult, error) {
		return runtime.NewRunCommandService(store).CreateAgentRun(ctx, request)
	})
	capabilities := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), `"deliver"`) {
		t.Fatalf("capabilities = %d %s", capabilities.Code, capabilities.Body.String())
	}
	disclosure := "Disclosure: I am an OpenSeal automated research agent."
	body := "Could you share which setup step was hardest? " + disclosure
	createBody := `{"scope":{"kind":"local","id":"research"},"initiativeId":"initiative-1","sourceObservationId":"` + ingested.Observation.ID + `","approvalPolicyRef":"human-review","identity":{"profileRef":"profile:research","displayName":"OpenSeal Research","affiliation":"OpenSeal","disclosure":"` + disclosure + `"},"message":{"id":"message-1","intent":"request_feedback","body":"` + body + `","capability":{"bindingId":"outreach-binding","bindingRevision":1,"skillId":"outreach","skillVersion":"1.0.0","action":"reply"}}}`
	createdResponse := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/initiatives/initiative-1/outreach", createBody, "draft-1")
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", createdResponse.Code, createdResponse.Body.String())
	}
	var thread runtime.OutreachThread
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	if thread.TargetURI != ingested.Observation.SourceURI || thread.Messages[0].Capability.Arguments["body"] != body || thread.Messages[0].Capability.Arguments["targetUri"] != ingested.Observation.SourceURI {
		t.Fatalf("thread = %#v", thread)
	}
	replayed := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/initiatives/initiative-1/outreach", createBody, "draft-1")
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), thread.ID) {
		t.Fatalf("replay = %d %s", replayed.Code, replayed.Body.String())
	}
	listed := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/initiatives/initiative-1/outreach?scopeKind=local&scopeId=research&limit=10", "", "")
	got := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/initiatives/initiative-1/outreach/"+thread.ID+"?scopeKind=local&scopeId=research", "", "")
	if listed.Code != http.StatusOK || got.Code != http.StatusOK || !strings.Contains(listed.Body.String(), thread.ID) || !strings.Contains(got.Body.String(), thread.ID) {
		t.Fatalf("listed = %d %s; got = %d %s", listed.Code, listed.Body.String(), got.Code, got.Body.String())
	}
	delivery := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/initiatives/initiative-1/outreach/"+thread.ID+"/messages/message-1/deliveries", `{"scope":{"kind":"local","id":"research"}}`, "delivery-1")
	if delivery.Code != http.StatusCreated {
		t.Fatalf("delivery = %d %s", delivery.Code, delivery.Body.String())
	}
	var run runtime.AgentRun
	if err := json.Unmarshal(delivery.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	invocation, _ := run.Context[runtime.OutreachInvocationContextKey].(map[string]interface{})
	if run.AssignedAgentID != "research-agent" || invocation["threadId"] != thread.ID || invocation["messageId"] != "message-1" {
		t.Fatalf("delivery run = %#v", run)
	}
	crossScope := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/initiatives/initiative-1/outreach/"+thread.ID+"?scopeKind=local&scopeId=foreign", "", "")
	if crossScope.Code != http.StatusNotFound {
		t.Fatalf("cross scope = %d %s", crossScope.Code, crossScope.Body.String())
	}
}
