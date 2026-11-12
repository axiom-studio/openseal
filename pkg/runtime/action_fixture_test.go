package runtime

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// proposalValidatorFunc adapts a function to ActionProposalValidator.
type proposalValidatorFunc func(context.Context, agentrun.ActionProposalValidationInput) (map[string]interface{}, error)

func (f proposalValidatorFunc) ValidateActionProposal(ctx context.Context, input agentrun.ActionProposalValidationInput) (map[string]interface{}, error) {
	return f(ctx, input)
}

// loopProposeRequest is req as the loop proposes tool call toolCallID of
// step-1: loop origin and execution and the per-call idempotency key.
func loopProposeRequest(req ProposeActionRequest, toolCallID string) ProposeActionRequest {
	req.Origin, req.Execution = models.ActionOriginLoop, models.ActionExecutionLoop
	if req.TurnID == "" {
		req.TurnID = "step-1"
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = "loop:" + req.TurnID + ":" + toolCallID
	}
	return req
}

// assertLoopRunUntouched checks a loop proposal left the Run as claimed:
// running, same lease, same revision, no wake condition.
func assertLoopRunUntouched(t *testing.T, store models.PortfolioStore, run *models.AgentRun) {
	t.Helper()
	current, err := store.GetAgentRun(context.Background(), run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != models.AgentRunStatusRunning || current.Revision != run.Revision || current.LeaseOwner != run.LeaseOwner || current.WakeCondition != nil {
		t.Fatalf("loop proposal changed the Run: %#v", current)
	}
}

func governedActionCatalog(t *testing.T) (*skill.Catalog, models.Scope) {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "one"}
	catalog := skill.NewCatalog()
	definition := &skill.Definition{
		ID: "release", Version: "1.0.0", Name: "Release", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://release.invalid"},
		Actions: map[string]skill.Action{"deploy": {
			Name: "deploy", Description: "Deploy a release", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelProduction,
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{
					"environment":     map[string]interface{}{"type": "string"},
					"credentialField": map[string]interface{}{"type": "string", "enum": []interface{}{"username", "password"}},
					"apiToken":        map[string]interface{}{"type": "string", "writeOnly": true},
					"nested":          map[string]interface{}{"type": "object", "properties": map[string]interface{}{"password": map[string]interface{}{"type": "string"}, "region": map[string]interface{}{"type": "string"}}},
				}, "required": []interface{}{"environment"},
			},
			Credentials: []skill.CredentialRequirement{{Name: "token", Kind: "api-token"}},
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 3}, Idempotency: skill.IdempotencyRequired,
		}},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "release-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", AllowedActions: []string{"deploy"}, MaximumRisk: skill.RiskLevelProduction,
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "api-token", ID: "release-secret"}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog, scope
}

func claimedActionRun(t *testing.T, store *sqlite.Store, scope models.Scope, now time.Time, worker string) *models.AgentRun {
	t.Helper()
	return claimedOwnedActionRun(t, store, scope, now, worker, models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "release-team"})
}

// claimedOwnedActionRun claims a run of release-agent under owner.
func claimedOwnedActionRun(t *testing.T, store *sqlite.Store, scope models.Scope, now time.Time, worker string, owner models.ObjectiveOwner) *models.AgentRun {
	t.Helper()
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: "release-agent", Goal: "deploy", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), models.AgentRunClaim{Scope: scope, WorkerID: worker, Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != run.ID {
		t.Fatalf("run was not claimed: %#v", claimed)
	}
	return claimed
}

type testCredentialLeaseSigner struct{ key []byte }

const testActionCredentialLeaseTransport = "reddit_search"

func (s testCredentialLeaseSigner) SignActionCredentialLease(_ context.Context, payload []byte) (models.ActionCredentialLeaseSignature, error) {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(payload)
	return models.ActionCredentialLeaseSignature{Algorithm: "hmac-sha256", KeyID: "test-key", Value: mac.Sum(nil)}, nil
}

func (s testCredentialLeaseSigner) VerifyActionCredentialLease(_ context.Context, payload []byte, signature models.ActionCredentialLeaseSignature) error {
	expected, _ := s.SignActionCredentialLease(context.Background(), payload)
	if signature.Algorithm != expected.Algorithm || signature.KeyID != expected.KeyID || !hmac.Equal(signature.Value, expected.Value) {
		return errors.New("invalid signature")
	}
	return nil
}

func actionCredentialLeaseFixture() (time.Time, *models.ActionCall, *models.AgentRun, map[string][]string) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	expires := now.Add(2 * time.Minute)
	scope := models.Scope{Kind: "tenant", ID: "tenant-7"}
	call := &models.ActionCall{
		ID: "action-1", Scope: scope, RunID: "run-1", TurnID: loopProposalStep, DeploymentID: "team-marketing", BindingID: "reddit-binding", BindingRevision: 3,
		Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop,
		SkillID: "reddit-research", SkillVersion: "1.2.0", Action: "search", Status: models.ActionCallStatusRunning,
		CredentialRefs: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "opaque://tenant-7/reddit"}},
		Attempt:        1, MaxAttempts: 3, LeaseOwner: "runtime-worker-1", LeaseExpiresAt: &expires, Revision: 4,
	}
	run := &models.AgentRun{ID: "run-1", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team-marketing"}, AssignedAgentID: "agent-researcher"}
	return now, call, run, map[string][]string{"reddit": {"api.token"}}
}

