package action_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

// copied from action_fixture_test.go
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

// copied from action_fixture_test.go
// loopProposeRequest is req as the loop proposes tool call toolCallID of
// step-1: loop origin and execution and the per-call idempotency key.
func loopProposeRequest(req action.ProposeActionRequest, toolCallID string) action.ProposeActionRequest {
	req.Origin, req.Execution = models.ActionOriginLoop, models.ActionExecutionLoop
	if req.TurnID == "" {
		req.TurnID = "step-1"
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = "loop:" + req.TurnID + ":" + toolCallID
	}
	return req
}

// copied from action_loop_store_test.go
// runLoopStoreScenario runs scenario on SQLite, and on Postgres
// when OPENSEAL_TEST_POSTGRES_DSN is set (Postgres-only checks live in
// action_loop_postgres_integration_test.go).
func runLoopStoreScenario(t *testing.T, scenario func(*testing.T, models.KernelStore)) {
	t.Helper()
	for _, backend := range nestedActionStores(t) {
		t.Run(backend.name, func(t *testing.T) { scenario(t, backend.open(t)) })
	}
}

// copied from action_fixture_test.go
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

// copied from kernel_store_test.go
// newTestKernelStore returns an empty in-memory kernel store that is closed
// when the test ends. runtimetest.NewKernelStore, skillbindingtest.NewStore
// and the openers in the store/sqlite, callback, skillbinding and action
// tests are its copies for other packages (importing runtimetest here would
// be a cycle); change them together.
func newTestKernelStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// copied from action_fixture_test.go
// proposalValidatorFunc adapts a function to ActionProposalValidator.
type proposalValidatorFunc func(context.Context, agentrun.ActionProposalValidationInput) (map[string]interface{}, error)

// copied from action_fixture_test.go
func (f proposalValidatorFunc) ValidateActionProposal(ctx context.Context, input agentrun.ActionProposalValidationInput) (map[string]interface{}, error) {
	return f(ctx, input)
}

// copied from connect_account_test.go
// personActionCatalog binds an OAuth action of release-agent whose google
// slot is per_person.
func personActionCatalog(t *testing.T) (*skill.Catalog, models.Scope) {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "one"}
	catalog := skill.NewCatalog()
	definition := &skill.Definition{
		ID: "mail", Version: "1.0.0", Name: "Mail", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://mail.invalid"},
		Actions: map[string]skill.Action{"send": {
			Name: "send", Description: "Send a message", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelWrite,
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{"to": map[string]interface{}{"type": "string"}}, "required": []interface{}{"to"}},
			Credentials: []skill.CredentialRequirement{{Name: "google", Kind: "oauth2", OAuth2: &skill.OAuth2Requirement{Provider: "google", Subject: "user", Scopes: []string{"mail.send"}}}},
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 3}, Idempotency: skill.IdempotencySupported,
		}},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "mail-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-agent",
		SkillID: "mail", SkillVersion: "1.0.0", AllowedActions: []string{"send"}, MaximumRisk: skill.RiskLevelWrite,
		Credentials: map[string]skill.CredentialReference{"google": {Kind: "oauth2", ID: skill.PersonCredentialReferenceID}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog, scope
}

// copied from connect_account_test.go
// claimedPersonChatRun claims a 1:1 chat run of release-agent acting for
// actingUser.
func claimedPersonChatRun(t *testing.T, store *sqlite.Store, scope models.Scope, now time.Time, actingUser string) *models.AgentRun {
	t.Helper()
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent", Goal: "send mail",
		Source: models.RunSourceObjective, Kind: models.RunKindConversation, ActingUser: actingUser,
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: "conversation-1", models.ConversationRunTriggerIDContextKey: "message-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), models.AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return claimed
}

// copied from connect_account_test.go
const connectAccountTestUser = "7"

