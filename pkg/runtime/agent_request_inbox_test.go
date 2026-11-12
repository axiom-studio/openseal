package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type agentRequestInboxTeamStore struct {
	*sqlite.Store
	deployment *kernelteam.Deployment
	definition *kernelteam.Definition
}

func (s *agentRequestInboxTeamStore) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	if s.deployment == nil || s.deployment.Scope != scope || s.deployment.ID != id {
		return nil, kernelteam.ErrDeploymentNotFound
	}
	return s.deployment, nil
}

func (s *agentRequestInboxTeamStore) GetTeamDefinition(_ context.Context, id, version string) (*kernelteam.Definition, error) {
	if s.definition == nil || s.definition.ID != id || s.definition.Version != version {
		return nil, kernelteam.ErrDefinitionNotFound
	}
	return s.definition, nil
}

func createInboxRequest(t *testing.T, store models.AgentRequestInboxStore, policy models.AgentRequestAcceptancePolicy, recipient models.CollaborationParty) (*models.AgentRun, *models.AgentRequest) {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "inbox"}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "requester"}, AssignedAgentID: "requester",
		Goal: "Coordinate launch", Source: models.RunSourceManual, Priority: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := NewCollaborationService(store).CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "requester"},
		Recipient: recipient, SourceRunID: source.ID, Goal: "Review the launch evidence",
		Instructions:     "Check the evidence before accepting.",
		SharedContext:    map[string]interface{}{"evidenceRef": "artifact:launch"},
		AcceptancePolicy: policy, IdempotencyKey: "review-launch",
	})
	if err != nil {
		t.Fatal(err)
	}
	return source, created.Request
}

func TestAgentRequestInboxBoundsConversationProjectionCadence(t *testing.T) {
	reconciler, err := NewAgentRequestInboxReconciler(newTestKernelStore(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	if !reconciler.conversationProjectionDue(now) {
		t.Fatal("initial conversation projection was not due")
	}
	if reconciler.conversationProjectionDue(now.Add(agentRequestConversationProjectionInterval - time.Nanosecond)) {
		t.Fatal("conversation projection ignored its minimum interval")
	}
	if !reconciler.conversationProjectionDue(now.Add(agentRequestConversationProjectionInterval)) {
		t.Fatal("conversation projection did not become due")
	}
}

func TestAgentRequestInboxCreatesOneRestartSafeDecisionRun(t *testing.T) {
	store := &agentRequestInboxTeamStore{Store: newTestKernelStore(t)}
	source, request := createInboxRequest(t, store, models.AgentRequestAcceptanceRecipientReview, models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "recipient"})
	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	first, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	secondReconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondReconciler.Reconcile(t.Context(), request.Scope, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	if first.DecisionRunsCreated != 1 || second.DecisionRunsReused != 1 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	runs, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("decision runs = %#v, %v", runs, err)
	}
	run := runs[0]
	inbox, _ := run.Context[agentrun.AgentRequestInboxContextKey].(map[string]interface{})
	if run.Source != models.RunSourceRequestDecision || run.Owner != (models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "recipient"}) ||
		run.AssignedAgentID != "recipient" || fmt.Sprint(inbox["requestId"]) != request.ID ||
		run.Budget == nil || run.Budget.MaxTurns != 12 {
		t.Fatalf("decision run = %#v", run)
	}
	persisted, err := NewCollaborationService(store).GetAgentRequest(t.Context(), request.Scope, request.ID)
	if err != nil || persisted.Status != models.AgentRequestStatusPending {
		t.Fatalf("pending request = %#v, %v", persisted, err)
	}
}

func TestAgentRequestInboxReusesHistoricalDecisionRunAfterInputSchemaDrift(t *testing.T) {
	kernel, db := newTestSQLiteStore(t)
	store := &agentRequestInboxTeamStore{Store: kernel}
	source, request := createInboxRequest(t, store, models.AgentRequestAcceptanceRecipientReview, models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "recipient"})
	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	first, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient")
	if err != nil || first.DecisionRunsCreated != 1 {
		t.Fatalf("first reconcile = %#v, %v", first, err)
	}
	runs, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("decision runs = %#v, %v", runs, err)
	}

	// Simulate a decision Run authored by an older OpenSeal release. Its
	// idempotency fingerprint no longer matches the current enriched input,
	// but its authoritative request lineage remains unchanged.
	storetest.RewriteRowPayload(t, db, "agent_runs", request.Scope, runs[0].ID, func(persisted *models.AgentRun) {
		inbox := persisted.Context[agentrun.AgentRequestInboxContextKey].(map[string]interface{})
		delete(inbox, "clarificationQuestion")
		delete(inbox, "clarificationResponse")
		delete(inbox, "artifactRequirements")
	})

	second, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	if second.DecisionRunsReused != 1 || second.DecisionRunsCreated != 0 {
		t.Fatalf("historical decision reconcile = %#v", second)
	}
	runs, err = store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("decision runs after drift = %#v, %v", runs, err)
	}
}

