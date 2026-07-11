package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type sequenceChangeSetGenerator struct{ payloads [][]byte }

func (g *sequenceChangeSetGenerator) Generate(context.Context, GenerateRequest) ([]byte, error) {
	if len(g.payloads) == 0 {
		return nil, errors.New("no fixture payload")
	}
	payload := g.payloads[0]
	g.payloads = g.payloads[1:]
	return payload, nil
}

func TestAtomicMemoryApplyIsIdempotentAndConcurrent(t *testing.T) {
	payload, _ := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	created, _, err := service.Create(context.Background(), CreateChangeSetRequest{Scope: scope, Prompt: "create", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research"}}}, Placement: ChangeSetPlacement{TeamDeploymentID: "marketing-live", AgentDeploymentIDs: map[string]string{"community-researcher": "researcher-live"}, Environment: "production"}, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	ready, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, CandidateDigest: created.CandidateDigest, Allowed: true, Actor: ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow"})
	if err != nil || ready.Status != ChangeSetReady {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
	req := ApplyChangeSetRequest{Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest, Reason: "Activate approved workforce", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply"}
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan *ChangeSet, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			value, _, err := service.Apply(context.Background(), req)
			results <- value
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var receipt string
	for value := range results {
		if value.Status != ChangeSetApplied || value.ApplyReceipt == nil || len(value.ApplyReceipt.Resources) < 4 {
			t.Fatalf("applied=%#v", value)
		}
		if value.ApplyReceipt.Reason != req.Reason || value.Lifecycle[len(value.Lifecycle)-1].Reason != req.Reason {
			t.Fatalf("apply audit=%#v", value)
		}
		if receipt != "" && receipt != value.ApplyReceipt.ID {
			t.Fatalf("receipts differ")
		}
		receipt = value.ApplyReceipt.ID
	}
	changedReason := req
	changedReason.Reason = "Different activation reason"
	if _, _, err := service.Apply(context.Background(), changedReason); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("changed reason replay=%v", err)
	}
	if len(store.definitions) != 2 || len(store.deployments) != 2 {
		t.Fatalf("definitions=%d deployments=%d", len(store.definitions), len(store.deployments))
	}
	if _, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest, Reason: req.Reason, Actor: req.Actor, IdempotencyKey: "other"}); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("different retry=%v", err)
	}
}

func TestAtomicMemoryApplyRejectsIncompletePlacementWithoutPartialState(t *testing.T) {
	payload, _ := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	created, _, _ := service.Create(context.Background(), CreateChangeSetRequest{Scope: scope, Prompt: "create", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research"}}}, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create"})
	ready, _, _ := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: 1, CandidateDigest: created.CandidateDigest, Allowed: true, Actor: ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow"})
	_, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: 2, CandidateDigest: ready.CandidateDigest, Reason: "Activate approved workforce", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply"})
	if err == nil || len(store.definitions) != 0 || len(store.deployments) != 0 {
		t.Fatalf("err=%v definitions=%d deployments=%d", err, len(store.definitions), len(store.deployments))
	}
}

