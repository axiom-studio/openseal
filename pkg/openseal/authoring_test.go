package openseal

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

type workforceFixtureGenerator struct{}

func (workforceFixtureGenerator) Generate(context.Context, WorkforceAuthoringRequest) ([]byte, error) {
	return []byte(`{"candidate":{"agents":[],"assignments":[]},"questions":["What should this Team own?"]}`), nil
}

type evaluableWorkforceFixtureGenerator struct{}

func (evaluableWorkforceFixtureGenerator) Generate(context.Context, WorkforceAuthoringRequest) ([]byte, error) {
	return []byte(`{"candidate":{"agents":[],"team":{"id":"team","version":"1","displayName":"Team","purpose":"Own work","roles":[{"id":"member","displayName":"Member","purpose":"Do work"}],"coordination":{"mode":"dynamic"},"approvals":{"maximumRisk":"read"}},"assignments":[]},"questions":[]}`), nil
}

func TestPublicWorkforceObjectivePlacementContract(t *testing.T) {
	key := WorkforceObjectiveKey("agent", "developer", "ship-feature")
	placement := WorkforceChangeSetPlacement{
		Objectives: map[string]WorkforceObjectivePlacement{
			key: {ID: "objective-live", ExpectedRevision: 3},
		},
	}
	if got := placement.Objectives[key]; got.ID != "objective-live" || got.ExpectedRevision != 3 {
		t.Fatalf("objective placement = %#v", got)
	}
}

func TestEngineExposesDurableWorkforceChangeSetsOnlyWithPersistentSupport(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := New(WithPersistentStore(store), WithWorkforceAuthoringGenerator(workforceFixtureGenerator{}))
	if err != nil {
		t.Fatal(err)
	}
	if !engine.WorkforceAuthoringAvailable() || !engine.WorkforceChangeSetsAvailable() {
		t.Fatal("durable workforce authoring was not exposed")
	}
	if !engine.WorkforceChangeSetApplyAvailable() {
		t.Fatal("atomic workforce Apply was not exposed for the shared SQLite kernel store")
	}
	created, replayed, err := engine.CreateWorkforceChangeSet(t.Context(), CreateWorkforceChangeSetRequest{
		Scope: SkillScope{Kind: "workspace", ID: "local"}, Prompt: "Create a Team", Catalog: WorkforceCapabilityCatalog{},
		Placement: WorkforceChangeSetPlacement{TeamDeploymentID: "team-live"}, Actor: WorkforceChangeSetActor{Type: "user", ID: "local"},
		IdempotencyKey: "create-team",
	})
	if err != nil || replayed || created.Status != WorkforceChangeSetBlocked {
		t.Fatalf("created = %#v, replayed = %t, err = %v", created, replayed, err)
	}
	restored, err := engine.GetWorkforceChangeSet(t.Context(), created.Scope, created.ID)
	if err != nil || restored.CandidateDigest != created.CandidateDigest {
		t.Fatalf("restored = %#v, err = %v", restored, err)
	}
}

func TestEngineSubmitsGovernedWorkforceEvaluation(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := New(WithPersistentStore(store), WithWorkforceAuthoringGenerator(evaluableWorkforceFixtureGenerator{}))
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := engine.CreateWorkforceChangeSet(t.Context(), CreateWorkforceChangeSetRequest{Scope: SkillScope{Kind: "workspace", ID: "local"}, Prompt: "Create a Team", Actor: WorkforceChangeSetActor{Type: "user", ID: "local"}, IdempotencyKey: "create"})
	if err != nil || created.Status != WorkforceChangeSetReview {
		t.Fatalf("created = %#v, err = %v", created, err)
	}
	evaluated, replay, err := engine.SubmitWorkforceChangeSetEvaluation(t.Context(), SubmitWorkforceChangeSetEvaluationRequest{
		Scope: created.Scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, CandidateDigest: created.CandidateDigest,
		Allowed: true, Actor: WorkforceChangeSetActor{Type: "policy_evaluator", ID: "local"}, IdempotencyKey: "evaluate",
		ApprovalRequirements: []WorkforceChangeSetApprovalRequirement{{PolicyID: "local-policy", Role: "operator", Count: 1}},
	})
	if err != nil || replay || evaluated.Status != WorkforceChangeSetAwaitingApproval || len(evaluated.Evaluations) != 1 {
		t.Fatalf("evaluated = %#v replay=%t err=%v", evaluated, replay, err)
	}
	approved, replay, err := engine.ResolveWorkforceChangeSetApproval(t.Context(), ResolveWorkforceChangeSetApprovalRequest{
		Scope: evaluated.Scope, ChangeSetID: evaluated.ID, ExpectedRevision: evaluated.Revision, EvaluationID: evaluated.Evaluations[0].ID,
		PolicyID: "local-policy", Role: "operator", Approved: true, Actor: WorkforceChangeSetActor{Type: "user", ID: "local"}, IdempotencyKey: "approve",
	})
	if err != nil || replay || approved.Status != WorkforceChangeSetReady || len(approved.ApprovalDecisions) != 1 {
		t.Fatalf("approved = %#v replay=%t err=%v", approved, replay, err)
	}
}
