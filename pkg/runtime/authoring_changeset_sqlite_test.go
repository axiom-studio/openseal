package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestSQLiteWorkforceChangeSetsAreConcurrentRestartSafeAndScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	value := testWorkforceChangeSet(scope, "change-one")
	var created, replayed atomic.Int32
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, replay, err := store.CreateChangeSet(context.Background(), value, "intent-one", "request-one")
			if err != nil || result == nil || result.ID != value.ID {
				t.Errorf("create = %#v, replay = %t, err = %v", result, replay, err)
				return
			}
			if replay {
				replayed.Add(1)
			} else {
				created.Add(1)
			}
		}()
	}
	wait.Wait()
	if created.Load() != 1 || replayed.Load() != 1 {
		t.Fatalf("created = %d, replayed = %d", created.Load(), replayed.Load())
	}
	if _, _, err := store.GetChangeSetByIdempotency(context.Background(), scope, "intent-one", "different"); !errors.Is(err, authoring.ErrChangeSetIdempotency) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	if _, err := store.GetChangeSet(context.Background(), capability.ScopeReference{Kind: "tenant", ID: "two"}, value.ID); !errors.Is(err, authoring.ErrChangeSetNotFound) {
		t.Fatalf("cross-scope read = %v", err)
	}
	updated := *value
	updated.Status, updated.Revision = authoring.ChangeSetReady, 2
	updated.UpdatedAt = value.UpdatedAt.Add(time.Minute)
	updated.ApprovalDecisions = []authoring.ChangeSetApprovalDecision{{ID: "decision", EvaluationID: "evaluation", PolicyID: "production", Role: "operator", Approved: true, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, DecidedAt: updated.UpdatedAt}}
	if persisted, err := store.UpdateChangeSet(context.Background(), &updated, 1); err != nil || persisted.Revision != 2 {
		t.Fatalf("update = %#v, err = %v", persisted, err)
	}
	stale := updated
	stale.Status, stale.Revision = authoring.ChangeSetRejected, 3
	if _, err := store.UpdateChangeSet(context.Background(), &stale, 1); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("stale update = %v", err)
	}
	modifiedCandidate := updated
	modifiedCandidate.CandidateDigest, modifiedCandidate.Revision = "changed", 3
	if _, err := store.UpdateChangeSet(context.Background(), &modifiedCandidate, 2); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("candidate mutation = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetChangeSet(context.Background(), scope, value.ID)
	if err != nil || restored.CandidateDigest != value.CandidateDigest || restored.Status != authoring.ChangeSetReady || restored.Revision != 2 || len(restored.ApprovalDecisions) != 1 || restored.ApprovalDecisions[0].ID != "decision" {
		t.Fatalf("restored = %#v, err = %v", restored, err)
	}
}

func TestSQLiteAtomicWorkforceApplyPersistsWholeAggregateAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	value := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status = authoring.ChangeSetApplied
	applied.Revision = 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt", IdempotencyKey: "apply", CandidateDigest: value.CandidateDigest, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, AppliedAt: value.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(context.Background(), applied, 2)
	if err != nil || result.Status != authoring.ChangeSetApplied || len(result.ApplyReceipt.Resources) != 6 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err = store.GetDefinition(context.Background(), "agent", "1"); err != nil {
		t.Fatal(err)
	}
	if deployment, err := store.GetDeployment(context.Background(), value.Scope, "agent-live"); err != nil || deployment.ActiveVersion != "1" {
		t.Fatalf("deployment=%#v err=%v", deployment, err)
	}
	if teamDeployment, err := store.GetTeamDeployment(context.Background(), value.Scope, "team-live"); err != nil || len(teamDeployment.Roster) != 1 {
		t.Fatalf("team=%#v err=%v", teamDeployment, err)
	}
	objectives, err := store.ListObjectives(context.Background(), ObjectiveFilter{Scope: Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}})
	if err != nil || len(objectives) != 2 {
		t.Fatalf("objectives=%d err=%v", len(objectives), err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetChangeSet(context.Background(), value.Scope, value.ID)
	if err != nil || restored.ApplyReceipt == nil || restored.ApplyReceipt.ID != "receipt" {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
}

func TestSQLiteWorkforceApplyMaterializesExecutableSkillBindings(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(context.Background(), &skill.Definition{
		ID: "research", Version: "1.0.0", Name: "Research", Prompt: &skill.PromptModule{Instructions: "Preserve cited evidence."},
	}); err != nil {
		t.Fatal(err)
	}
	value := testApplicableWorkforceChangeSet()
	value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "research", PromptRequired: true}}
	value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{"research"}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"research": {ID: "research", Version: "1.0.0", PromptAvailable: true, MaximumRisk: capability.RiskLevelRead},
	}}
	if _, _, err := store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt", IdempotencyKey: "apply", CandidateDigest: value.CandidateDigest, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, AppliedAt: value.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(context.Background(), applied, 2)
	if err != nil {
		t.Fatal(err)
	}
	prompts, err := catalog.ListModelPrompts(context.Background(), value.Scope, "agent-live")
	if err != nil || len(prompts) != 1 || prompts[0].SkillID != "research" {
		t.Fatalf("prompts=%#v error=%v", prompts, err)
	}
	found := false
	for _, resource := range result.ApplyReceipt.Resources {
		found = found || resource.Kind == "skill_binding" && resource.ID == "workforce:agent-live:research"
	}
	if !found {
		t.Fatalf("receipt resources=%#v", result.ApplyReceipt.Resources)
	}
}

