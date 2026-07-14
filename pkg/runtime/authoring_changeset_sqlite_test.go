package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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
		"research": {ID: "research", Version: "1.0.0", PromptAvailable: true},
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

func TestSQLiteWorkforceApplyRequiresExactSourceForCollidingSkills(t *testing.T) {
	for _, test := range []struct {
		name           string
		sourceIdentity string
		wantAmbiguous  bool
	}{
		{name: "exact", sourceIdentity: "clawhub::@alice/research"},
		{name: "ambiguous", wantAmbiguous: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			catalog := skill.NewCatalogWithStore(store)
			for _, identity := range []string{"clawhub::@alice/research", "clawhub::@bob/research"} {
				if err := catalog.Register(ctx, &skill.Definition{
					ID: "research", Version: "1.0.0", Name: "Research",
					Source: &skill.SourceProvenance{Identity: identity, Format: "openclaw.skill.v1"},
					Prompt: &skill.PromptModule{Instructions: "Preserve cited evidence."},
				}); err != nil {
					t.Fatal(err)
				}
			}
			value := testApplicableWorkforceChangeSet()
			value.Result.Candidate.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "research", PromptRequired: true}}
			value.Result.Candidate.Agents[0].Authority.AllowedSkillIDs = []string{"research"}
			value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
				"research": {ID: "research", Version: "1.0.0", PromptAvailable: true},
			}}
			if test.sourceIdentity != "" {
				value.Placement.SkillSourceIdentities = map[string]map[string]string{"agent": {"research": test.sourceIdentity}}
			}
			if _, _, err := store.CreateChangeSet(ctx, value, "create", "digest"); err != nil {
				t.Fatal(err)
			}
			applied := appliedRuntimeChangeSet(value, "receipt", "apply", value.UpdatedAt.Add(time.Minute))
			result, err := store.ApplyChangeSet(ctx, applied, 2)
			if test.wantAmbiguous {
				if !errors.Is(err, skill.ErrDefinitionAmbiguous) {
					t.Fatalf("ambiguous apply = %#v, %v", result, err)
				}
				if _, err := store.GetDefinition(ctx, "agent", "1"); !errors.Is(err, agent.ErrDefinitionNotFound) {
					t.Fatalf("ambiguous apply leaked Agent state: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			bindings, err := store.ListSkillBindings(ctx, value.Scope, "agent-live")
			if err != nil || len(bindings) != 1 || bindings[0].SourceIdentity != test.sourceIdentity {
				t.Fatalf("exact workforce binding = %#v, %v", bindings, err)
			}
			prompts, err := catalog.ListModelPrompts(ctx, value.Scope, "agent-live")
			if err != nil || len(prompts) != 1 || prompts[0].BindingID != bindings[0].ID {
				t.Fatalf("exact model prompts = %#v, %v", prompts, err)
			}
			encoded, _ := json.Marshal(prompts)
			if strings.Contains(string(encoded), test.sourceIdentity) {
				t.Fatalf("model prompt leaked source identity: %s", encoded)
			}
		})
	}
}

