package authoring

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestCompilerRepairsAliasedSkillQuestionOptionsToExactCatalogKeys(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	invalidQuestion := RefinementQuestion{
		ID: "report-skills", Category: RefinementCategorySkill, Prompt: "Which report capabilities should be used?",
		WhyNeeded: "The requested report needs document generation and delivery.", Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer: RefinementAnswerSchema{Kind: RefinementAnswerSkillSelection, Minimum: 1, Maximum: 2, Options: []RefinementQuestionOption{
			{ID: "document", Label: "Document"}, {ID: "delivery", Label: "Delivery"},
		}}, Priority: 100, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	repairedQuestion := invalidQuestion
	repairedQuestion.Answer.Options = []RefinementQuestionOption{
		{ID: "openseal.document", Label: "Document", Description: "Ready and compatible with PDF generation."},
		{ID: "openseal.delivery", Label: "Delivery", Description: "Requires a delivery binding; email delivery is compatible."},
	}
	generated, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{invalidQuestion}})
	repaired, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{repairedQuestion}})
	generator := &repairingGenerator{generated: generated, repaired: repaired}
	compiler, _ := NewCompiler(generator)
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"reddit-research":   {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}, Readiness: SkillReadinessReady},
		"openseal.document": {ID: "openseal.document", Version: "1.0.0", Actions: []string{"render_pdf"}, Readiness: SkillReadinessReady, Compatibility: []SkillCompatibility{{Requirement: "pdf", Compatible: true, Evidence: "native renderer"}}},
		"openseal.delivery": {ID: "openseal.delivery", Version: "1.0.0", Actions: []string{"send_email"}, Readiness: SkillReadinessNeedsBinding, Compatibility: []SkillCompatibility{{Requirement: "email", Compatible: true, Evidence: "host adapter"}}},
	}}

	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a research Team that delivers a PDF report.", Catalog: catalog})
	if err != nil || generator.repairs != 1 || len(result.UnresolvedQuestions) != 1 || len(result.Validation) != 0 {
		t.Fatalf("Skill question repair result=%#v repairs=%d err=%v", result, generator.repairs, err)
	}
	if got := generator.lastError.Error(); !strings.Contains(got, `non-canonical Skill option id \"document\"`) || !strings.Contains(got, `exact authorized catalog key \"openseal.document\"`) || !strings.Contains(got, "aliases are not accepted") {
		t.Fatalf("repair diagnostic=%s", got)
	}
	options := result.UnresolvedQuestions[0].Answer.Options
	if options[0].ID != "openseal.document" || options[1].ID != "openseal.delivery" || options[0].Description == "" || options[1].Description == "" {
		t.Fatalf("repaired Skill options=%#v", options)
	}
}
