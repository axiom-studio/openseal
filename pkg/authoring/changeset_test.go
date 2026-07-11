package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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

func jsonMarshal(value interface{}) ([]byte, error) {
	return json.Marshal(value)
}
