package openseal

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

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
