package openseal

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	_ "github.com/mattn/go-sqlite3"
)

// umbrellaFixture registers one Agent and one Team definition for the
// deployment-ID namespace tests.
func umbrellaFixture(t *testing.T, engine *Engine) {
	t.Helper()
	ctx := context.Background()
	if _, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: "member", Version: "1", DisplayName: "Member", Purpose: "Help", SystemPrompt: "Help.",
		Authority: AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RegisterTeamDefinition(ctx, &TeamDefinition{
		ID: "crew", Version: "1", DisplayName: "Crew", Purpose: "Work together",
		Roles:     []TeamRoleSlot{{ID: "member", DisplayName: "Member", Purpose: "Help", MinimumMembers: 1}},
		Approvals: TeamApprovalPolicy{MaximumRisk: capability.RiskLevelRead, ApproverRoleIDs: []string{"member"}},
	}); err != nil {
		t.Fatal(err)
	}
}

func createUmbrellaAgent(engine *Engine, scope capability.ScopeReference, id string) error {
	_, _, err := engine.CreateAgentDeployment(context.Background(), &AgentDeployment{
		ID: id, Scope: scope, DefinitionID: "member", ActiveVersion: "1",
		RolloutStatus: AgentRolloutActive, Environment: "local", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "test")
	return err
}

func createUmbrellaTeam(engine *Engine, scope capability.ScopeReference, id, memberID string) error {
	_, _, err := engine.CreateTeamDeployment(context.Background(), &TeamDeployment{
		ID: id, Scope: scope, DefinitionID: "crew", ActiveVersion: "1", Status: TeamDeploymentActive,
		Roster: []TeamRosterAssignment{{ID: "member", RoleID: "member", AgentDeploymentID: memberID}},
	}, "user", "operator", "test")
	return err
}

func assertDeploymentIDNamespace(t *testing.T, engine *Engine) {
	t.Helper()
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	umbrellaFixture(t, engine)
	if err := createUmbrellaAgent(engine, scope, "shared"); err != nil {
		t.Fatal(err)
	}
	if err := createUmbrellaTeam(engine, scope, "shared", "shared"); !errors.Is(err, ErrDeploymentIDTaken) {
		t.Fatalf("Team with an Agent's ID: err = %v, want ErrDeploymentIDTaken", err)
	}
	if err := createUmbrellaTeam(engine, scope, "crew-one", "shared"); err != nil {
		t.Fatal(err)
	}
	if err := createUmbrellaAgent(engine, scope, "crew-one"); !errors.Is(err, ErrDeploymentIDTaken) {
		t.Fatalf("Agent with a Team's ID: err = %v, want ErrDeploymentIDTaken", err)
	}
	if owner, err := engine.ResolveUmbrella(ctx, scope, "shared"); err != nil || owner != (ObjectiveOwner{Type: OwnerTypeAgent, ID: "shared"}) {
		t.Fatalf("ResolveUmbrella(agent) = %#v, %v", owner, err)
	}
	if owner, err := engine.ResolveUmbrella(ctx, scope, "crew-one"); err != nil || owner != (ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew-one"}) {
		t.Fatalf("ResolveUmbrella(team) = %#v, %v", owner, err)
	}
	if _, err := engine.ResolveUmbrella(ctx, scope, "nobody"); !errors.Is(err, ErrDeploymentIDNotReserved) {
		t.Fatalf("ResolveUmbrella(unknown) err = %v", err)
	}
	other := capability.ScopeReference{Kind: "tenant", ID: "two"}
	if err := createUmbrellaAgent(engine, other, "crew-one"); err != nil {
		t.Fatalf("IDs are reserved per scope: %v", err)
	}
}

func TestDeploymentIDsShareOneNamespaceInMemory(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	assertDeploymentIDNamespace(t, engine)
}

func TestDeploymentIDsShareOneNamespaceInSQLite(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "ids.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := New(WithPersistentStore(store))
	if err != nil {
		t.Fatal(err)
	}
	assertDeploymentIDNamespace(t, engine)
}

func TestDeploymentIdentityBackfillFailsOnCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collision.db")
	store, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"agent_deployments", "team_deployments"} {
		if _, err := db.Exec(`INSERT INTO ` + table + `(scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES('tenant','one','dup','d','1',1,CURRENT_TIMESTAMP,'{}')`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`DELETE FROM deployment_identities`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := runtime.NewSQLiteStore(path); !errors.Is(err, ErrDeploymentIDTaken) {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("reopen with a shared Agent and Team ID: err = %v", err)
	}
}

func agentOwner(id string) ObjectiveOwner { return ObjectiveOwner{Type: OwnerTypeAgent, ID: id} }

// ensureTestAgent creates an active Agent deployment id in scope so owner
// checks accept it.
func ensureTestAgent(t *testing.T, engine *Engine, scope capability.ScopeReference, id string) ObjectiveOwner {
	t.Helper()
	ctx := context.Background()
	if owner, err := engine.ResolveUmbrella(ctx, scope, id); err == nil {
		return owner
	}
	definition := "test-agent-" + id
	if _, err := engine.GetAgentDefinition(ctx, definition, "1"); err != nil {
		if _, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
			ID: definition, Version: "1", DisplayName: "Test", Purpose: "Test", SystemPrompt: "Test.",
			Authority: AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, MaxConcurrentRuns: 1},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: id, Scope: scope, DefinitionID: definition, ActiveVersion: "1",
		RolloutStatus: AgentRolloutActive, Environment: "local", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "test"); err != nil {
		t.Fatal(err)
	}
	return agentOwner(id)
}

