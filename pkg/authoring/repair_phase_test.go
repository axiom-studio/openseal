package authoring

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestCompilerSeparatelyRepairsSchemaThenRefinementContract(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	invalidQuestion := repairPhaseSkillQuestion("")
	validQuestion := repairPhaseSkillQuestion(RefinementAnswerSkillSelection)
	semanticInvalid, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{invalidQuestion}})
	semanticValid, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{validQuestion}})
	generator := &repairingGenerator{
		generated:      []byte(`{"unknown":true}`),
		repairSequence: [][]byte{semanticInvalid, semanticValid},
	}
	compiler, _ := NewCompiler(generator)

	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a research Team and ask which reporting Skill to use.",
		InvocationKey: "change-set:81631653:0", Catalog: repairPhaseCatalog(),
	})
	if err != nil || generator.repairs != 2 || len(result.Validation) != 0 || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("result=%#v repairs=%d err=%v", result, generator.repairs, err)
	}
	if got := result.UnresolvedQuestions[0].Answer.Kind; got != RefinementAnswerSkillSelection {
		t.Fatalf("answer kind=%q", got)
	}
	wantKeys := []string{"change-set:81631653:0:schema:1", "change-set:81631653:0:contract:1"}
	if len(generator.repairInvocationKeys) != len(wantKeys) || generator.repairInvocationKeys[0] != wantKeys[0] || generator.repairInvocationKeys[1] != wantKeys[1] {
		t.Fatalf("repair invocation keys=%#v want=%#v", generator.repairInvocationKeys, wantKeys)
	}
}

func TestChangeSetDoesNotExposeQuestionThatFailsBoundedContractRepair(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	invalidQuestion := repairPhaseSkillQuestion("")
	semanticInvalid, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{invalidQuestion}})
	generator := &repairingGenerator{
		generated:      []byte(`{"unknown":true}`),
		repairSequence: [][]byte{semanticInvalid, semanticInvalid},
	}
	compiler, _ := NewCompiler(generator)
	service, _ := NewChangeSetService(compiler, NewMemoryChangeSetStore())

	created, replay, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create a research Team and ask which reporting Skill to use.",
		Catalog: repairPhaseCatalog(), Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "invalid-after-contract-repair",
	})
	if err != nil || replay || generator.repairs != 2 || created.Status != ChangeSetBlocked {
		t.Fatalf("created=%#v replay=%t repairs=%d err=%v", created, replay, generator.repairs, err)
	}
	if !hasValidationCode(created.Result.Validation, "invalid_refinement_question") {
		t.Fatalf("validation=%#v", created.Result.Validation)
	}
	if len(created.Refinement.Questions) != 0 || created.Refinement.NextQuestion() != nil {
		t.Fatalf("invalid question became actionable: %#v", created.Refinement)
	}
}

func repairPhaseSkillQuestion(kind RefinementAnswerKind) RefinementQuestion {
	return RefinementQuestion{
		ID: "report-skill", Category: RefinementCategorySkill, Prompt: "Which reporting Skill should be used?",
		WhyNeeded: "Report generation requires an authorized Skill.", Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer:   RefinementAnswerSchema{Kind: kind, Minimum: 1, Maximum: 1, Options: []RefinementQuestionOption{{ID: "openseal.document", Label: "Document"}}},
		Priority: 100, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
}

func repairPhaseCatalog() CapabilityCatalog {
	return CapabilityCatalog{Skills: map[string]SkillCapability{
		"reddit-research":   {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}, Readiness: SkillReadinessReady},
		"openseal.document": {ID: "openseal.document", Version: "1.0.0", Actions: []string{"render_pdf"}, Readiness: SkillReadinessReady},
	}}
}