func TestSQLiteAtomicWorkforceApplyMaterializesInitiativeAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	registerInitiativeSourceSkill(t, store)
	value := testInitiativeWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, value, "create-initiative", "digest"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-initiative", IdempotencyKey: "apply-initiative", CandidateDigest: value.CandidateDigest, Actor: value.Actor, AppliedAt: value.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(ctx, applied, 2)
	if err != nil || len(result.ApplyReceipt.Resources) != 8 {
		t.Fatalf("apply result=%#v err=%v", result, err)
	}
	initiative, err := store.GetInitiative(ctx, Scope{Kind: value.Scope.Kind, ID: value.Scope.ID}, value.Placement.InitiativeID)
	if err != nil || initiative.Status != InitiativeStatusActive || initiative.Owner != (ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-live"}) || initiative.Revision != 1 ||
		len(initiative.ObjectiveRefs) != 1 || initiative.ObjectiveRefs[0] != "objective:team" || len(initiative.SourceMonitors) != 1 ||
		initiative.SourceMonitors[0].AssignedAgentID != "agent-live" || initiative.SourceMonitors[0].ObjectiveID != "objective:team" ||
		len(initiative.Milestones) != 1 || len(initiative.Hypotheses) != 1 || len(initiative.Deliverables) != 1 {
		t.Fatalf("Initiative=%#v err=%v", initiative, err)
	}
	objectives, err := store.ListObjectives(ctx, ObjectiveFilter{Scope: initiative.Scope})
	if err != nil {
		t.Fatal(err)
	}
	var monitorObjective *Objective
	for _, objective := range objectives {
		if objective.ID == "objective:team" {
			monitorObjective = objective
		}
	}
	if monitorObjective == nil || monitorObjective.Cadence == nil || monitorObjective.Cadence.AssignedAgentID != "agent-live" ||
		monitorObjective.Cadence.RunTemplate.Context["initiativeId"] != initiative.ID ||
		monitorObjective.Cadence.RunTemplate.Capability.SkillVersion != "1.2.3" {
		t.Fatalf("materialized monitor Objective=%#v", monitorObjective)
	}
	found := false
	for _, resource := range result.ApplyReceipt.Resources {
		found = found || resource.Kind == "initiative" && resource.ID == initiative.ID && resource.Revision == 1
	}
	if !found {
		t.Fatalf("Initiative missing from receipt: %#v", result.ApplyReceipt.Resources)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetInitiative(ctx, initiative.Scope, initiative.ID)
	if err != nil || restored.Revision != 1 || restored.SourceMonitors[0] != initiative.SourceMonitors[0] || restored.CreationFingerprint == "" || restored.IdempotencyKeyHash == "" {
		t.Fatalf("restored Initiative=%#v err=%v", restored, err)
	}
}

func TestSQLiteAtomicWorkforceInitiativeAmendUsesCASWithoutPartialState(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	registerInitiativeSourceSkill(t, store)
	created := testInitiativeWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create-initiative", "create"); err != nil {
		t.Fatal(err)
	}
	first := cloneRuntimeChangeSet(created)
	first.Status, first.Revision = authoring.ChangeSetApplied, 3
	first.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create", IdempotencyKey: "apply-create", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	first.UpdatedAt = first.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, first, 2); err != nil {
		t.Fatal(err)
	}

	stale := initiativeAmendChangeSet(created, "amend-stale", 99)
	if _, _, err = store.CreateChangeSet(ctx, stale, "amend-stale", "amend-stale"); err != nil {
		t.Fatal(err)
	}
	staleApply := appliedRuntimeChangeSet(stale, "receipt-stale", "apply-stale", first.UpdatedAt.Add(time.Minute))
	if _, err = store.ApplyChangeSet(ctx, staleApply, 2); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("stale Initiative apply=%v", err)
	}
	if _, err = store.GetDefinition(ctx, "agent", "2"); !errors.Is(err, agent.ErrDefinitionNotFound) {
		t.Fatalf("stale Initiative apply leaked Agent definition: %v", err)
	}
	current, err := store.GetInitiative(ctx, Scope{Kind: "tenant", ID: "one"}, created.Placement.InitiativeID)
	if err != nil || current.Revision != 1 || current.Title != "Research program" {
		t.Fatalf("Initiative after stale apply=%#v err=%v", current, err)
	}

	amend := initiativeAmendChangeSet(created, "amend-valid", 1)
	if _, _, err = store.CreateChangeSet(ctx, amend, "amend-valid", "amend-valid"); err != nil {
		t.Fatal(err)
	}
	validApply := appliedRuntimeChangeSet(amend, "receipt-amend", "apply-amend", first.UpdatedAt.Add(2*time.Minute))
	if _, err = store.ApplyChangeSet(ctx, validApply, 2); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetInitiative(ctx, Scope{Kind: "tenant", ID: "one"}, created.Placement.InitiativeID)
	if err != nil || updated.Revision != 2 || updated.Title != "Research program v2" || !updated.CreatedAt.Equal(current.CreatedAt) || updated.IdempotencyKeyHash != current.IdempotencyKeyHash {
		t.Fatalf("amended Initiative=%#v err=%v", updated, err)
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

func TestSQLiteAtomicWorkforceAmendCreatesUnappliedResourcesAndPreservesUnrelatedBindings(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	catalog := skill.NewCatalogWithStore(store)
	if err = catalog.Register(ctx, &skill.Definition{ID: "unrelated", Version: "1", Name: "Unrelated", Prompt: &skill.PromptModule{Instructions: "Remain bound."}}); err != nil {
		t.Fatal(err)
	}
	unrelated := &skill.Binding{ID: "workforce:other-live:unrelated", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "other-live", SkillID: "unrelated", SkillVersion: "1", EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1}
	if err = catalog.Bind(ctx, unrelated); err != nil {
		t.Fatal(err)
	}

	parent := testApplicableWorkforceChangeSet()
	parent.ID = "rejected-parent"
	parent.Status = authoring.ChangeSetRejected
	if _, _, err = store.CreateChangeSet(ctx, parent, "parent", "parent"); err != nil {
		t.Fatal(err)
	}
	recovered := testApplicableWorkforceChangeSet()
	recovered.ID = "recovered"
	recovered.ParentID = parent.ID
	recovered.Mode = authoring.ModeAmend
	if _, _, err = store.CreateChangeSet(ctx, recovered, "recovered", "recovered"); err != nil {
		t.Fatal(err)
	}
	applied := cloneRuntimeChangeSet(recovered)
	applied.Status = authoring.ChangeSetApplied
	applied.Revision = 3
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-recovered", IdempotencyKey: "apply-recovered", CandidateDigest: recovered.CandidateDigest, Actor: recovered.Actor, AppliedAt: recovered.UpdatedAt.Add(time.Minute)}
	applied.UpdatedAt = applied.ApplyReceipt.AppliedAt
	result, err := store.ApplyChangeSet(ctx, applied, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deployment, err := store.GetDeployment(ctx, recovered.Scope, "agent-live"); err != nil || deployment.Revision != 1 {
		t.Fatalf("Agent deployment=%#v err=%v", deployment, err)
	}
	if deployment, err := store.GetTeamDeployment(ctx, recovered.Scope, "team-live"); err != nil || deployment.Revision != 1 {
		t.Fatalf("Team deployment=%#v err=%v", deployment, err)
	}
	if prompts, err := catalog.ListModelPrompts(ctx, unrelated.Scope, unrelated.DeploymentID); err != nil || len(prompts) != 1 {
		t.Fatalf("unrelated prompts=%#v err=%v", prompts, err)
	}
	if result.ApplyReceipt == nil || len(result.ApplyReceipt.Resources) != 6 {
		t.Fatalf("receipt=%#v", result.ApplyReceipt)
	}
}

func TestSQLiteAtomicWorkforceAmendSupportsMixedCreateAndUpdatePlacements(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	created := testApplicableWorkforceChangeSet()
	if _, _, err = store.CreateChangeSet(ctx, created, "create-mixed", "create-mixed"); err != nil {
		t.Fatal(err)
	}
	first := cloneRuntimeChangeSet(created)
	first.Status, first.Revision = authoring.ChangeSetApplied, 3
	first.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-create-mixed", IdempotencyKey: "apply-create-mixed", CandidateDigest: created.CandidateDigest, Actor: created.Actor, AppliedAt: created.UpdatedAt.Add(time.Minute)}
	first.UpdatedAt = first.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, first, 2); err != nil {
		t.Fatal(err)
	}

	amend := testApplicableWorkforceChangeSet()
	amend.ID, amend.ParentID, amend.Mode, amend.CandidateDigest = "mixed", created.ID, authoring.ModeAmend, "candidate-mixed"
	newAgent := &agent.AgentDefinition{ID: "reviewer", Version: "1", DisplayName: "Reviewer", Purpose: "Review", SystemPrompt: "Review the work", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}}
	amend.Result.Candidate.Agents = append(amend.Result.Candidate.Agents, newAgent)
	amend.Result.Candidate.Team.Version = "2"
	amend.Result.Candidate.Team.Roles = append(amend.Result.Candidate.Team.Roles, team.RoleSlot{ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review", MinimumMembers: 1, MaximumMembers: 1, RequiredDefinitionIDs: []string{"reviewer"}})
	amend.Result.Candidate.Assignments = append(amend.Result.Candidate.Assignments, authoring.Assignment{ID: "reviewer", RoleID: "reviewer", AgentDefinitionID: "reviewer"})
	amend.Placement.AgentDeploymentIDs["reviewer"] = "reviewer-live"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 1}
	amend.Placement.TeamExpectedRevision = 1
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	if _, _, err = store.CreateChangeSet(ctx, amend, "mixed", "mixed"); err != nil {
		t.Fatal(err)
	}
	second := cloneRuntimeChangeSet(amend)
	second.Status, second.Revision = authoring.ChangeSetApplied, 3
	second.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-mixed", IdempotencyKey: "apply-mixed", CandidateDigest: amend.CandidateDigest, Actor: amend.Actor, AppliedAt: first.UpdatedAt.Add(time.Minute)}
	second.UpdatedAt = second.ApplyReceipt.AppliedAt
	if _, err = store.ApplyChangeSet(ctx, second, 2); err != nil {
		t.Fatal(err)
	}
	if deployment, err := store.GetDeployment(ctx, amend.Scope, "agent-live"); err != nil || deployment.Revision != 2 {
		t.Fatalf("updated Agent=%#v err=%v", deployment, err)
	}
	if deployment, err := store.GetDeployment(ctx, amend.Scope, "reviewer-live"); err != nil || deployment.Revision != 1 {
		t.Fatalf("new Agent=%#v err=%v", deployment, err)
	}
}

func testApplicableWorkforceChangeSet() *authoring.ChangeSet {
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	agentDefinition := &agent.AgentDefinition{ID: "agent", Version: "1", DisplayName: "Agent", Purpose: "Work", SystemPrompt: "Do the work", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}, ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "agent-goal", Title: "Agent goal", Goal: "Finish agent work"}}}
	teamDefinition := &team.Definition{ID: "team", Version: "1", DisplayName: "Team", Purpose: "Work together", Roles: []team.RoleSlot{{ID: "worker", DisplayName: "Worker", Purpose: "Work", MinimumMembers: 1, MaximumMembers: 1, RequiredDefinitionIDs: []string{"agent"}}}, Coordination: team.CoordinationPolicy{Mode: team.CoordinationDynamic}, Approvals: team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead}, ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "team-goal", Title: "Team goal", Goal: "Finish team work"}}}
	return &authoring.ChangeSet{ID: "change", Scope: scope, Mode: authoring.ModeCreate, Prompt: "Create", PromptDigest: "prompt", CandidateDigest: "candidate", Result: authoring.CompileResult{Candidate: authoring.WorkforceCandidate{Agents: []*agent.AgentDefinition{agentDefinition}, Team: teamDefinition, Assignments: []authoring.Assignment{{ID: "worker", RoleID: "worker", AgentDefinitionID: "agent"}}}, Valid: true}, Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "team-live", AgentDeploymentIDs: map[string]string{"agent": "agent-live"}, Objectives: map[string]authoring.ObjectivePlacement{authoring.WorkforceObjectiveKey("agent", "agent", "agent-goal"): {ID: "objective:agent"}, authoring.WorkforceObjectiveKey("team", "team", "team-goal"): {ID: "objective:team"}}, Environment: "test"}, Status: authoring.ChangeSetReady, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, Revision: 2, CreatedAt: now, UpdatedAt: now}
}