func forActionLifecycleStores(t *testing.T, test func(*testing.T, models.KernelStore)) {
	t.Helper()
	forActionLifecycleBackends(t, func(t *testing.T, store models.KernelStore, _ actionLifecycleBackend) { test(t, store) })
}

// actionLifecycleBackend is the database behind one lifecycle store as the
// test owns it: its own connection and table names for rows no store method
// writes, and reopen, which opens the store again on the same data as a
// restart would.
type actionLifecycleBackend struct {
	postgres bool
	db       *sql.DB
	table    func(name string) string
	reopen   func(*testing.T) models.KernelStore
}

// forActionLifecycleBackends runs test on SQLite, and on Postgres when
// OPENSEAL_TEST_POSTGRES_DSN is set.
func forActionLifecycleBackends(t *testing.T, test func(*testing.T, models.KernelStore, actionLifecycleBackend)) {
	t.Helper()
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (models.KernelStore, actionLifecycleBackend)
	}{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, actionLifecycleBackend) {
			path := filepath.Join(t.TempDir(), "action-lifecycle.db")
			store, err := sqlite.NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store, actionLifecycleBackend{db: openTestSQLiteDB(t, path), table: func(name string) string { return name }, reopen: func(t *testing.T) models.KernelStore {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := sqlite.NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reopened.Close() })
				return reopened
			}}
		}},
		{name: "postgres", open: func(t *testing.T) (models.KernelStore, actionLifecycleBackend) {
			pgSchema := storetest.NewPostgresSchema(t, "openseal_action_lifecycle_")
			dsn := pgSchema.DSN
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			store, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(pgSchema.Name))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
				_ = store.Close()
			})
			return store, actionLifecycleBackend{postgres: true, db: pgSchema.DB, table: pgSchema.Table, reopen: func(t *testing.T) models.KernelStore {
				return openPostgresStoreOn(t, pgSchema)
			}}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, backend := testCase.open(t)
			test(t, store, backend)
		})
	}
}

func getActionLifecycleRun(t *testing.T, store models.KernelStore, call *models.ActionCall) *models.AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(t.Context(), call.Scope, call.RunID)
	if err != nil || run == nil {
		t.Fatalf("get parent Run = %#v, %v", run, err)
	}
	return run
}

func transitionActionLifecycleRun(t *testing.T, store models.KernelStore, call *models.ActionCall, status models.AgentRunStatus, wake *models.WakeCondition, now time.Time) *models.AgentRun {
	t.Helper()
	run := getActionLifecycleRun(t, store, call)
	service := activity.NewRunActivityService(store, store, models.WithClock(func() time.Time { return now }))
	updated, _, err := service.TransitionRun(t.Context(), call.Scope, call.RunID, activity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: status, WakeCondition: wake,
		Actor: models.ActivityActor{Type: "user", ID: "alice"}, Summary: "Update the current workstream",
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func mutateActionLifecycleRun(t *testing.T, store models.KernelStore, call *models.ActionCall, now time.Time, mutate func(*models.AgentRun)) *models.AgentRun {
	t.Helper()
	run := getActionLifecycleRun(t, store, call)
	expected := run.Revision
	mutate(run)
	run.Revision++
	run.UpdatedAt = now
	event := actionLifecycleEvent(call, fmt.Sprintf("lifecycle-mutation-%d", run.Revision), "run.updated", now)
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), run, expected, event, nil); err != nil {
		t.Fatal(err)
	}
	return run
}

func assertActionLifecycleUnclaimable(t *testing.T, store models.KernelStore, call *models.ActionCall, now time.Time) {
	t.Helper()
	claimed, err := store.ClaimNextAction(t.Context(), models.ActionClaim{Scope: call.Scope, WorkerID: "later-worker", Now: now, LeaseDuration: time.Minute})
	if err != nil || claimed != nil {
		t.Fatalf("terminal action was eligible again: %#v, %v", claimed, err)
	}
}

func actionLifecycleEvent(call *models.ActionCall, id, eventType string, now time.Time) *models.ActivityEvent {
	return &models.ActivityEvent{
		ID: id, Scope: call.Scope, RunID: call.RunID, AgentID: call.DeploymentID,
		EventType: eventType, Severity: models.ActivitySeverityInfo, Summary: "Record action lifecycle state",
		Actor: models.ActivityActor{Type: "worker", ID: "lifecycle-worker"}, Visibility: models.ActivityVisibilityScope,
		CreatedAt: now, Payload: map[string]interface{}{"actionCallId": call.ID, "status": call.Status},
	}
}

type actionLifecycleCountingCatalog struct {
	agentrun.ActionExecutionCatalog
	calls *int
}