func TestChangeSetCanonicalizesDefinitionIdentityPerScopeBeforeApproval(t *testing.T) {
	response := GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)}
	payload, _ := json.Marshal(response)
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload, payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	create := func(scopeID, key string) *ChangeSet {
		scope := capability.ScopeReference{Kind: "tenant", ID: scopeID}
		value, _, err := service.Create(context.Background(), CreateChangeSetRequest{Scope: scope, Prompt: "create", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research"}}}, Placement: ChangeSetPlacement{TeamDeploymentID: "team-live", AgentDeploymentIDs: map[string]string{"community-researcher": "agent-live"}, Environment: "test"}, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: key})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	one, two := create("one", "one"), create("two", "two")
	if one.Result.Candidate.Agents[0].ID != "tenant/one/community-researcher" || two.Result.Candidate.Agents[0].ID != "tenant/two/community-researcher" || one.CandidateDigest == two.CandidateDigest {
		t.Fatalf("one=%s two=%s", one.Result.Candidate.Agents[0].ID, two.Result.Candidate.Agents[0].ID)
	}
	if one.Placement.AgentDeploymentIDs[one.Result.Candidate.Agents[0].ID] != "agent-live" {
		t.Fatalf("placement=%#v", one.Placement)
	}
}

func TestAtomicMemoryApplySupportsAgentWithoutTeam(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Team = nil
	candidate.Assignments = nil
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	created, _, err := service.Create(context.Background(), CreateChangeSetRequest{Scope: scope, Prompt: "agent only", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research"}}}, Placement: ChangeSetPlacement{AgentDeploymentIDs: map[string]string{"community-researcher": "agent-live"}, Environment: "test"}, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create"})
	if err != nil || !created.Result.Valid {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	ready, _, _ := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: 1, CandidateDigest: created.CandidateDigest, Allowed: true, Actor: ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow"})
	applied, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest, Reason: "Activate approved workforce", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply"})
	if err != nil || applied.Status != ChangeSetApplied {
		t.Fatalf("applied=%#v err=%v", applied, err)
	}
	for _, resource := range applied.ApplyReceipt.Resources {
		if resource.Kind == "team_definition" || resource.Kind == "team_deployment" {
			t.Fatalf("unexpected Team resource %#v", resource)
		}
	}
}

func TestChangeSetServicePersistsIdempotentImmutableCreateAndRefineLineage(t *testing.T) {
	create := GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead), Questions: []string{"Which sources are authorized?"}}
	amend := GenerationResponse{Candidate: marketingCandidate("2", capability.RiskLevelExternal)}
	createPayload, _ := jsonMarshal(create)
	amendPayload, _ := jsonMarshal(amend)
	generator := &sequenceChangeSetGenerator{payloads: [][]byte{createPayload, amendPayload}}
	compiler, _ := NewCompiler(generator)
	store := NewMemoryChangeSetStore()
	service, err := NewChangeSetService(compiler, store)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	request := CreateChangeSetRequest{
		Scope: scope, Prompt: "Create a marketing research Team", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research"}}},
		Placement: ChangeSetPlacement{TeamDeploymentID: "marketing", AgentDeploymentIDs: map[string]string{"researcher": "researcher-live"}},
		Actor:     ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create-marketing",
	}
	created, replayed, err := service.Create(context.Background(), request)
	if err != nil || replayed || created.Status != ChangeSetBlocked || created.Mode != ModeCreate || created.Revision != 1 || created.CandidateDigest == "" {
		t.Fatalf("created = %#v, replayed = %t, err = %v", created, replayed, err)
	}
	replay, replayed, err := service.Create(context.Background(), request)
	if err != nil || !replayed || replay.ID != created.ID || replay.CandidateDigest != created.CandidateDigest {
		t.Fatalf("replay = %#v, replayed = %t, err = %v", replay, replayed, err)
	}
	request.Prompt = "Different intent"
	if _, _, err := service.Create(context.Background(), request); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("idempotency conflict = %v", err)
	}

	refined, replayed, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: scope, ParentID: created.ID, Prompt: "Allow reviewed outreach", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research"}}},
		Placement: created.Placement, Actor: request.Actor, IdempotencyKey: "refine-marketing",
	})
	if err != nil || replayed || refined.Mode != ModeAmend || refined.ParentID != created.ID || refined.Status != ChangeSetReview || len(refined.Result.RiskChanges) == 0 {
		t.Fatalf("refined = %#v, replayed = %t, err = %v", refined, replayed, err)
	}
	refined.Result.Candidate.Team.DisplayName = "mutated caller copy"
	restored, err := service.Get(context.Background(), scope, refined.ID)
	if err != nil || restored.Result.Candidate.Team.DisplayName == "mutated caller copy" {
		t.Fatalf("stored candidate was mutable: %#v, err = %v", restored, err)
	}
	if _, err := service.Get(context.Background(), capability.ScopeReference{Kind: "tenant", ID: "two"}, refined.ID); !errors.Is(err, ErrChangeSetNotFound) {
		t.Fatalf("cross-scope lookup = %v", err)
	}
}