func testInitiativeWorkforceChangeSet() *authoring.ChangeSet {
	value := testApplicableWorkforceChangeSet()
	agentDefinition := value.Result.Candidate.Agents[0]
	agentDefinition.SkillRequirements = []agent.SkillRequirement{{SkillID: "community-source", VersionConstraint: "1.2.3", RequiredActions: []string{"observe"}}}
	agentDefinition.Authority.AllowedSkillIDs = []string{"community-source"}
	value.Catalog = authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{
		"community-source": {ID: "community-source", Version: "1.2.3", Actions: []string{"observe"}, MaximumRisk: capability.RiskLevelRead},
	}}
	teamObjective := &value.Result.Candidate.Team.ObjectiveTemplates[0]
	teamObjective.Cadence = map[string]interface{}{
		"type": "interval", "intervalSeconds": int64(3600), "assignedAgentId": "agent", "maximumConcurrent": 1,
		"runBudget": map[string]interface{}{"maxTurns": int64(2), "maxActions": int64(1), "maxDurationMs": int64(60000)},
		"runTemplate": map[string]interface{}{
			"entrypoint": "monitor",
			"context":    map[string]interface{}{"initiativeId": "research-program", "sourceMonitorId": "community-listening"},
			"policy":     map[string]interface{}{"sourcePolicyRef": "approved-communities"},
			"capability": map[string]interface{}{"skillId": "community-source", "skillVersion": "1.2.3", "action": "observe", "inputs": map[string]interface{}{"query": "agent runtime pain points"}},
		},
	}
	teamObjectiveRef := authoring.WorkforceObjectiveKey(authoring.InitiativeOwnerTeam, "team", "team-goal")
	value.Result.Candidate.Initiative = &authoring.InitiativeBlueprint{
		ID: "research-program", Title: "Research program", Purpose: "Continuously understand user pain points",
		Owner:         authoring.InitiativeOwnerReference{Type: authoring.InitiativeOwnerTeam, DefinitionID: "team"},
		ObjectiveRefs: []string{teamObjectiveRef},
		Milestones:    []authoring.InitiativeMilestoneBlueprint{{ID: "baseline", Title: "Establish baseline", ObjectiveRefs: []string{teamObjectiveRef}}},
		Hypotheses:    []authoring.InitiativeHypothesisBlueprint{{ID: "setup-friction", Statement: "Setup friction limits adoption", Confidence: 0.5}},
		SourceMonitors: []authoring.InitiativeSourceMonitorBlueprint{{
			ID: "community-listening", ObjectiveRef: teamObjectiveRef, AssignedAgentDefinitionID: "agent", SkillID: "community-source", SkillVersion: "1.2.3", Action: "observe",
			SourcePolicyRef: "approved-communities", Deduplication: authoring.InitiativeDeduplicateStableSourceAndContent,
		}},
		Deliverables: []authoring.InitiativeDeliverableBlueprint{{ID: "cited-report", Title: "Cited report", ObjectiveRefs: []string{teamObjectiveRef}}},
		Policy:       map[string]interface{}{"outreachApproval": "required"},
	}
	value.Placement.InitiativeID = "initiative:research"
	return value
}

