package authoring

import (
	"context"
	"encoding/json"
	"errors"
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

func jsonMarshal(value interface{}) ([]byte, error) {
	return json.Marshal(value)
}