func TestSQLiteAtomicWorkforceApplyConcurrentRetryHasOneReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	primary, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	replica, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	ctx := context.Background()
	ready := testApplicableWorkforceChangeSet()
	if _, _, err = primary.CreateChangeSet(ctx, ready, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	candidate := cloneRuntimeChangeSet(ready)
	candidate.Status = authoring.ChangeSetApplied
	candidate.Revision = 3
	candidate.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt", IdempotencyKey: "apply", CandidateDigest: ready.CandidateDigest, Actor: ready.Actor, AppliedAt: ready.UpdatedAt.Add(time.Minute)}
	candidate.UpdatedAt = candidate.ApplyReceipt.AppliedAt
	stores := []*SQLiteStore{primary, replica}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range stores {
		wg.Add(1)
		go func(store *SQLiteStore) {
			defer wg.Done()
			_, err := store.ApplyChangeSet(ctx, cloneRuntimeChangeSet(candidate), 2)
			errs <- err
		}(store)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	restored, err := primary.GetChangeSet(ctx, ready.Scope, ready.ID)
	if err != nil || restored.ApplyReceipt == nil || restored.ApplyReceipt.ID != "receipt" {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
}

func TestSQLiteAtomicWorkforceAmendRejectsStaleRevisionWithoutPartialState(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	created := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create", "create"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(created)
	applied.Status = authoring.ChangeSetApplied
	applied.Revision = 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create", IdempotencyKey: "apply-create", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, applied, 2); err != nil {
		t.Fatal(err)
	}
	amend := testApplicableWorkforceChangeSet()
	amend.ID = "amend"
	amend.Mode = authoring.ModeAmend
	amend.ParentID = created.ID
	amend.CandidateDigest = "candidate-2"
	amend.Result.Candidate.Agents[0].Version = "2"
	amend.Result.Candidate.Team.Version = "2"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 99}
	amend.Placement.TeamExpectedRevision = 1
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	if _, _, err = store.CreateChangeSet(ctx, amend, "amend", "amend"); err != nil {
		t.Fatal(err)
	}
	attempt := cloneRuntimeChangeSet(amend)
	attempt.Status = authoring.ChangeSetApplied
	attempt.Revision = 3
	attempt.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-amend", IdempotencyKey: "apply-amend", CandidateDigest: amend.CandidateDigest, Actor: amend.Actor, AppliedAt: amend.UpdatedAt.Add(time.Minute)}
	attempt.UpdatedAt = attempt.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, attempt, 2); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("stale apply=%v", err)
	}
	if _, err = store.GetDefinition(ctx, "agent", "2"); !errors.Is(err, agent.ErrDefinitionNotFound) {
		t.Fatalf("partial Agent definition=%v", err)
	}
	if _, err = store.GetTeamDefinition(ctx, "team", "2"); !errors.Is(err, team.ErrDefinitionNotFound) {
		t.Fatalf("partial Team definition=%v", err)
	}
	current, err := store.GetChangeSet(ctx, amend.Scope, amend.ID)
	if err != nil || current.Status != authoring.ChangeSetReady {
		t.Fatalf("change set=%#v err=%v", current, err)
	}
}