func registerInitiativeSourceSkill(t *testing.T, store skill.CatalogStore) {
	t.Helper()
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(context.Background(), &skill.Definition{
		ID: "community-source", Version: "1.2.3", Name: "Community source", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://community.invalid"},
		Actions: map[string]skill.Action{"observe": {
			Name: "observe", Description: "Observe a permitted community source", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
			Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

func initiativeAmendChangeSet(created *authoring.ChangeSet, id string, initiativeRevision int64) *authoring.ChangeSet {
	amend := testInitiativeWorkforceChangeSet()
	amend.ID, amend.ParentID, amend.Mode, amend.CandidateDigest = id, created.ID, authoring.ModeAmend, "candidate-"+id
	amend.Result.Candidate.Agents[0].Version = "2"
	amend.Result.Candidate.Team.Version = "2"
	amend.Result.Candidate.Initiative.Title = "Research program v2"
	amend.Placement.AgentExpectedRevisions = map[string]int64{"agent": 1}
	amend.Placement.TeamExpectedRevision = 1
	amend.Placement.InitiativeExpectedRevision = initiativeRevision
	for key, placement := range amend.Placement.Objectives {
		placement.ExpectedRevision = 1
		amend.Placement.Objectives[key] = placement
	}
	return amend
}

func appliedRuntimeChangeSet(value *authoring.ChangeSet, receiptID, key string, at time.Time) *authoring.ChangeSet {
	applied := cloneRuntimeChangeSet(value)
	applied.Status, applied.Revision, applied.UpdatedAt = authoring.ChangeSetApplied, 3, at
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: receiptID, IdempotencyKey: key, CandidateDigest: value.CandidateDigest, Actor: value.Actor, AppliedAt: at}
	return applied
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