// copied from action_fixture_test.go
func claimedActionRun(t *testing.T, store *sqlite.Store, scope models.Scope, now time.Time, worker string) *models.AgentRun {
	t.Helper()
	return claimedOwnedActionRun(t, store, scope, now, worker, models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "release-team"})
}

// copied from action_fixture_test.go
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

// copied from action_fixture_test.go
const testActionCredentialLeaseTransport = "reddit_search"

// copied from action_fixture_test.go
type testCredentialLeaseSigner struct{ key []byte }

// copied from action_fixture_test.go
func (s testCredentialLeaseSigner) SignActionCredentialLease(_ context.Context, payload []byte) (models.ActionCredentialLeaseSignature, error) {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(payload)
	return models.ActionCredentialLeaseSignature{Algorithm: "hmac-sha256", KeyID: "test-key", Value: mac.Sum(nil)}, nil
}

// copied from action_fixture_test.go
func (s testCredentialLeaseSigner) VerifyActionCredentialLease(_ context.Context, payload []byte, signature models.ActionCredentialLeaseSignature) error {
	expected, _ := s.SignActionCredentialLease(context.Background(), payload)
	if signature.Algorithm != expected.Algorithm || signature.KeyID != expected.KeyID || !hmac.Equal(signature.Value, expected.Value) {
		return errors.New("invalid signature")
	}
	return nil
}

// copied from action_fixture_test.go
func forActionLifecycleStores(t *testing.T, test func(*testing.T, models.KernelStore)) {
	t.Helper()
	forActionLifecycleBackends(t, func(t *testing.T, store models.KernelStore, _ actionLifecycleBackend) { test(t, store) })
}

// copied from action_fixture_test.go
func getActionLifecycleRun(t *testing.T, store models.KernelStore, call *models.ActionCall) *models.AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(t.Context(), call.Scope, call.RunID)
	if err != nil || run == nil {
		t.Fatalf("get parent Run = %#v, %v", run, err)
	}
	return run
}

// copied from action_fixture_test.go
func actionLifecycleEvent(call *models.ActionCall, id, eventType string, now time.Time) *models.ActivityEvent {
	return &models.ActivityEvent{
		ID: id, Scope: call.Scope, RunID: call.RunID, AgentID: call.DeploymentID,
		EventType: eventType, Severity: models.ActivitySeverityInfo, Summary: "Record action lifecycle state",
		Actor: models.ActivityActor{Type: "worker", ID: "lifecycle-worker"}, Visibility: models.ActivityVisibilityScope,
		CreatedAt: now, Payload: map[string]interface{}{"actionCallId": call.ID, "status": call.Status},
	}
}

// copied from action_fixture_test.go
func assertActionLifecycleUnclaimable(t *testing.T, store models.KernelStore, call *models.ActionCall, now time.Time) {
	t.Helper()
	claimed, err := store.ClaimNextAction(t.Context(), models.ActionClaim{Scope: call.Scope, WorkerID: "later-worker", Now: now, LeaseDuration: time.Minute})
	if err != nil || claimed != nil {
		t.Fatalf("terminal action was eligible again: %#v, %v", claimed, err)
	}
}

// copied from action_fixture_test.go
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

// copied from action_fixture_test.go
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

// copied from action_fixture_test.go
// loopActionSpec describes one loop tool call proposeLoopAction stores.
type loopActionSpec struct {
	id, skillID, version, action string
	arguments                    map[string]interface{}
	externalOperation            *activity.ExternalOperationIdentity
}

// copied from action_fixture_test.go
type actionLifecycleCountingCatalog struct {
	agentrun.ActionExecutionCatalog
	calls *int
}

// copied from action_fixture_test.go
func (c *actionLifecycleCountingCatalog) Resolve(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string, bindings ...skill.BindingReference) (*skill.BoundAction, error) {
	(*c.calls)++
	return c.ActionExecutionCatalog.Resolve(ctx, scope, deploymentID, skillID, version, action, bindings...)
}

// copied from action_fixture_test.go
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

// copied from action_fixture_test.go
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

