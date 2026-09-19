package authoring

import (
	"context"
	"errors"
	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/skill"
	"strings"
	"testing"
)

func TestDraftAgentNamePersistsAcrossGenerationRenameAndAmendment(t *testing.T) {
	intent := AuthoringIntent{SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent, Name: "Marginalia", Purpose: "Review literature", Agents: []AuthoringAgentIntent{{Key: "reader", Name: "Marginalia", Purpose: "Review literature", Behavior: "Compare supplied papers accurately."}}}
	compiler, _ := NewCompiler(semanticIntentGenerator{intent: intent})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	request := CreateChangeSetRequest{Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create a literature reviewer", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "name-create"}
	prepared, _, err := service.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := service.GeneratePrepared(t.Context(), prepared.Scope, prepared.ID, prepared.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if generated.Result.Candidate.Agents[0].DisplayName != "Marginalia" {
		t.Fatal("model name was replaced")
	}
	renamed, err := service.RenameAgent(t.Context(), generated.Scope, generated.ID, generated.Revision, "Paper Trail", request.Actor)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.CandidateDigest == generated.CandidateDigest || renamed.Result.Candidate.Agents[0].ID != generated.Result.Candidate.Agents[0].ID || renamed.Status != ChangeSetReview {
		t.Fatal("rename did not preserve identity and invalidate review")
	}
	restored, err := service.Get(t.Context(), renamed.Scope, renamed.ID)
	if err != nil || restored.AgentName != "Paper Trail" {
		t.Fatal("name did not persist", err)
	}
	replay, err := service.RenameAgent(t.Context(), renamed.Scope, renamed.ID, generated.Revision, "Paper Trail", request.Actor)
	if err != nil || replay.Revision != renamed.Revision {
		t.Fatal("rename retry duplicated mutation", err)
	}
	if _, err := service.RenameAgent(t.Context(), renamed.Scope, renamed.ID, generated.Revision, "Different", request.Actor); !errors.Is(err, ErrChangeSetRevision) {
		t.Fatal("stale rename accepted", err)
	}
	if _, err := service.RenameAgent(t.Context(), renamed.Scope, renamed.ID, renamed.Revision, "Other", ChangeSetActor{Type: "user", ID: "8"}); err == nil {
		t.Fatal("foreign creator renamed draft")
	}
	if _, err := service.RenameAgent(t.Context(), skill.ScopeReference{Kind: "tenant", ID: "two"}, renamed.ID, renamed.Revision, "Other", request.Actor); !errors.Is(err, ErrChangeSetNotFound) {
		t.Fatal("foreign tenant read draft", err)
	}
	request.ParentID, request.IdempotencyKey = renamed.ID, "name-amend"
	child, _, err := service.Prepare(t.Context(), request)
	if err != nil || child.AgentName != "Paper Trail" {
		t.Fatal("amendment lost explicit name", err)
	}
	completed, err := service.GeneratePrepared(t.Context(), child.Scope, child.ID, child.Revision)
	if err != nil || completed.Result.Candidate.Agents[0].DisplayName != "Paper Trail" {
		t.Fatal("LLM overwrote explicit name", err)
	}
}

func TestDraftAgentNameIsValidatedAndIncludedInIdempotency(t *testing.T) {
	compiler, _ := NewCompiler(semanticIntentGenerator{})
	service, _ := NewChangeSetService(compiler, NewMemoryChangeSetStore())
	req := CreateChangeSetRequest{Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Review papers", AgentName: "Paper Trail", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "name"}
	first, _, err := service.Prepare(t.Context(), req)
	if err != nil || first.AgentName != req.AgentName {
		t.Fatal(err)
	}
	req.AgentName = "Different"
	if _, _, err := service.Prepare(t.Context(), req); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatal("conflicting name replay accepted", err)
	}
	if err := validateDraftAgentName("Bad\nName", false); err == nil {
		t.Fatal("control characters allowed")
	}
}

type namingRepairGenerator struct {
	calls  int
	refuse bool
}

func (g *namingRepairGenerator) GenerateIntent(_ context.Context, req GenerateRequest) (AuthoringIntent, error) {
	return AuthoringIntent{SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent, Name: "Marginalia", Purpose: "Review literature", Agents: []AuthoringAgentIntent{{Key: "reader", Name: "Marginalia", Purpose: "Review literature", Behavior: "Compare supplied papers accurately."}}}, nil
}
func (g *namingRepairGenerator) RepairIntent(_ context.Context, req GenerateRequest, previous AuthoringIntent, reason error) (AuthoringIntent, error) {
	g.calls++
	if len(req.ExistingAgentNames) != 1 || !strings.Contains(reason.Error(), "agent_name_in_use") {
		return previous, errors.New("missing collision context")
	}
	if !g.refuse {
		previous.Name = "Paper Trail"
		previous.Agents[0].Name = "Paper Trail"
	}
	return previous, nil
}
func TestAgentNameCollisionIsRepairedByModel(t *testing.T) {
	generator := &namingRepairGenerator{}
	compiler, _ := NewCompiler(generator)
	request := GenerateRequest{Mode: ModeCreate, Prompt: "Create a literature reviewer", ExistingAgentNames: []string{"  MARGINALIA  "}}
	projected := promptGenerateRequest(request)
	if len(projected.ExistingAgentNames) != 1 {
		t.Fatal("existing names missing from model context")
	}
	result, err := compiler.Compile(t.Context(), request)
	if err != nil || !result.Valid || generator.calls != 1 || result.Candidate.Agents[0].DisplayName != "Paper Trail" {
		t.Fatalf("collision repair failed: calls=%d result=%#v err=%v", generator.calls, result, err)
	}
	generator.refuse = true
	generator.calls = 0
	result, err = compiler.Compile(t.Context(), request)
	if err == nil && result.Valid {
		t.Fatal("duplicate name accepted after failed repairs")
	}
	if generator.calls > maximumRepairAttempts {
		t.Fatal("unbounded naming repairs")
	}
}
func TestDraftNameRejectsPublishedActivationAndMalformedCandidates(t *testing.T) {
	current := &ChangeSet{Mode: ModeAmend, Status: ChangeSetReview, ParentID: "published", Result: CompileResult{Diff: []FieldDiff{{Path: "activation"}}, Candidate: WorkforceCandidate{Activation: WorkforceActivationActive, Agents: []*agent.AgentDefinition{{ID: "reader", Version: "1"}}}}}
	if DraftAgentNameEditable(current) {
		t.Fatal("published activation draft permits rename")
	}
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{nil}}
	applyDraftAgentName(&candidate, "Safe")
	current.Result.Candidate = candidate
	if DraftAgentNameEditable(current) {
		t.Fatal("nil candidate permits rename")
	}
}

func TestNamingContextSurvivesRefinement(t *testing.T) {
	question := RefinementQuestion{ID: "audience", Category: RefinementCategoryScope, Prompt: "Who is the audience?", WhyNeeded: "Choose useful detail", Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerText}, Priority: 100, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}}}
	generator := &refinementGenerator{payloads: [][]byte{refinementPayload(t, "1", question)}}
	compiler, _ := NewCompiler(generator)
	service, _ := NewChangeSetService(compiler, NewMemoryChangeSetStore())
	req := CreateChangeSetRequest{Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Help with marketing", AgentName: "Fresh Perspective", ExistingAgentNames: []string{"Marginalia"}, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "refine-name"}
	initial, _, err := service.Create(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	answered, _, err := service.AnswerRefinement(t.Context(), AnswerChangeSetRefinementRequest{Scope: req.Scope, ChangeSetID: initial.ID, ExpectedRevision: initial.Revision, QuestionID: question.ID, Value: RefinementAnswerValue{Text: "Readers"}, Actor: req.Actor, IdempotencyKey: "name-answer"})
	if err != nil {
		t.Fatal(err)
	}
	next := answered.Generation.Request
	if next.AgentName != req.AgentName || len(next.ExistingAgentNames) != 1 || next.ExistingAgentNames[0] != "Marginalia" {
		t.Fatal("refinement lost naming context", next.AgentName, next.ExistingAgentNames)
	}
}