func TestChangeSetEvaluationIsScopedIdempotentAuditableAndPolicyDerived(t *testing.T) {
	payload, err := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryChangeSetStore()
	service, err := NewChangeSetService(compiler, store)
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create team",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research"}}},
		Actor:   ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create",
	})
	if err != nil || created.Status != ChangeSetReview {
		t.Fatalf("created = %#v, err = %v", created, err)
	}
	request := SubmitChangeSetEvaluationRequest{Scope: created.Scope, ChangeSetID: created.ID, ExpectedRevision: 1,
		CandidateDigest: created.CandidateDigest, Allowed: true, Actor: ChangeSetActor{Type: "policy_evaluator", ID: "atlas"},
		IdempotencyKey: "evaluation-1", Findings: []ChangeSetPolicyFinding{{PolicyID: "production", Code: "review", Message: "Human review required"}},
		ApprovalRequirements: []ChangeSetApprovalRequirement{{PolicyID: "production", Role: "workforce_admin", Count: 1}}}
	evaluated, replay, err := service.SubmitEvaluation(context.Background(), request)
	if err != nil || replay || evaluated.Status != ChangeSetAwaitingApproval || evaluated.Revision != 2 {
		t.Fatalf("evaluated = %#v replay=%t err=%v", evaluated, replay, err)
	}
	if evaluated.CandidateDigest != created.CandidateDigest || len(evaluated.Evaluations) != 1 || len(evaluated.Lifecycle) != 2 || evaluated.Lifecycle[1].Reason != "policy_requires_approval" {
		t.Fatalf("evaluation audit = %#v", evaluated)
	}
	replayed, replay, err := service.SubmitEvaluation(context.Background(), request)
	if err != nil || !replay || replayed.Revision != 2 {
		t.Fatalf("replay = %#v replay=%t err=%v", replayed, replay, err)
	}
	conflict := request
	conflict.Allowed = false
	if _, _, err := service.SubmitEvaluation(context.Background(), conflict); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	other := request
	other.Scope.ID = "two"
	if _, _, err := service.SubmitEvaluation(context.Background(), other); !errors.Is(err, ErrChangeSetNotFound) {
		t.Fatalf("cross scope = %v", err)
	}
	stale := request
	stale.IdempotencyKey = "evaluation-2"
	if _, _, err := service.SubmitEvaluation(context.Background(), stale); !errors.Is(err, ErrChangeSetRevision) {
		t.Fatalf("stale revision = %v", err)
	}
}

func TestChangeSetEvaluationCanMakeCandidateReadyOrRejectIt(t *testing.T) {
	for _, test := range []struct {
		name    string
		allowed bool
		want    ChangeSetStatus
		reason  string
	}{{"ready", true, ChangeSetReady, "policy_allowed"}, {"rejected", false, ChangeSetRejected, "policy_denied"}} {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemoryChangeSetStore()
			now := time.Now().UTC()
			value := &ChangeSet{ID: "change", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, CandidateDigest: "candidate", Status: ChangeSetReview, Revision: 1, CreatedAt: now, UpdatedAt: now}
			if _, _, err := store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
				t.Fatal(err)
			}
			service := &ChangeSetService{store: store, now: func() time.Time { return now.Add(time.Minute) }}
			updated, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{Scope: value.Scope, ChangeSetID: value.ID, ExpectedRevision: 1, CandidateDigest: "candidate", Allowed: test.allowed, Actor: ChangeSetActor{Type: "evaluator", ID: "one"}, IdempotencyKey: "eval"})
			if err != nil || updated.Status != test.want || updated.Lifecycle[0].Reason != test.reason {
				t.Fatalf("updated = %#v, err = %v", updated, err)
			}
		})
	}
}