// copied from action_runtime_maintenance_test.go
type actionRuntimeMaintenanceCatalog struct {
	agentrun.ActionExecutionCatalog
	calls        *int
	afterResolve func()
}

// copied from action_runtime_maintenance_test.go
func (c *actionRuntimeMaintenanceCatalog) Resolve(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string, bindings ...skill.BindingReference) (*skill.BoundAction, error) {
	(*c.calls)++
	bound, err := c.ActionExecutionCatalog.Resolve(ctx, scope, deploymentID, skillID, version, action, bindings...)
	if c.afterResolve != nil {
		c.afterResolve()
	}
	return bound, err
}

// copied from connect_account_test.go
type availabilityFunc func(context.Context, action.CredentialAvailabilityRequest) (action.CredentialAvailability, error)

// copied from connect_account_test.go
func (f availabilityFunc) CheckCredentialAvailability(ctx context.Context, req action.CredentialAvailabilityRequest) (action.CredentialAvailability, error) {
	return f(ctx, req)
}

// copied from action_loop_store_test.go
// loopStoreFixture is one running Run claimed by loopStoreWorker under a live
// lease, whose step "step-1" proposes loop calls.
type loopStoreFixture struct {
	t     *testing.T
	ctx   context.Context
	store models.KernelStore
	scope models.Scope
	now   time.Time
	run   *models.AgentRun
}

// copied from action_loop_store_test.go
// loopCall is a ready loop call of step-1 with its own idempotency key.
func (f *loopStoreFixture) loopCall(id string) *models.ActionCall {
	call := &models.ActionCall{
		ID: id, Scope: f.scope, RunID: f.run.ID, TurnID: "step-1", DeploymentID: "agent",
		SkillID: "repo", SkillVersion: "1.0.0", Action: "read_file", Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop,
		Status: models.ActionCallStatusReady, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
		Arguments: map[string]interface{}{"path": id}, IdempotencyKey: "tool-call:" + id,
		MaxAttempts: 1, AvailableAt: f.now, Revision: 1, CreatedAt: f.now, UpdatedAt: f.now,
	}
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	call.SemanticDigest = models.ComputeActionSemanticDigest(call)
	return call
}

// copied from action_loop_store_test.go
func (f *loopStoreFixture) event(id, eventType string) *models.ActivityEvent {
	return &models.ActivityEvent{
		ID: id, Scope: f.scope, EventType: eventType, Severity: models.ActivitySeverityInfo, AgentID: "agent", RunID: f.run.ID,
		Actor: models.ActivityActor{Type: "worker", ID: loopStoreWorker}, Summary: "Loop call", Visibility: models.ActivityVisibilityScope, CreatedAt: f.now,
	}
}

// copied from action_loop_store_test.go
// proposal is call's loop proposal without a Run change, fenced by the live
// Run lease.
func (f *loopStoreFixture) proposal(call *models.ActionCall) models.ActionProposalRecord {
	return models.ActionProposalRecord{
		Call: call, ExpectedRunRevision: f.run.Revision, Lease: &models.AgentRunLeaseGuard{WorkerID: loopStoreWorker, Now: f.now},
		Event: f.event("event-"+call.ID, "action.proposed"),
	}
}

// copied from action_loop_store_test.go
func (f *loopStoreFixture) propose(call *models.ActionCall) *models.ActionCall {
	f.t.Helper()
	result, err := f.store.CreateActionProposal(f.ctx, f.proposal(call))
	if err != nil || result == nil || !result.Created {
		f.t.Fatalf("propose %s = %#v, %v", call.ID, result, err)
	}
	return result.Call
}

// copied from action_loop_store_test.go
func (f *loopStoreFixture) currentRun() *models.AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.ctx, f.scope, f.run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