// teamRequestFixture parks a Team member's run on a credential request saved
// to the Team.
func teamRequestFixture(t *testing.T) (context.Context, *runtime.MemoryStore, *Engine, *runtime.AgentRun, *CredentialRequest, ObjectiveOwner) {
	t.Helper()
	ctx := context.Background()
	store := runtime.NewMemoryStore()
	engine, err := New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "one"}
	skillScope := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	umbrellaFixture(t, engine)
	for _, id := range []string{"member", "other"} {
		if err := createUmbrellaAgent(engine, skillScope, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := createUmbrellaTeam(engine, skillScope, "crew", "member"); err != nil {
		t.Fatal(err)
	}
	crew := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}
	run, err := runtime.NewPortfolioService(store).CreateAgentRun(ctx, runtime.CreateAgentRunRequest{Scope: scope, Kind: runtime.RunKindConversation,
		Owner: crew, AssignedAgentID: "member", Goal: "Order a cable", Source: runtime.RunSourceChat,
		Context: map[string]interface{}{"conversationId": "team-chat", "triggerMessageId": "message"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := &CredentialRequest{ID: "credential-request:call", Scope: scope, Owner: crew, DeploymentID: crew.ID, AgentID: "member", ConversationID: "team-chat", TriggerMessageID: "message",
		RunID: run.ID, ActionCallID: "call", Kind: CredentialRequestKindWebsiteLogin, Website: "https://www.amazon.in", Reason: "Add the login",
		Status: CredentialRequestStatusPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveCredentialRequest(ctx, request, 0); err != nil {
		t.Fatal(err)
	}
	run, err = store.ClaimNextAgentRun(ctx, runtime.AgentRunClaim{Scope: scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || run == nil {
		t.Fatal(err)
	}
	waiting, _, err := runtime.NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, runtime.RunTransitionRequest{
		ExpectedRevision: run.Revision, LeaseOwner: "worker", Status: runtime.AgentRunStatusWaitingForEvent, EventType: "run.waiting", Summary: "Waiting",
		Actor: runtime.ActivityActor{Type: "worker", ID: "worker"}, WakeCondition: &runtime.WakeCondition{Type: CredentialRequestWakeType, Reference: request.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, engine, waiting, request, crew
}

func TestTeamRequestResumesTheExactMemberRun(t *testing.T) {
	ctx, store, engine, run, request, crew := teamRequestFixture(t)
	if _, err := engine.ResolveCredentialRequest(ctx, run.Scope, agentOwner("member"), request.ID, 1, "user:5", false); err == nil {
		t.Fatal("a Team request was resolved under the member's own umbrella")
	}
	result, err := engine.ResolveCredentialRequest(ctx, run.Scope, crew, request.ID, 1, "user:5", false)
	if err != nil || !result.Continued {
		t.Fatalf("resolution = %#v, %v", result, err)
	}
	if resumed, _ := store.GetAgentRun(ctx, run.Scope, run.ID); resumed.Status != runtime.AgentRunStatusQueued {
		t.Fatalf("member run status = %s", resumed.Status)
	}
}

func TestTeamRequestDoesNotResumeAfterTheMemberLeft(t *testing.T) {
	ctx, store, engine, run, request, crew := teamRequestFixture(t)
	skillScope := capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	team, err := engine.GetTeamDeployment(ctx, skillScope, crew.ID)
	if err != nil {
		t.Fatal(err)
	}
	team.Roster = []TeamRosterAssignment{{ID: "member", RoleID: "member", AgentDeploymentID: "other"}}
	if _, _, err := engine.UpdateTeamDeployment(ctx, team, team.Revision, "user", "operator", "replace member"); err != nil {
		t.Fatal(err)
	}
	result, err := engine.ResolveCredentialRequest(ctx, run.Scope, crew, request.ID, 1, "user:5", false)
	if err != nil || result.Continued || result.Request.Status != CredentialRequestStatusResolved {
		t.Fatalf("resolution = %#v, %v", result, err)
	}
	if parked, _ := store.GetAgentRun(ctx, run.Scope, run.ID); parked.Status != runtime.AgentRunStatusWaitingForEvent {
		t.Fatalf("a removed member's run resumed: %s", parked.Status)
	}
}
