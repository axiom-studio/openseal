package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	wantKeys := []string{"change-set:81631653:0:repair:1", "change-set:81631653:0:repair:2"}
	if len(generator.repairInvocationKeys) != len(wantKeys) || generator.repairInvocationKeys[0] != wantKeys[0] || generator.repairInvocationKeys[1] != wantKeys[1] {
		t.Fatalf("repair invocation keys=%#v want=%#v", generator.repairInvocationKeys, wantKeys)
	}
}

func TestCompilerRevalidatesAndRepairsStillInvalidSemanticResponse(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	invalidQuestion := repairPhaseSkillQuestion("")
	validQuestion := repairPhaseSkillQuestion(RefinementAnswerSkillSelection)
	semanticInvalid, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{invalidQuestion}})
	semanticValid, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{validQuestion}})
	generator := &repairingGenerator{generated: semanticInvalid, repairSequence: [][]byte{semanticInvalid, semanticValid}}
	compiler, _ := NewCompiler(generator)

	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a research Team and ask which reporting Skill to use.",
		InvocationKey: "change-set:01548b54:0", Catalog: repairPhaseCatalog(),
	})
	if err != nil || generator.repairs != 2 || len(result.UnresolvedQuestions) != 1 || result.UnresolvedQuestions[0].Answer.Kind != RefinementAnswerSkillSelection {
		t.Fatalf("result=%#v repairs=%d err=%v", result, generator.repairs, err)
	}
	wantKeys := []string{"change-set:01548b54:0:repair:1", "change-set:01548b54:0:repair:2"}
	if len(generator.repairInvocationKeys) != 2 || generator.repairInvocationKeys[0] != wantKeys[0] || generator.repairInvocationKeys[1] != wantKeys[1] {
		t.Fatalf("repair invocation keys=%#v want=%#v", generator.repairInvocationKeys, wantKeys)
	}
}

func TestCompilerRepairsPlaintextCredentialQuestionIntoOpaqueReference(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	invalidQuestion := RefinementQuestion{
		ID: "reddit-credential", Category: RefinementCategoryCredential,
		Prompt: "Provide the Reddit API client secret.", WhyNeeded: "Reddit access requires authorization.",
		Blocking: []RefinementBlockingScope{RefinementBlocksApply},
		Answer:   RefinementAnswerSchema{Kind: RefinementAnswerText},
		Priority: 100, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCredential}},
	}
	validQuestion := invalidQuestion
	validQuestion.Prompt = "Which authorized Reddit credential reference should be bound?"
	validQuestion.Answer.Kind = RefinementAnswerCredentialReference
	semanticInvalid, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{invalidQuestion}})
	semanticValid, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{validQuestion}})
	generator := &repairingGenerator{generated: semanticInvalid, repairSequence: [][]byte{semanticValid}}
	compiler, _ := NewCompiler(generator)

	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a Reddit research Agent.", InvocationKey: "change-set:credential:0",
		Catalog: repairPhaseCatalog(),
	})
	if err != nil || generator.repairs != 1 || len(result.UnresolvedQuestions) != 1 {
		t.Fatalf("result=%#v repairs=%d err=%v", result, generator.repairs, err)
	}
	question := result.UnresolvedQuestions[0]
	if question.Answer.Kind != RefinementAnswerCredentialReference || strings.Contains(strings.ToLower(question.Prompt), "secret") {
		t.Fatalf("credential question=%#v", question)
	}
}

func TestCompilerRejectsQuestionThatFailsBoundedContractRepairBeforePersistence(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	invalidQuestion := repairPhaseSkillQuestion("")
	semanticInvalid, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{invalidQuestion}})
	generator := &repairingGenerator{
		generated:      []byte(`{"unknown":true}`),
		repairSequence: [][]byte{semanticInvalid, semanticInvalid, semanticInvalid},
	}
	compiler, _ := NewCompiler(generator)
	service, _ := NewChangeSetService(compiler, NewMemoryChangeSetStore())

	created, replay, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create a research Team and ask which reporting Skill to use.",
		Catalog: repairPhaseCatalog(), Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "invalid-after-contract-repair",
	})
	var schemaError *SchemaGenerationError
	if !errors.As(err, &schemaError) || replay || created != nil || generator.repairs != 2 {
		t.Fatalf("created=%#v replay=%t repairs=%d err=%v", created, replay, generator.repairs, err)
	}
	if schemaError.RepairAttempts != 2 || !strings.Contains(schemaError.Diagnostic, "/unresolvedQuestions/0/answer/kind") || !strings.Contains(schemaError.Diagnostic, "value must be one of") {
		t.Fatalf("schema error=%#v", schemaError)
	}
}

func TestPreparedGenerationPersistsFailureWithoutInvalidRefinementPayload(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	invalidQuestion := repairPhaseSkillQuestion("")
	semanticInvalid, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{invalidQuestion}})
	generator := &repairingGenerator{
		generated:      []byte(`{"unknown":true}`),
		repairSequence: [][]byte{semanticInvalid, semanticInvalid, semanticInvalid},
	}
	compiler, _ := NewCompiler(generator)
	service, _ := NewChangeSetService(compiler, NewMemoryChangeSetStore())
	prepared, _, err := service.Prepare(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create a research Team and ask which reporting Skill to use.",
		Catalog: repairPhaseCatalog(), Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "prepared-invalid-contract",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.GeneratePrepared(context.Background(), prepared.Scope, prepared.ID, prepared.Revision)
	var schemaError *SchemaGenerationError
	if !errors.As(err, &schemaError) || failed == nil || failed.Status != ChangeSetFailed || failed.Generation.FailureCode != "schema_failed" {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
	if failed.Generation.LastError != "We couldn't finish this proposal automatically. Your request and answers are saved; try again." || strings.Contains(failed.Generation.LastError, "invalid_refinement_question") {
		t.Fatalf("public failure leaked diagnostics: %q", failed.Generation.LastError)
	}
	if len(failed.Result.UnresolvedQuestions) != 0 || len(failed.Refinement.Questions) != 0 {
		t.Fatalf("invalid refinement payload persisted: result=%#v refinement=%#v", failed.Result.UnresolvedQuestions, failed.Refinement.Questions)
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