// copied from action_loop_store_test.go
// setRunStatus moves the Run to status outside any lease, as a park,
// requeue, pause or cancel would.
func (f *loopStoreFixture) setRunStatus(status models.AgentRunStatus) {
	f.t.Helper()
	current := f.currentRun()
	updated := models.CloneAgentRun(current)
	updated.Status = status
	updated.Revision++
	updated.UpdatedAt = f.now
	if status != models.AgentRunStatusRunning {
		updated.LeaseOwner, updated.LeaseExpiresAt = "", nil
	}
	if models.IsTerminalAgentRunStatus(status) {
		updated.CompletedAt = &f.now
	}
	if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, updated, current.Revision, f.event("status-"+string(status)+"-"+uuid.NewString()[:8], "run.status_changed"), nil); err != nil {
		f.t.Fatalf("set Run %s: %v", status, err)
	}
	f.run = f.currentRun()
}

// copied from action_loop_store_test.go
func (f *loopStoreFixture) poolClaim(at time.Time) *models.ActionCall {
	f.t.Helper()
	claimed, err := f.store.ClaimNextAction(f.ctx, models.ActionClaim{Scope: f.scope, WorkerID: "pool-worker", Now: at, LeaseDuration: time.Minute})
	if err != nil {
		f.t.Fatal(err)
	}
	return claimed
}

// copied from action_loop_store_test.go
func (f *loopStoreFixture) ownClaim(id string, at time.Time) *models.ActionCall {
	f.t.Helper()
	claimed, err := f.store.ClaimNextAction(f.ctx, models.ActionClaim{Scope: f.scope, WorkerID: loopStoreWorker, Now: at, LeaseDuration: models.LoopActionLease, OwnActionID: id})
	if err != nil {
		f.t.Fatal(err)
	}
	return claimed
}

// copied from action_loop_store_test.go
func (f *loopStoreFixture) requireNoCall(id string) {
	f.t.Helper()
	if call, err := f.store.GetActionCall(f.ctx, f.scope, id); !errors.Is(err, models.ErrActionNotFound) {
		f.t.Fatalf("call %s = %#v, %v; want none", id, call, err)
	}
}

// copied from action_loop_store_test.go
// proposeLoopApproval commits a write loop call of step-1 waiting on its
// approval, with the Run unchanged unless the call reserves budget.
func (f *loopStoreFixture) proposeLoopApproval(id string, opts loopApprovalOptions) (*models.ActionCall, *models.ApprovalCheckpoint) {
	f.t.Helper()
	call := f.loopCall(id)
	call.Action, call.Risk, call.SideEffect = "push", skill.RiskLevelExternal, skill.SideEffectExternal
	call.Status, call.ApprovalID = models.ActionCallStatusWaitingApproval, "approval-"+id
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	call.SemanticDigest = models.ComputeActionSemanticDigest(call)
	proposed := map[string]interface{}{"skillId": call.SkillID, "action": call.Action, "arguments": maps.Clone(call.Arguments)}
	if opts.alwaysReview {
		proposed["review"] = "always"
	}
	approval := &models.ApprovalCheckpoint{
		ID: call.ApprovalID, Scope: f.scope, RunID: f.run.ID, ActionCallID: call.ID, Status: models.ApprovalStatusPending,
		Risk: skill.RiskLevelExternal, Summary: "Push the fix to the release branch", ProposedAction: proposed,
		EligibleApprovers: []models.ApprovalPrincipal{loopApprover}, TimeoutDecision: opts.timeout,
		ExpiresAt: f.now.Add(time.Hour), Revision: 1, CreatedAt: f.now, UpdatedAt: f.now,
	}
	proposal := f.proposal(call)
	proposal.Approval = approval
	if opts.reserveBudget {
		current := f.currentRun()
		reserved := models.CloneAgentRun(current)
		if err := models.ReserveRunBudget(reserved, models.BudgetReservation{ID: agentrun.ActionBudgetReservationID(call.ID), Usage: models.BudgetUsage{Actions: 1}, CreatedAt: f.now}); err != nil {
			f.t.Fatal(err)
		}
		reserved.Revision++
		proposal.Run, proposal.ExpectedRunRevision = reserved, current.Revision
	}
	result, err := f.store.CreateActionProposal(f.ctx, proposal)
	if err != nil || !result.Created {
		f.t.Fatalf("propose %s = %#v, %v", id, result, err)
	}
	f.run = f.currentRun()
	return result.Call, result.Approval
}

