package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestSQLiteRefinementIntentAndAnswerSurviveRestartAndGenerationCAS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	value := testWorkforceChangeSet(testScope("refinement"), "change-refinement")
	if _, _, err = store.CreateChangeSet(context.Background(), value, "create-refinement", "create-digest"); err != nil {
		t.Fatal(err)
	}

	refining := cloneRuntimeChangeSet(value)
	refining.Status, refining.Revision = authoring.ChangeSetEvaluating, 2
	refining.Generation = &authoring.ChangeSetGeneration{
		PreviousCandidateDigest: value.CandidateDigest,
		Request: authoring.GenerateRequest{Mode: authoring.ModeAmend, Prompt: value.Prompt, Refinement: &authoring.RefinementContext{
			Questions: []authoring.RefinementQuestion{{
				ID: "scope", Category: authoring.RefinementCategoryScope, Prompt: "Which sources?", WhyNeeded: "Source scope is required.",
				Blocking: []authoring.RefinementBlockingScope{authoring.RefinementBlocksCandidate}, Answer: authoring.RefinementAnswerSchema{Kind: authoring.RefinementAnswerStringList, Minimum: 1, Maximum: 10},
				Priority: 10, Provenance: []authoring.RefinementQuestionProvenance{{Kind: authoring.RefinementProvenancePrompt}},
			}},
			Answers: []authoring.RefinementResolvedAnswer{{QuestionID: "scope", Value: authoring.RefinementProviderAnswerValue{Items: []string{"one"}}, Source: authoring.RefinementAnswerSourceUser}},
		}},
	}
	refining.Refinement = authoring.ChangeSetRefinement{
		Questions: refining.Generation.Request.Refinement.Questions,
		Answers:   []authoring.RefinementAnswerEvent{{ID: "answer", IdempotencyKey: "answer-scope", RequestDigest: "answer-digest", QuestionID: "scope", QuestionRevision: 1, Value: authoring.RefinementAnswerValue{Items: []string{"one"}}, Source: authoring.RefinementAnswerSourceUser, Actor: value.Actor, AnsweredAt: value.UpdatedAt}},
	}
	if _, err = store.UpdateChangeSet(context.Background(), refining, 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.GetChangeSet(context.Background(), value.Scope, value.ID)
	if err != nil || restored.Status != authoring.ChangeSetEvaluating || len(restored.Refinement.Answers) != 1 || restored.Generation.Request.Refinement.Answers[0].Value.Items[0] != "one" {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
	completed := cloneRuntimeChangeSet(restored)
	completed.Status, completed.Revision, completed.CandidateDigest = authoring.ChangeSetReview, 3, "candidate-refined"
	if _, err = restarted.CompleteChangeSetGeneration(context.Background(), completed, 2); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Close(); err != nil {
		t.Fatal(err)
	}

	verified, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	result, err := verified.GetChangeSet(context.Background(), value.Scope, value.ID)
	if err != nil || result.CandidateDigest != "candidate-refined" || result.Revision != 3 || len(result.Refinement.Answers) != 1 {
		t.Fatalf("verified=%#v err=%v", result, err)
	}
}

func testScope(id string) capability.ScopeReference {
	return capability.ScopeReference{Kind: "tenant", ID: id}
}
