package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type agentRequestInboxTeamStore struct {
	*MemoryStore
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

func createInboxRequest(t *testing.T, store AgentRequestInboxStore, policy AgentRequestAcceptancePolicy, recipient CollaborationParty) (*AgentRun, *AgentRequest) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "inbox"}
	source, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "requester"}, AssignedAgentID: "requester",
		Goal: "Coordinate launch", Source: RunSourceManual, Priority: 3,
		Context: map[string]interface{}{"initiativeId": "initiative-launch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := NewCollaborationService(store).CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "requester"},
		Recipient: recipient, SourceRunID: source.ID, Goal: "Review the launch evidence",
		Instructions: "Check the evidence before accepting.", SemanticRole: "reviewer",
		SharedContext:    map[string]interface{}{"evidenceRef": "artifact:launch"},
		AcceptancePolicy: policy, IdempotencyKey: "review-launch",
	})
	if err != nil {
		t.Fatal(err)
	}
	return source, created.Request
}

func TestAgentRequestInboxCreatesOneRestartSafeDecisionRun(t *testing.T) {
	store := &agentRequestInboxTeamStore{MemoryStore: NewMemoryStore()}
	source, request := createInboxRequest(t, store, AgentRequestAcceptanceRecipientReview, CollaborationParty{Type: OwnerTypeAgent, ID: "recipient"})
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
	runs, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("decision runs = %#v, %v", runs, err)
	}
	run := runs[0]
	inbox, _ := run.Context[AgentRequestInboxContextKey].(map[string]interface{})
	if run.Source != RunSourceRequestDecision || run.Owner != (ObjectiveOwner{Type: OwnerTypeAgent, ID: "recipient"}) ||
		run.AssignedAgentID != "recipient" || fmt.Sprint(inbox["requestId"]) != request.ID ||
		run.Budget == nil || run.Budget.MaxTurns != 3 || run.Context["initiativeId"] != "initiative-launch" {
		t.Fatalf("decision run = %#v", run)
	}
	persisted, err := NewCollaborationService(store).GetAgentRequest(t.Context(), request.Scope, request.ID)
	if err != nil || persisted.Status != AgentRequestStatusPending {
		t.Fatalf("pending request = %#v, %v", persisted, err)
	}
}

func TestAgentRequestInboxReusesHistoricalDecisionRunAfterInputSchemaDrift(t *testing.T) {
	store := &agentRequestInboxTeamStore{MemoryStore: NewMemoryStore()}
	source, request := createInboxRequest(t, store, AgentRequestAcceptanceRecipientReview, CollaborationParty{Type: OwnerTypeAgent, ID: "recipient"})
	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	first, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient")
	if err != nil || first.DecisionRunsCreated != 1 {
		t.Fatalf("first reconcile = %#v, %v", first, err)
	}
	runs, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("decision runs = %#v, %v", runs, err)
	}

	// Simulate a decision Run authored by an older OpenSeal release. Its
	// idempotency fingerprint no longer matches the current enriched input,
	// but its authoritative request lineage remains unchanged.
	store.mu.Lock()
	persisted := store.agentRuns[portfolioKey(request.Scope, runs[0].ID)]
	inbox := persisted.Context[AgentRequestInboxContextKey].(map[string]interface{})
	delete(inbox, "clarificationQuestion")
	delete(inbox, "clarificationResponse")
	delete(inbox, "artifactRequirements")
	store.mu.Unlock()

	second, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	if second.DecisionRunsReused != 1 || second.DecisionRunsCreated != 0 {
		t.Fatalf("historical decision reconcile = %#v", second)
	}
	runs, err = store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("decision runs after drift = %#v, %v", runs, err)
	}
}