// copied from action_loop_store_test.go
// parkOn moves the running Run to waiting_for_approval on approvalID, as the
// loop's step-end park does.
func (f *loopStoreFixture) parkOn(approvalID string) {
	f.t.Helper()
	current := f.currentRun()
	parked := models.CloneAgentRun(current)
	parked.Status = models.AgentRunStatusWaitingForApproval
	parked.WakeCondition = &models.WakeCondition{Type: "approval", Reference: approvalID}
	parked.LeaseOwner, parked.LeaseExpiresAt = "", nil
	parked.Revision++
	parked.UpdatedAt = f.now
	if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, parked, current.Revision, f.event("park-"+uuid.NewString()[:8], "run.waiting"), nil); err != nil {
		f.t.Fatalf("park on %s: %v", approvalID, err)
	}
	f.run = f.currentRun()
}

// copied from action_loop_store_test.go
func (f *loopStoreFixture) approvalCoordinator(at time.Time) *action.ApprovalCoordinator {
	coordinator := action.NewApprovalCoordinator(f.store, f.store, action.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return at }))
	return coordinator
}

// copied from action_loop_store_test.go
func (f *loopStoreFixture) decide(coordinator *action.ApprovalCoordinator, approval *models.ApprovalCheckpoint, decision models.ApprovalDecision, reason string) (*models.ApprovalResolutionResult, error) {
	return coordinator.Resolve(f.ctx, action.ResolveApprovalRequest{
		Scope: f.scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision,
		DecisionID: "decision-" + approval.ID, Decision: decision, Principal: loopApprover, Reason: reason,
	})
}

// copied from action_loop_store_test.go
func newLoopStoreFixture(t *testing.T, store models.KernelStore) *loopStoreFixture {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "loop-" + uuid.NewString()[:8]}
	now := time.Now().UTC().Truncate(time.Millisecond)
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	created, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "check the repo", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: loopStoreWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	return &loopStoreFixture{t: t, ctx: ctx, store: store, scope: scope, now: now, run: run}
}

// copied from action_loop_store_test.go
var loopApprover = models.ApprovalPrincipal{Type: "user", ID: "user"}