func TestSQLiteAtomicWorkforceAmendActivatesNewVersionsTogether(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	created := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create", "create"); err != nil {
		t.Fatal(err)
	}
	first := cloneRuntimeChangeSet(created)
	first.Status = authoring.ChangeSetApplied
	first.Revision = 3
	first.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create", IdempotencyKey: "apply-create", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	first.UpdatedAt = first.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, first, 2); err != nil {
		t.Fatal(err)
	}
	amend := testApplicableWorkforceChangeSet()
	amend.ID = "amend"
	amend.Mode = authoring.ModeAmend
	amend.ParentID = created.ID
	amend.CandidateDigest = "candidate-2"
	amend.Result.Candidate.Agents[0].Version = "2"
	amend.Result.Candidate.Team.Version = "2"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 1}
	amend.Placement.TeamExpectedRevision = 1
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	if _, _, err = store.CreateChangeSet(ctx, amend, "amend", "amend"); err != nil {
		t.Fatal(err)
	}
	second := cloneRuntimeChangeSet(amend)
	second.Status = authoring.ChangeSetApplied
	second.Revision = 3
	second.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-amend", IdempotencyKey: "apply-amend", CandidateDigest: amend.CandidateDigest, Actor: amend.Actor, AppliedAt: first.UpdatedAt.Add(time.Minute)}
	second.UpdatedAt = second.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, second, 2); err != nil {
		t.Fatal(err)
	}
	agentDeployment, err := store.GetDeployment(ctx, created.Scope, "agent-live")
	if err != nil || agentDeployment.ActiveVersion != "2" || agentDeployment.PreviousVersion != "1" || agentDeployment.Revision != 2 {
		t.Fatalf("Agent deployment=%#v err=%v", agentDeployment, err)
	}
	teamDeployment, err := store.GetTeamDeployment(ctx, created.Scope, "team-live")
	if err != nil || teamDeployment.ActiveVersion != "2" || teamDeployment.Revision != 2 {
		t.Fatalf("Team deployment=%#v err=%v", teamDeployment, err)
	}
	if versions, err := store.ListDefinitionVersions(ctx, "agent"); err != nil || len(versions) != 2 {
		t.Fatalf("Agent versions=%d err=%v", len(versions), err)
	}
	if versions, err := store.ListTeamDefinitionVersions(ctx, "team"); err != nil || len(versions) != 2 {
		t.Fatalf("Team versions=%d err=%v", len(versions), err)
	}
	objectives, err := store.ListObjectives(ctx, ObjectiveFilter{Scope: Scope{Kind: "tenant", ID: "one"}})
	if err != nil || len(objectives) != 2 || objectives[0].Revision != 2 || objectives[1].Revision != 2 {
		t.Fatalf("objective portfolio=%#v err=%v", objectives, err)
	}
}

func testApplicableWorkforceChangeSet() *authoring.ChangeSet {
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agentDefinition := &agent.AgentDefinition{ID: "agent", Version: "1", DisplayName: "Agent", Purpose: "Work", SystemPrompt: "Do the work", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}, ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "agent-goal", Title: "Agent goal", Goal: "Finish agent work"}}}
	teamDefinition := &team.Definition{ID: "team", Version: "1", DisplayName: "Team", Purpose: "Work together", Roles: []team.RoleSlot{{ID: "worker", DisplayName: "Worker", Purpose: "Work", MinimumMembers: 1, MaximumMembers: 1, RequiredDefinitionIDs: []string{"agent"}}}, Coordination: team.CoordinationPolicy{Mode: team.CoordinationDynamic}, Approvals: team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead}, ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "team-goal", Title: "Team goal", Goal: "Finish team work"}}}
	return &authoring.ChangeSet{ID: "change", Scope: scope, Mode: authoring.ModeCreate, Prompt: "Create", PromptDigest: "prompt", CandidateDigest: "candidate", Result: authoring.CompileResult{Candidate: authoring.WorkforceCandidate{Agents: []*agent.AgentDefinition{agentDefinition}, Team: teamDefinition, Assignments: []authoring.Assignment{{ID: "worker", RoleID: "worker", AgentDefinitionID: "agent"}}}, Valid: true}, Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "team-live", AgentDeploymentIDs: map[string]string{"agent": "agent-live"}, Objectives: map[string]authoring.ObjectivePlacement{authoring.WorkforceObjectiveKey("agent", "agent", "agent-goal"): {ID: "objective:agent"}, authoring.WorkforceObjectiveKey("team", "team", "team-goal"): {ID: "objective:team"}}, Environment: "test"}, Status: authoring.ChangeSetReady, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, Revision: 2, CreatedAt: now, UpdatedAt: now}
}

func cloneRuntimeChangeSet(value *authoring.ChangeSet) *authoring.ChangeSet {
	payload, _ := json.Marshal(value)
	var result authoring.ChangeSet
	_ = json.Unmarshal(payload, &result)
	return &result
}

func testWorkforceChangeSet(scope capability.ScopeReference, id string) *authoring.ChangeSet {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &authoring.ChangeSet{
		ID: id, Scope: scope, Mode: authoring.ModeCreate, Prompt: "Create a Team", PromptDigest: "prompt",
		CandidateDigest: "candidate", Result: authoring.CompileResult{Valid: true},
		Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "team-live"}, Status: authoring.ChangeSetReview,
		Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}