func TestAgentRequestInboxTerminalDecisionFailureResolvesRequestAndWakesSource(t *testing.T) {
	store := &agentRequestInboxTeamStore{MemoryStore: NewMemoryStore()}
	source, request := createInboxRequest(t, store, AgentRequestAcceptanceRecipientReview, CollaborationParty{Type: OwnerTypeAgent, ID: "recipient"})
	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	if result, reconcileErr := reconciler.Reconcile(t.Context(), request.Scope, "recipient"); reconcileErr != nil || result.DecisionRunsCreated != 1 {
		t.Fatalf("create decision Run = %#v, %v", result, reconcileErr)
	}
	runs, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("decision runs = %#v, %v", runs, err)
	}
	activity := NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(t.Context(), request.Scope, runs[0].ID, RunTransitionRequest{
		ExpectedRevision: runs[0].Revision, Status: AgentRunStatusRunning,
		Summary: "Review started", Actor: ActivityActor{Type: "worker", ID: "recipient"},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := activity.TransitionRun(t.Context(), request.Scope, running.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusFailed, Error: "provider exhausted retries",
		Summary: "Review failed", Actor: ActivityActor{Type: "worker", ID: "recipient"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied, resolveErr := reconciler.ResolveDecisionRun(t.Context(), failed); resolveErr != nil || !applied {
		t.Fatalf("resolve failed decision = %t, %v", applied, resolveErr)
	}
	persisted, err := NewCollaborationService(store).GetAgentRequest(t.Context(), request.Scope, request.ID)
	if err != nil || persisted.Status != AgentRequestStatusFailed || persisted.ResolutionReason != "provider exhausted retries" {
		t.Fatalf("resolved request = %#v, %v", persisted, err)
	}
	resumed, err := store.GetAgentRun(t.Context(), request.Scope, source.ID)
	if err != nil || resumed.Status != AgentRunStatusQueued || resumed.WakeCondition != nil {
		t.Fatalf("resumed source = %#v, %v", resumed, err)
	}
	results, _ := resumed.Output["collaborationResults"].(map[string]interface{})
	result, _ := results[request.ID].(map[string]interface{})
	if result["status"] != string(AgentRequestStatusFailed) || result["reason"] != "provider exhausted retries" {
		t.Fatalf("source collaboration result = %#v", result)
	}
	events, err := store.ListActivity(t.Context(), ActivityFilter{Scope: request.Scope, RunID: source.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var failure *ActivityEvent
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

func TestAgentRequestInboxIrreconcilableIdempotencyCollisionFailsOnce(t *testing.T) {
	store := &agentRequestInboxTeamStore{MemoryStore: NewMemoryStore()}
	source, request := createInboxRequest(t, store, AgentRequestAcceptanceRecipientReview, CollaborationParty{Type: OwnerTypeAgent, ID: "recipient"})
	otherSource, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: request.Scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "other"},
		AssignedAgentID: "other", Goal: "Unrelated work", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("agent-request-decision:%s:%d:%s", request.ID, request.Revision, "recipient")
	if _, err := NewRunCommandService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: request.Scope, Kind: RunKindAgentWork, ParentRunID: otherSource.ID,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "recipient"}, AssignedAgentID: "recipient",
		ConcurrencyKey: "unrelated", Goal: "Unrelated review", Source: RunSourceRequestDecision,
		Context: map[string]interface{}{AgentRequestInboxContextKey: map[string]interface{}{
			"requestId": "another-request", "requestRevision": int64(1),
		}},
		IdempotencyKey: key, Actor: ActivityActor{Type: "system", ID: "test"},
		Visibility: ActivityVisibilityTeam,
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
	if err != nil || persisted.Status != AgentRequestStatusFailed {
		t.Fatalf("failed request = %#v, %v", persisted, err)
	}
	resumed, err := store.GetAgentRun(t.Context(), request.Scope, source.ID)
	if err != nil || resumed.Status != AgentRunStatusQueued || resumed.WakeCondition != nil {
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
	store := &agentRequestInboxTeamStore{MemoryStore: NewMemoryStore()}
	scope := Scope{Kind: "tenant", ID: "paginated-inbox"}
	portfolio := NewPortfolioService(store)
	collaboration := NewCollaborationService(store)
	const requestCount = 205
	for index := 0; index < requestCount; index++ {
		source, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{
			Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "requester"}, AssignedAgentID: "requester",
			Goal: fmt.Sprintf("Source %d", index), Source: RunSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = collaboration.CreateAgentRequest(t.Context(), CreateAgentRequestRequest{
			Scope: scope, Kind: AgentRequestKindRequest,
			Requester:   CollaborationParty{Type: OwnerTypeAgent, ID: "requester"},
			Recipient:   CollaborationParty{Type: OwnerTypeAgent, ID: "recipient"},
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
	runs, err := store.ListAgentRuns(t.Context(), AgentRunFilter{
		Scope: scope, AssignedAgentID: "recipient", Statuses: []AgentRunStatus{AgentRunStatusQueued}, Limit: requestCount + 1,
	})
	if err != nil || len(runs) != requestCount {
		t.Fatalf("decision runs = %d, %v", len(runs), err)
	}
}

func TestAgentRequestInboxDecisionSurvivesSQLiteRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-request-inbox.db")
	firstStore, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	source, request := createInboxRequest(t, firstStore, AgentRequestAcceptanceRecipientReview, CollaborationParty{Type: OwnerTypeAgent, ID: "recipient"})
	firstReconciler, err := NewAgentRequestInboxReconciler(firstStore)
	if err != nil {
		t.Fatal(err)
	}
	if result, reconcileErr := firstReconciler.Reconcile(t.Context(), request.Scope, "recipient"); reconcileErr != nil || result.DecisionRunsCreated != 1 {
		t.Fatalf("first reconcile = %#v, %v", result, reconcileErr)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	secondReconciler, err := NewAgentRequestInboxReconciler(restarted)
	if err != nil {
		t.Fatal(err)
	}
	if result, reconcileErr := secondReconciler.Reconcile(t.Context(), request.Scope, "recipient"); reconcileErr != nil || result.DecisionRunsReused != 1 {
		t.Fatalf("restart reconcile = %#v, %v", result, reconcileErr)
	}
	runs, err := restarted.ListAgentRuns(t.Context(), AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 || runs[0].Source != RunSourceRequestDecision {
		t.Fatalf("restored decision run = %#v, %v", runs, err)
	}
	resolver := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		switch run.Source {
		case RunSourceRequestDecision:
			return &TurnRunnerBinding{DefinitionID: "recipient", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{
					NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Accepted after restart",
					RunOutput: map[string]interface{}{AgentRequestDecisionOutputKey: map[string]interface{}{
						"decision": string(AgentRequestDecisionAccept), "message": "The request is clear.",
					}},
				}, nil
			})}, nil
		case RunSourceRequest:
			return &TurnRunnerBinding{DefinitionID: "recipient", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Work completed after restart", RunOutput: map[string]interface{}{"result": "done"}}, nil
			})}, nil
		default:
			return nil, fmt.Errorf("unexpected claimed run source %s", run.Source)
		}
	})
	pool, err := NewAgentRunWorkerPool(restarted, resolver, nil, AgentRunWorkerConfig{
		Scope: request.Scope, Kind: RunKindAgentWork, AssignedAgentID: "recipient",
		Concurrency: 1, PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)
	defer func() {
		cancel()
		pool.Stop()
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := NewCollaborationService(restarted).GetAgentRequest(t.Context(), request.Scope, request.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current.Status == AgentRequestStatusCompleted {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	current, _ := NewCollaborationService(restarted).GetAgentRequest(t.Context(), request.Scope, request.ID)
	t.Fatalf("request did not complete after restart: %#v", current)
}

func TestAgentRequestInboxRecoversPreauthorizedDelegationWithoutReview(t *testing.T) {
	store := &agentRequestInboxTeamStore{MemoryStore: NewMemoryStore()}
	source, request := createInboxRequest(t, store, AgentRequestAcceptancePreauthorized, CollaborationParty{Type: OwnerTypeAgent, ID: "recipient"})
	reconciler, err := NewAgentRequestInboxReconciler(store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reconciler.Reconcile(t.Context(), request.Scope, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := NewCollaborationService(store).GetAgentRequest(t.Context(), request.Scope, request.ID)
	if err != nil || persisted.Status != AgentRequestStatusAccepted || persisted.AssignedAgentID != "recipient" {
		t.Fatalf("accepted request = %#v, %v", persisted, err)
	}
	runs, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: request.Scope, ParentRunID: source.ID, Limit: 10})
	if err != nil || len(runs) != 1 || runs[0].Source != RunSourceRequest {
		t.Fatalf("delegated child runs = %#v, %v", runs, err)
	}
	if result.RequestsAccepted != 1 || result.DecisionRunsCreated != 0 {
		t.Fatalf("reconcile result = %#v", result)
	}
}

func TestAgentRequestInboxAppliesAutonomousRecipientDecisions(t *testing.T) {
	tests := []struct {
		name       string
		decision   AgentRequestDecision
		message    string
		wantStatus AgentRequestStatus
		wantEvent  string
		wantSource AgentRunStatus
	}{
		{name: "accept", decision: AgentRequestDecisionAccept, message: "This is relevant and clear.", wantStatus: AgentRequestStatusCompleted, wantEvent: "collaboration.accepted", wantSource: AgentRunStatusQueued},
		{name: "reject", decision: AgentRequestDecisionReject, message: "This is outside my role.", wantStatus: AgentRequestStatusRejected, wantEvent: "collaboration.rejected", wantSource: AgentRunStatusQueued},
		{name: "clarify", decision: AgentRequestDecisionRequestClarification, message: "Which customer segment should I prioritize?", wantStatus: AgentRequestStatusClarificationRequested, wantEvent: "collaboration.clarification_requested", wantSource: AgentRunStatusQueued},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &agentRequestInboxTeamStore{MemoryStore: NewMemoryStore()}
			source, request := createInboxRequest(t, store, AgentRequestAcceptanceRecipientReview, CollaborationParty{Type: OwnerTypeAgent, ID: "recipient"})
			resolver := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
				switch run.Source {
				case RunSourceRequestDecision:
					return &TurnRunnerBinding{DefinitionID: "recipient", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
						return &TurnOutcome{
							NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Request evaluated",
							RunOutput: map[string]interface{}{AgentRequestDecisionOutputKey: map[string]interface{}{
								"decision": string(tc.decision), "message": tc.message,
							}},
						}, nil
					})}, nil
				case RunSourceRequest:
					return &TurnRunnerBinding{DefinitionID: "recipient", DefinitionVersion: "1", Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
						return &TurnOutcome{
							NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Requested work complete",
							RunOutput: map[string]interface{}{"summary": "Reviewed launch evidence", "finding": "ready"},
						}, nil
					})}, nil
				default:
					return nil, fmt.Errorf("unexpected claimed run source %s", run.Source)
				}
			})
			pool, err := NewAgentRunWorkerPool(store, resolver, nil, AgentRunWorkerConfig{
				Scope: request.Scope, Kind: RunKindAgentWork, AssignedAgentID: "recipient",
				Concurrency: 1, MaxTurnsPerClaim: 1, PollInterval: 5 * time.Millisecond,
				LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			pool.Start(ctx)
			defer func() {
				cancel()
				pool.Stop()
			}()
			deadline := time.Now().Add(3 * time.Second)
			var current *AgentRequest
			for time.Now().Before(deadline) {
				current, err = NewCollaborationService(store).GetAgentRequest(t.Context(), request.Scope, request.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.Status == tc.wantStatus {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if current == nil || current.Status != tc.wantStatus {
				t.Fatalf("request = %#v", current)
			}
			resumed, getErr := store.GetAgentRun(t.Context(), request.Scope, source.ID)
			if getErr != nil || resumed.Status != tc.wantSource || resumed.WakeCondition != nil {
				t.Fatalf("source after %s = %#v, %v", tc.decision, resumed, getErr)
			}
			results, _ := resumed.Output["collaborationResults"].(map[string]interface{})
			result, _ := results[request.ID].(map[string]interface{})
			if fmt.Sprint(result["status"]) != string(tc.wantStatus) {
				t.Fatalf("source collaboration result = %#v", result)
			}
			events, err := store.ListActivity(t.Context(), ActivityFilter{Scope: request.Scope, RunID: current.SourceRunID, Limit: 30})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events {
				if event.EventType != tc.wantEvent {
					continue
				}
				found = true
				if event.Actor != (ActivityActor{Type: "agent", ID: "recipient"}) ||
					fmt.Sprint(event.Payload["decisionRunId"]) == "" || event.CausationID != fmt.Sprint(event.Payload["decisionRunId"]) {
					t.Fatalf("decision event = %#v", event)
				}
			}
			if !found {
				t.Fatalf("decision event %s missing: %#v", tc.wantEvent, events)
			}
		})
	}
}

func TestAgentRequestInboxUsesDeterministicTeamRoleAndAcceptancePolicy(t *testing.T) {
	for _, requireAcceptance := range []bool{false, true} {
		t.Run(fmt.Sprintf("review-%t", requireAcceptance), func(t *testing.T) {
			scope := Scope{Kind: "tenant", ID: "inbox"}
			store := &agentRequestInboxTeamStore{
				MemoryStore: NewMemoryStore(),
				deployment: &kernelteam.Deployment{
					ID: "marketing", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID},
					DefinitionID: "marketing", ActiveVersion: "1", Status: kernelteam.DeploymentActive, Revision: 1,
					Roster: []kernelteam.RosterAssignment{
						{ID: "z", RoleID: "reviewer", AgentDeploymentID: "z-agent"},
						{ID: "a", RoleID: "reviewer", AgentDeploymentID: "a-agent"},
					},
				},
				definition: &kernelteam.Definition{
					ID: "marketing", Version: "1",
					Delegation: kernelteam.DelegationPolicy{RequireAcceptance: requireAcceptance},
				},
			}
			source, request := createInboxRequest(t, store, AgentRequestAcceptanceRecipientReview, CollaborationParty{Type: OwnerTypeTeam, ID: "marketing"})
			reconciler, err := NewAgentRequestInboxReconciler(store)
			if err != nil {
				t.Fatal(err)
			}
			result, err := reconciler.Reconcile(t.Context(), request.Scope, "")
			if err != nil {
				t.Fatal(err)
			}
			runs, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope, ParentRunID: source.ID, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if requireAcceptance {
				if result.DecisionRunsCreated != 1 || len(runs) != 1 || runs[0].Source != RunSourceRequestDecision ||
					runs[0].AssignedAgentID != "a-agent" || runs[0].Owner != (ObjectiveOwner{Type: OwnerTypeTeam, ID: "marketing"}) {
					t.Fatalf("review result=%#v runs=%#v", result, runs)
				}
			} else {
				persisted, getErr := NewCollaborationService(store).GetAgentRequest(t.Context(), scope, request.ID)
				if getErr != nil || result.RequestsAccepted != 1 || persisted.Status != AgentRequestStatusAccepted ||
					persisted.AssignedAgentID != "a-agent" || len(runs) != 1 || runs[0].Source != RunSourceRequest {
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
		if _, _, err := parseAgentRequestDecisionOutput(output); err == nil {
			t.Fatalf("output unexpectedly accepted: %#v", output)
		}
	}
}

func TestAgentRequestDecisionTurnFailsClosedOnWorkOrMalformedDecision(t *testing.T) {
	tests := []struct {
		name    string
		outcome *TurnOutcome
	}{
		{
			name: "delegation",
			outcome: &TurnOutcome{
				NextRunStatus: AgentRunStatusRunning,
				ProposedDelegation: &TurnDelegationProposal{
					StepID: "escape", AssignedAgentID: "other", Goal: "Do the requested work",
				},
			},
		},
		{
			name: "malformed completion",
			outcome: &TurnOutcome{
				NextRunStatus: AgentRunStatusCompleted,
				RunOutput:     map[string]interface{}{"summary": "No structured decision"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &agentRequestDecisionTurnRunner{inner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return tc.outcome, nil
			})}
			if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{
				Run: &AgentRun{ID: "decision"}, Turn: &AgentTurn{ID: "turn"},
			}); err == nil {
				t.Fatalf("outcome unexpectedly accepted: %#v", tc.outcome)
			}
		})
	}
}