func TestChangeSetApprovalsRequireDistinctPrincipalsAndAreAuditable(t *testing.T) {
	store := NewMemoryChangeSetStore()
	now := time.Now().UTC()
	evaluation := ChangeSetEvaluation{ID: "evaluation", CandidateDigest: "candidate", Allowed: true,
		ApprovalRequirements: []ChangeSetApprovalRequirement{{PolicyID: "production", Role: "workforce_admin", Count: 2}}}
	value := &ChangeSet{ID: "change", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, CandidateDigest: "candidate",
		Status: ChangeSetAwaitingApproval, Evaluations: []ChangeSetEvaluation{evaluation}, Revision: 2, CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	service := &ChangeSetService{store: store, now: func() time.Time { return now.Add(time.Minute) }}
	firstRequest := ResolveChangeSetApprovalRequest{Scope: value.Scope, ChangeSetID: value.ID, ExpectedRevision: 2,
		EvaluationID: evaluation.ID, PolicyID: "production", Role: "workforce_admin", Approved: true,
		Actor: ChangeSetActor{Type: "user", ID: "alice"}, IdempotencyKey: "alice-approves"}
	first, replay, err := service.ResolveApproval(context.Background(), firstRequest)
	if err != nil || replay || first.Status != ChangeSetAwaitingApproval || first.Revision != 3 || len(first.ApprovalDecisions) != 1 || first.Lifecycle[0].Reason != "approval_recorded" {
		t.Fatalf("first approval = %#v replay=%t err=%v", first, replay, err)
	}
	replayed, replay, err := service.ResolveApproval(context.Background(), firstRequest)
	if err != nil || !replay || replayed.Revision != 3 {
		t.Fatalf("approval replay = %#v replay=%t err=%v", replayed, replay, err)
	}
	duplicatePrincipal := firstRequest
	duplicatePrincipal.ExpectedRevision = 3
	duplicatePrincipal.IdempotencyKey = "alice-again"
	if _, _, err := service.ResolveApproval(context.Background(), duplicatePrincipal); !errors.Is(err, ErrChangeSetTransition) {
		t.Fatalf("duplicate principal = %v", err)
	}
	secondRequest := firstRequest
	secondRequest.ExpectedRevision = 3
	secondRequest.Actor.ID = "bob"
	secondRequest.IdempotencyKey = "bob-approves"
	ready, replay, err := service.ResolveApproval(context.Background(), secondRequest)
	if err != nil || replay || ready.Status != ChangeSetReady || ready.Revision != 4 || len(ready.ApprovalDecisions) != 2 || ready.Lifecycle[1].Reason != "approvals_satisfied" {
		t.Fatalf("ready = %#v replay=%t err=%v", ready, replay, err)
	}
	stale := secondRequest
	stale.Actor.ID = "carol"
	stale.IdempotencyKey = "carol-stale"
	if _, _, err := service.ResolveApproval(context.Background(), stale); !errors.Is(err, ErrChangeSetRevision) {
		t.Fatalf("stale approval = %v", err)
	}
}

func TestChangeSetApprovalRejectionFailsClosed(t *testing.T) {
	store := NewMemoryChangeSetStore()
	now := time.Now().UTC()
	evaluation := ChangeSetEvaluation{ID: "evaluation", CandidateDigest: "candidate", Allowed: true,
		ApprovalRequirements: []ChangeSetApprovalRequirement{{PolicyID: "production", Role: "workforce_admin", Count: 1}}}
	value := &ChangeSet{ID: "change", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, CandidateDigest: "candidate",
		Status: ChangeSetAwaitingApproval, Evaluations: []ChangeSetEvaluation{evaluation}, Revision: 2, CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	service := &ChangeSetService{store: store, now: func() time.Time { return now.Add(time.Minute) }}
	rejected, _, err := service.ResolveApproval(context.Background(), ResolveChangeSetApprovalRequest{Scope: value.Scope, ChangeSetID: value.ID, ExpectedRevision: 2,
		EvaluationID: evaluation.ID, PolicyID: "production", Role: "workforce_admin", Approved: false, Reason: "insufficient controls",
		Actor: ChangeSetActor{Type: "user", ID: "alice"}, IdempotencyKey: "reject"})
	if err != nil || rejected.Status != ChangeSetRejected || rejected.Lifecycle[0].Reason != "approval_rejected" || rejected.ApprovalDecisions[0].Reason != "insufficient controls" {
		t.Fatalf("rejected = %#v err=%v", rejected, err)
	}
}

func jsonMarshal(value interface{}) ([]byte, error) {
	return json.Marshal(value)
}