func TestAgentRequestInboxTerminalDecisionFailureResolvesRequestAndWakesSource(t *testing.T) {
	store := &agentRequestInboxTeamStore{Store: newTestKernelStore(t)}
	source, request := createInboxRequest(t, store, models.AgentRequestAcceptanceRecipientReview, models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "recipient"})
	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	if result, reconcileErr := reconciler.Reconcile(t.Context(), request.Scope, "recipient"); reconcileErr != nil || result.DecisionRunsCreated != 1 {
		t.Fatalf("create decision Run = %#v, %v", result, reconcileErr)
	}
	runs, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("decision runs = %#v, %v", runs, err)
	}
	activity := runtimeactivity.NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(t.Context(), request.Scope, runs[0].ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: runs[0].Revision, Status: models.AgentRunStatusRunning,
		Summary: "Review started", Actor: models.ActivityActor{Type: "worker", ID: "recipient"},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := activity.TransitionRun(t.Context(), request.Scope, running.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: models.AgentRunStatusFailed, Error: "provider exhausted retries",
		Summary: "Review failed", Actor: models.ActivityActor{Type: "worker", ID: "recipient"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied, resolveErr := reconciler.ResolveDecisionRun(t.Context(), failed); resolveErr != nil || !applied {
		t.Fatalf("resolve failed decision = %t, %v", applied, resolveErr)
	}
	persisted, err := NewCollaborationService(store).GetAgentRequest(t.Context(), request.Scope, request.ID)
	if err != nil || persisted.Status != models.AgentRequestStatusFailed || persisted.ResolutionReason != "provider exhausted retries" {
		t.Fatalf("resolved request = %#v, %v", persisted, err)
	}
	resumed, err := store.GetAgentRun(t.Context(), request.Scope, source.ID)
	if err != nil || resumed.Status != models.AgentRunStatusQueued || resumed.WakeCondition != nil {
		t.Fatalf("resumed source = %#v, %v", resumed, err)
	}
	results, _ := resumed.Output["collaborationResults"].(map[string]interface{})
	result, _ := results[request.ID].(map[string]interface{})
	if result["status"] != string(models.AgentRequestStatusFailed) || result["reason"] != "provider exhausted retries" {
		t.Fatalf("source collaboration result = %#v", result)
	}
	events, err := store.ListActivity(t.Context(), models.ActivityFilter{Scope: request.Scope, RunID: source.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var failure *models.ActivityEvent
	for _, event := range events {
		if event.EventType == "collaboration.review_failed" {
			failure = event
			break
		}
	}
	if failure == nil || failure.CausationID != failed.ID || failure.Payload["decisionRunId"] != failed.ID {
		t.Fatalf("review failure event = %#v", failure)
	}
}

func TestAgentRequestInboxTerminalDecisionFailureConvergesAfterSourceAlreadyEnded(t *testing.T) {
	store := &agentRequestInboxTeamStore{Store: newTestKernelStore(t)}
	source, request := createInboxRequest(t, store, models.AgentRequestAcceptanceRecipientReview, models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "recipient"})
	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	if result, reconcileErr := reconciler.Reconcile(t.Context(), request.Scope, "recipient"); reconcileErr != nil || result.DecisionRunsCreated != 1 {
		t.Fatalf("create decision Run = %#v, %v", result, reconcileErr)
	}
	runs, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("decision runs = %#v, %v", runs, err)
	}
	activity := runtimeactivity.NewRunActivityService(store, store)
	currentSource, err := store.GetAgentRun(t.Context(), request.Scope, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	endedSource, _, err := activity.TransitionRun(t.Context(), request.Scope, currentSource.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: currentSource.Revision, Status: models.AgentRunStatusFailed, Error: "source independently exhausted retries",
		Summary: "Source ended", Actor: models.ActivityActor{Type: "worker", ID: "requester"},
	})
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := activity.TransitionRun(t.Context(), request.Scope, runs[0].ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: runs[0].Revision, Status: models.AgentRunStatusRunning,
		Summary: "Review started", Actor: models.ActivityActor{Type: "worker", ID: "recipient"},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := activity.TransitionRun(t.Context(), request.Scope, running.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: models.AgentRunStatusFailed, Error: "review provider unavailable",
		Summary: "Review failed", Actor: models.ActivityActor{Type: "worker", ID: "recipient"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied, resolveErr := reconciler.ResolveDecisionRun(t.Context(), failed); resolveErr != nil || !applied {
		t.Fatalf("resolve orphaned review = %t, %v", applied, resolveErr)
	}
	persisted, err := NewCollaborationService(store).GetAgentRequest(t.Context(), request.Scope, request.ID)
	if err != nil || persisted.Status != models.AgentRequestStatusFailed || persisted.ResolutionReason != "review provider unavailable" {
		t.Fatalf("resolved request = %#v, %v", persisted, err)
	}
	unchangedSource, err := store.GetAgentRun(t.Context(), request.Scope, source.ID)
	if err != nil || unchangedSource.Status != models.AgentRunStatusFailed || unchangedSource.Revision != endedSource.Revision ||
		unchangedSource.Error != endedSource.Error {
		t.Fatalf("terminal source changed = %#v, %v", unchangedSource, err)
	}
	second, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient")
	if err != nil || second.RequestsScanned != 0 {
		t.Fatalf("terminal request retried = %#v, %v", second, err)
	}
}

func TestAgentRequestInboxIrreconcilableIdempotencyCollisionFailsOnce(t *testing.T) {
	store := &agentRequestInboxTeamStore{Store: newTestKernelStore(t)}
	source, request := createInboxRequest(t, store, models.AgentRequestAcceptanceRecipientReview, models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "recipient"})
	otherSource, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: request.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "other"},
		AssignedAgentID: "other", Goal: "Unrelated work", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("agent-request-decision:%s:%d:%s", request.ID, request.Revision, "recipient")
	if _, err := agentrun.NewRunCommandService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: request.Scope, Kind: models.RunKindAgentWork, ParentRunID: otherSource.ID,
		Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "recipient"}, AssignedAgentID: "recipient",
		ConcurrencyKey: "unrelated", Goal: "Unrelated review", Source: models.RunSourceRequestDecision,
		Context: map[string]interface{}{agentrun.AgentRequestInboxContextKey: map[string]interface{}{
			"requestId": "another-request", "requestRevision": int64(1),
		}},
		IdempotencyKey: key, Actor: models.ActivityActor{Type: "system", ID: "test"},
		Visibility: models.ActivityVisibilityTeam,
	}); err != nil {
		t.Fatal(err)
	}

	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	first, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	if first.DecisionsApplied != 1 {
		t.Fatalf("collision reconcile = %#v", first)
	}
	persisted, err := NewCollaborationService(store).GetAgentRequest(t.Context(), request.Scope, request.ID)
	if err != nil || persisted.Status != models.AgentRequestStatusFailed {
		t.Fatalf("failed request = %#v, %v", persisted, err)
	}
	resumed, err := store.GetAgentRun(t.Context(), request.Scope, source.ID)
	if err != nil || resumed.Status != models.AgentRunStatusQueued || resumed.WakeCondition != nil {
		t.Fatalf("resumed source = %#v, %v", resumed, err)
	}
	second, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	if second.RequestsScanned != 0 || second.DecisionsApplied != 0 {
		t.Fatalf("terminal request retried = %#v", second)
	}
}

