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

type sourceQualifiedWorkforceFixtureGenerator struct{}

func (sourceQualifiedWorkforceFixtureGenerator) Generate(context.Context, WorkforceAuthoringRequest) ([]byte, error) {
	return []byte(`{"candidate":{"agents":[{"id":"researcher","version":"1","displayName":"Researcher","purpose":"Research safely","systemPrompt":"Research with evidence.","skillRequirements":[{"skillId":"research","versionConstraint":"1.0.0","promptRequired":true}],"authority":{"maximumRisk":"read","maxConcurrentRuns":1}}],"assignments":[]},"questions":[]}`), nil
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

func TestPublicWorkforceInitiativeAuthoringContract(t *testing.T) {
	candidate := WorkforceCandidate{Initiative: &WorkforceInitiativeBlueprint{
		ID: "research", Title: "Research", Purpose: "Understand users",
		Owner:         WorkforceInitiativeOwnerReference{Type: WorkforceInitiativeOwnerTeam, DefinitionID: "research-team"},
		ObjectiveRefs: []string{WorkforceObjectiveKey("team", "research-team", "monitor")},
		Milestones:    []WorkforceInitiativeMilestoneBlueprint{{ID: "baseline", Title: "Baseline"}},
		Hypotheses:    []WorkforceInitiativeHypothesisBlueprint{{ID: "friction", Statement: "Setup is difficult", Confidence: 0.5}},
		SourceMonitors: []WorkforceInitiativeSourceMonitorBlueprint{{
			ID: "community", ObjectiveRef: WorkforceObjectiveKey("team", "research-team", "monitor"), AssignedAgentDefinitionID: "researcher",
			SkillID: "source", SkillVersion: "1", Action: "observe", SourcePolicyRef: "approved", Deduplication: WorkforceInitiativeDeduplicateStableSourceAndContent,
		}},
		Deliverables: []WorkforceInitiativeDeliverableBlueprint{{ID: "report", Title: "Cited report"}},
	}}
	placement := WorkforceChangeSetPlacement{InitiativeID: "initiative-live", InitiativeExpectedRevision: 2}
	catalog := WorkforceCapabilityCatalog{SourcePolicies: map[string]WorkforceSourcePolicyCapability{
		"approved@1": {Reference: "approved@1", Sources: []WorkforceSourcePolicySourceCapability{{Host: "community.example", PathPrefixes: []string{"/forum"}}}},
	}}
	if candidate.Initiative.Owner.DefinitionID != "research-team" || placement.InitiativeID != "initiative-live" || placement.InitiativeExpectedRevision != 2 || catalog.SourcePolicies["approved@1"].Sources[0].Host != "community.example" {
		t.Fatalf("public Initiative contract candidate=%#v placement=%#v", candidate, placement)
	}
}

func TestPublicInitiativeLifecycleConstants(t *testing.T) {
	initiative := Initiative{Status: InitiativeStatusActive}
	milestone := InitiativeMilestone{Status: InitiativeMilestonePending}
	hypothesis := InitiativeHypothesis{Status: InitiativeHypothesisOpen}
	deliverable := InitiativeDeliverable{Status: InitiativeDeliverablePlanned}
	monitor := InitiativeSourceMonitorReference{Deduplication: InitiativeSourceDeduplicateStableSourceAndContent}
	if initiative.Status != "active" || milestone.Status != "pending" || hypothesis.Status != "open" || deliverable.Status != "planned" || monitor.Deduplication != "stable_source_and_content" {
		t.Fatalf("public Initiative lifecycle constants drifted")
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

func TestEngineUpdatesWorkforcePlacementWithoutModelGeneration(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := New(WithPersistentStore(store), WithWorkforceAuthoringGenerator(sourceQualifiedWorkforceFixtureGenerator{}))
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := engine.CreateWorkforceChangeSet(t.Context(), CreateWorkforceChangeSetRequest{
		Scope: SkillScope{Kind: "workspace", ID: "local"}, Prompt: "Create a research Agent",
		Catalog: WorkforceCapabilityCatalog{Skills: map[string]WorkforceSkillCapability{
			"research": {ID: "research", Version: "1.0.0", PromptAvailable: true},
		}},
		Actor: WorkforceChangeSetActor{Type: "user", ID: "local"}, IdempotencyKey: "create-researcher",
	})
	if err != nil || created.Status != WorkforceChangeSetReview {
		t.Fatalf("created = %#v, err = %v", created, err)
	}
	evaluated, _, err := engine.SubmitWorkforceChangeSetEvaluation(t.Context(), SubmitWorkforceChangeSetEvaluationRequest{
		Scope: created.Scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, CandidateDigest: created.CandidateDigest,
		Allowed: true, Actor: WorkforceChangeSetActor{Type: "policy_evaluator", ID: "local"}, IdempotencyKey: "evaluate-before-placement",
		ApprovalRequirements: []WorkforceChangeSetApprovalRequirement{{PolicyID: "source-policy", Role: "operator", Count: 1}},
	})
	if err != nil || evaluated.Status != WorkforceChangeSetAwaitingApproval {
		t.Fatalf("evaluated = %#v, err = %v", evaluated, err)
	}
	request := UpdateWorkforceChangeSetPlacementRequest{
		Scope: evaluated.Scope, ChangeSetID: evaluated.ID, ExpectedRevision: evaluated.Revision,
		Placement: WorkforceChangeSetPlacement{SkillSourceIdentities: map[string]map[string]string{
			"researcher": {"research": "clawhub::@alice/research"},
		}},
		Reason: "Select the reviewed publisher", Actor: WorkforceChangeSetActor{Type: "user", ID: "local"}, IdempotencyKey: "select-alice",
	}
	updated, replayed, err := engine.UpdateWorkforceChangeSetPlacement(t.Context(), request)
	qualifiedAgentID := "workspace/local/researcher"
	if err != nil || replayed || updated.Status != WorkforceChangeSetReview || updated.Revision != evaluated.Revision+1 ||
		updated.Placement.SkillSourceIdentities[qualifiedAgentID]["research"] != "clawhub::@alice/research" || len(updated.PlacementUpdates) != 1 {
		t.Fatalf("updated = %#v, replayed = %t, err = %v", updated, replayed, err)
	}
	if replay, replayed, err := engine.UpdateWorkforceChangeSetPlacement(t.Context(), request); err != nil || !replayed || replay.Revision != updated.Revision {
		t.Fatalf("replay = %#v, replayed = %t, err = %v", replay, replayed, err)
	}
	request.IdempotencyKey = "stale-select-bob"
	request.Placement.SkillSourceIdentities["researcher"]["research"] = "clawhub::@bob/research"
	if _, _, err := engine.UpdateWorkforceChangeSetPlacement(t.Context(), request); err != ErrWorkforceChangeSetRevision {
		t.Fatalf("stale placement error = %v", err)
	}
	restored, err := engine.GetWorkforceChangeSet(t.Context(), updated.Scope, updated.ID)
	if err != nil || restored.Placement.SkillSourceIdentities[qualifiedAgentID]["research"] != "clawhub::@alice/research" || len(restored.PlacementUpdates) != 1 {
		t.Fatalf("restored = %#v, err = %v", restored, err)
	}
	reEvaluated, _, err := engine.SubmitWorkforceChangeSetEvaluation(t.Context(), SubmitWorkforceChangeSetEvaluationRequest{
		Scope: updated.Scope, ChangeSetID: updated.ID, ExpectedRevision: updated.Revision, CandidateDigest: updated.CandidateDigest,
		Allowed: true, Actor: WorkforceChangeSetActor{Type: "policy_evaluator", ID: "local"}, IdempotencyKey: "evaluate-after-placement",
		ApprovalRequirements: []WorkforceChangeSetApprovalRequirement{{PolicyID: "source-policy", Role: "operator", Count: 1}},
	})
	if err != nil || reEvaluated.Status != WorkforceChangeSetAwaitingApproval {
		t.Fatalf("re-evaluated = %#v, err = %v", reEvaluated, err)
	}
	if _, _, err := engine.ResolveWorkforceChangeSetApproval(t.Context(), ResolveWorkforceChangeSetApprovalRequest{
		Scope: reEvaluated.Scope, ChangeSetID: reEvaluated.ID, ExpectedRevision: reEvaluated.Revision,
		EvaluationID: evaluated.Evaluations[0].ID, PolicyID: "source-policy", Role: "operator", Approved: true,
		Actor: WorkforceChangeSetActor{Type: "user", ID: "local"}, IdempotencyKey: "approve-stale-evaluation",
	}); err == nil {
		t.Fatal("approval against a superseded placement evaluation was accepted")
	}
}