func (c *actionLifecycleCountingCatalog) Resolve(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string, bindings ...skill.BindingReference) (*skill.BoundAction, error) {
	(*c.calls)++
	return c.ActionExecutionCatalog.Resolve(ctx, scope, deploymentID, skillID, version, action, bindings...)
}

// loopActionSpec describes one loop tool call proposeLoopAction stores.
type loopActionSpec struct {
	id, skillID, version, action string
	arguments                    map[string]interface{}
	externalOperation            *activity.ExternalOperationIdentity
}

// proposeLoopAction stores a ready loop call of step-1 for the Run's lease
// owner workerID, as ActionCoordinator.Propose does for a governed loop tool
// call: the Run is unchanged except for the action reservation of a budgeted
// Run.
// loopActionCredentialRefs are the binding's references for the credentials
// the bound action requires, as a proposal records them; nil when none.
func loopActionCredentialRefs(bound *skill.BoundAction) map[string]skill.CredentialReference {
	refs := make(map[string]skill.CredentialReference)
	for _, requirement := range bound.Action.Credentials {
		if reference, ok := bound.Binding.Credentials[requirement.Name]; ok {
			refs[requirement.Name] = reference
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

func proposeLoopAction(t *testing.T, store models.KernelStore, catalog agentrun.ActionExecutionCatalog, run *models.AgentRun, workerID string, now time.Time, spec loopActionSpec) *models.ActionProposalResult {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: current.Scope.Kind, ID: current.Scope.ID}, current.AssignedAgentID, spec.skillID, spec.version, spec.action)
	if err != nil {
		t.Fatal(err)
	}
	call := &models.ActionCall{
		ID: spec.id, Scope: current.Scope, RunID: current.ID, TurnID: "step-1", DeploymentID: current.AssignedAgentID,
		BindingID: bound.Binding.ID, BindingRevision: bound.Binding.Revision,
		SkillID: spec.skillID, SkillVersion: spec.version, Action: spec.action, Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop,
		Status: models.ActionCallStatusReady, Risk: bound.Action.Risk, SideEffect: bound.Action.SideEffect, Review: bound.Action.Review,
		Arguments: maps.Clone(spec.arguments), CredentialRefs: loopActionCredentialRefs(bound), IdempotencyKey: "tool-call:step-1:" + spec.id,
		MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if spec.externalOperation != nil {
		call.ExternalOperationDigest, err = activity.ComputeExternalOperationDigest(current.Scope, current.Owner, current.ObjectiveID, spec.externalOperation, call.IdempotencyKey)
		if err != nil {
			t.Fatal(err)
		}
	}
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	call.SemanticDigest = models.ComputeActionSemanticDigest(call)
	record := models.ActionProposalRecord{
		Call: call, ExpectedRunRevision: current.Revision, Lease: &models.AgentRunLeaseGuard{WorkerID: workerID, Now: now},
		Event: &models.ActivityEvent{
			ID: "proposed-" + spec.id, Scope: current.Scope, EventType: "action.proposed", Severity: models.ActivitySeverityInfo, AgentID: current.AssignedAgentID,
			RunID: current.ID, TurnID: "step-1", Actor: models.ActivityActor{Type: "worker", ID: workerID}, Summary: "Proposed " + spec.action,
			Visibility: models.ActivityVisibilityScope, CreatedAt: now, Payload: map[string]interface{}{"actionCallId": spec.id},
		},
	}
	if current.Budget != nil {
		updated := models.CloneAgentRun(current)
		if err := models.ReserveRunBudget(updated, models.BudgetReservation{ID: agentrun.ActionBudgetReservationID(call.ID), Usage: models.BudgetUsage{Actions: 1}, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		updated.Revision++
		updated.UpdatedAt = now
		record.Run = updated
	}
	result, err := store.CreateActionProposal(ctx, record)
	if err != nil || result == nil || !result.Created {
		t.Fatalf("propose loop call %s = %#v, %v", spec.id, result, err)
	}
	return result
}

// loopWorkerCatalog binds release-agent to a Skill with a production write
// (deploy) and a read (status), both using one credential.
func loopWorkerCatalog(t *testing.T, scope models.Scope) *skill.Catalog {
	t.Helper()
	ctx := context.Background()
	schema := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{"environment": map[string]interface{}{"type": "string"}}, "required": []interface{}{"environment"},
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "release", Version: "1.0.0", Name: "Release", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://release.invalid"},
		Actions: map[string]skill.Action{
			"deploy": {Name: "deploy", Description: "Deploy a release", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelProduction, InputSchema: schema,
				Credentials: []skill.CredentialRequirement{{Name: "token", Kind: "api-token"}}, Idempotency: skill.IdempotencyRequired},
			"status": {Name: "status", Description: "Read release status", SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, InputSchema: schema,
				Credentials: []skill.CredentialRequirement{{Name: "token", Kind: "api-token"}}, Idempotency: skill.IdempotencyNone},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "release-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", AllowedActions: []string{"deploy", "status"}, MaximumRisk: skill.RiskLevelProduction,
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "api-token", ID: "release-secret"}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}