func TestAgentRequestInboxReconcilesEveryPaginatedRequest(t *testing.T) {
	store := &agentRequestInboxTeamStore{Store: newTestKernelStore(t)}
	scope := models.Scope{Kind: "tenant", ID: "paginated-inbox"}
	portfolio := portfolio.NewPortfolioService(store)
	collaboration := NewCollaborationService(store)
	const requestCount = 205
	for index := 0; index < requestCount; index++ {
		source, err := portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
			Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "requester"}, AssignedAgentID: "requester",
			Goal: fmt.Sprintf("Source %d", index), Source: models.RunSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = collaboration.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
			Scope: scope, Kind: models.AgentRequestKindRequest,
			Requester:   models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "requester"},
			Recipient:   models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "recipient"},
			SourceRunID: source.ID, Goal: fmt.Sprintf("Review %d", index),
			IdempotencyKey: fmt.Sprintf("paginated-review-%d", index),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reconciler.Reconcile(t.Context(), scope, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	if result.RequestsScanned != requestCount || result.DecisionRunsCreated != requestCount {
		t.Fatalf("paginated reconcile = %#v", result)
	}
	runs, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{
		Scope: scope, AssignedAgentID: "recipient", Statuses: []models.AgentRunStatus{models.AgentRunStatusQueued}, Limit: requestCount + 1,
	})
	if err != nil || len(runs) != requestCount {
		t.Fatalf("decision runs = %d, %v", len(runs), err)
	}
}