// copied from space_seed_test.go
// seedTestSpaceFor creates a space owned by ownerID. The owner's first space
// in the scope is their default space. An owner who already has a default
// space keeps it and gets no second one, so a fixture that is built more than
// once on a store can seed every time.
func seedTestSpaceFor(t testing.TB, store models.SpaceStore, scope models.Scope, ownerID string) *models.Space {
	t.Helper()
	if existing, err := store.DefaultSpace(context.Background(), scope, ownerID); err == nil {
		return existing
	} else if !errors.Is(err, models.ErrNoDefaultSpace) {
		t.Fatalf("seed space: %v", err)
	}
	space, err := portfolio.NewSpaceService(store).CreateSpace(context.Background(), portfolio.CreateSpaceRequest{
		Scope: scope, Name: "Home", Look: models.DefaultSpaceLook, OwnerID: ownerID,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	return space
}

// copied from space_seed_test.go
// seedTestSpace gives the empty owner a default space in the scope, so a
// conversation created with no space and no person behind it has one to go to.
func seedTestSpace(t testing.TB, store models.SpaceStore, scope models.Scope) *models.Space {
	t.Helper()
	return seedTestSpaceFor(t, store, scope, "")
}

// copied from kernel_store_test.go
// openTestSQLiteDB opens the test's own connection to the SQLite file at
// path, closed when the test ends.
func openTestSQLiteDB(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// copied from action_loop_store_test.go
const loopStoreWorker = "loop-worker"

// copied from action_loop_store_test.go
type loopApprovalOptions struct {
	reserveBudget bool
	timeout       models.ApprovalTimeoutDecision
	alwaysReview  bool
}

// copied from action_fixture_test.go
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

// copied from action_loop_store_test.go
func nestedActionStores(t *testing.T) []struct {
	name string
	open func(*testing.T) models.KernelStore
} {
	stores := []struct {
		name string
		open func(*testing.T) models.KernelStore
	}{
		{name: "sqlite", open: func(t *testing.T) models.KernelStore {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "nested.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	if dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"); dsn != "" {
		stores = append(stores, struct {
			name string
			open func(*testing.T) models.KernelStore
		}{name: "postgres", open: func(t *testing.T) models.KernelStore {
			store, _ := openTestPostgresStore(t, "openseal_nested_")
			return store
		}})
	}
	return stores
}

// copied from worker_idle_poll_test.go
func newTestActionPool(t *testing.T, store models.KernelStore, catalog *skill.Catalog, scope models.Scope, interval time.Duration, concurrency int, dispatcher agentrun.ActionDispatcher) *action.ActionWorkerPool {
	t.Helper()
	pool, err := action.NewActionWorkerPool(store, catalog, action.CredentialResolverFunc(func(context.Context, action.CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "test-secret"}, nil
	}), dispatcher, nil, action.ActionWorkerConfig{Scope: scope, Concurrency: concurrency, PollInterval: interval, LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

// copied from worker_idle_poll_test.go
func createActionPoolWork(t *testing.T, store models.KernelStore, catalog *skill.Catalog, scope models.Scope) *models.ActionProposalResult {
	t.Helper()
	now := time.Now().Add(-2 * time.Second)
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	// Action workers execute the nested calls of a running step; loop calls
	// run inline in their Run's loop.
	stepExpires := now.Add(time.Hour)
	step, err := store.CreateAgentTurn(t.Context(), &models.AgentTurn{
		ID: "step-" + run.ID, Scope: scope, RunID: claimed.ID, Status: models.AgentTurnStatusRunning, Producer: models.StepProducerModel,
		LeaseOwner: "agent-worker", LeaseExpiresAt: &stepExpires, Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := action.NewActionCoordinator(store, store, catalog, action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{Disposition: action.ActionDispositionAllow}, nil
	}), nil, models.WithClock(func() time.Time { return now.Add(time.Second) }))
	proposal, err := coordinator.Propose(t.Context(), action.ProposeActionRequest{
		Scope: scope, RunID: claimed.ID, TurnID: step.ID, WorkerID: "agent-worker", DeploymentID: "release-agent", Origin: models.ActionOriginVM, OriginRef: "command-1",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy", Arguments: map[string]interface{}{"environment": "production"}, IdempotencyKey: "deploy-" + run.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

// copied from loop_proposal_fixture_test.go
// loopProposalStep is the step that proposes the governed tool calls of a
// loopProposalFixture.
const loopProposalStep = "step-1"

// copied from action_fixture_test.go
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

// copied from postgres_test_schema_test.go
// openPostgresStoreOn opens another store on schema s, closed when the
// test ends.
func openPostgresStoreOn(t testing.TB, s storetest.PostgresSchema, options ...postgres.PostgresStoreOption) *postgres.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := postgres.NewPostgresStore(ctx, s.DSN, append([]postgres.PostgresStoreOption{postgres.WithPostgresSchema(s.Name)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// copied from postgres_test_schema_test.go
// openTestPostgresStore opens a Postgres store on a fresh schema (see
// storetest.NewPostgresSchema) and returns it with the schema.
func openTestPostgresStore(t testing.TB, prefix string, options ...postgres.PostgresStoreOption) (*postgres.Store, storetest.PostgresSchema) {
	t.Helper()
	schema := storetest.NewPostgresSchema(t, prefix)
	return openPostgresStoreOn(t, schema, options...), schema
}