func TestAgentRequestInboxRecoversPreauthorizedDelegationWithoutReview(t *testing.T) {
	store := &agentRequestInboxTeamStore{Store: newTestKernelStore(t)}
	source, request := createInboxRequest(t, store, models.AgentRequestAcceptancePreauthorized, models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "recipient"})
	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := NewCollaborationService(store).GetAgentRequest(t.Context(), request.Scope, request.ID)
	if err != nil || persisted.Status != models.AgentRequestStatusAccepted || persisted.AssignedAgentID != "recipient" {
		t.Fatalf("accepted request = %#v, %v", persisted, err)
	}
	runs, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 || runs[0].Source != models.RunSourceRequest {
		t.Fatalf("delegated child runs = %#v, %v", runs, err)
	}
	if result.RequestsAccepted != 1 || result.DecisionRunsCreated != 0 {
		t.Fatalf("reconcile result = %#v", result)
	}
}

func TestAgentRequestInboxUsesDeterministicTeamMemberAndAcceptancePolicy(t *testing.T) {
	for _, requireAcceptance := range []bool{false, true} {
		t.Run(fmt.Sprintf("review-%t", requireAcceptance), func(t *testing.T) {
			scope := models.Scope{Kind: "tenant", ID: "inbox"}
			store := &agentRequestInboxTeamStore{
				Store: newTestKernelStore(t),
				deployment: &kernelteam.Deployment{
					ID: "marketing", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID},
					DefinitionID: "marketing", ActiveVersion: "1", Status: kernelteam.DeploymentActive, Revision: 1,
					Roster: []kernelteam.RosterAssignment{
						{ID: "z", AgentDeploymentID: "z-agent"},
						{ID: "a", AgentDeploymentID: "a-agent"},
					},
				},
				definition: &kernelteam.Definition{
					ID: "marketing", Version: "1",
					Delegation: kernelteam.DelegationPolicy{RequireAcceptance: requireAcceptance},
				},
			}
			source, request := createInboxRequest(t, store, models.AgentRequestAcceptanceRecipientReview, models.CollaborationParty{Type: models.OwnerTypeTeam, ID: "marketing"})
			reconciler, err := NewAgentRequestInboxReconciler(store)
			if err != nil {
				t.Fatal(err)
			}
			result, err := reconciler.Reconcile(t.Context(), request.Scope, "")
			if err != nil {
				t.Fatal(err)
			}
			runs, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: scope, ParentRunID: source.ID, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if requireAcceptance {
				if result.DecisionRunsCreated != 1 || len(runs) != 1 || runs[0].Source != models.RunSourceRequestDecision ||
					runs[0].AssignedAgentID != "a-agent" || runs[0].Owner != (models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "marketing"}) {
					t.Fatalf("review result=%#v runs=%#v", result, runs)
				}
			} else {
				persisted, getErr := NewCollaborationService(store).GetAgentRequest(t.Context(), scope, request.ID)
				if getErr != nil || result.RequestsAccepted != 1 || persisted.Status != models.AgentRequestStatusAccepted ||
					persisted.AssignedAgentID != "a-agent" || len(runs) != 1 || runs[0].Source != models.RunSourceRequest {
					t.Fatalf("auto acceptance result=%#v request=%#v runs=%#v error=%v", result, persisted, runs, getErr)
				}
			}
		})
	}
}

func TestParseAgentRequestDecisionOutputRejectsAmbiguousOrUnsafeShape(t *testing.T) {
	for _, output := range []map[string]interface{}{
		nil,
		{AgentRequestDecisionOutputKey: map[string]interface{}{"decision": "maybe", "message": "unsure"}},
		{AgentRequestDecisionOutputKey: map[string]interface{}{"decision": string(AgentRequestDecisionRequestClarification)}},
	} {
		if _, _, err := ParseAgentRequestDecisionOutput(output); err == nil {
			t.Fatalf("output unexpectedly accepted: %#v", output)
		}
	}
}
