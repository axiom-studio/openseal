package openseal

import (
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestEngineAnswersRefinementThroughCanonicalDurableRun(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := New(WithPersistentStore(store), WithWorkforceAuthoringGenerator(workforceFixtureGenerator{}))
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := engine.CreateWorkforceChangeSet(t.Context(), CreateWorkforceChangeSetRequest{
		Scope: SkillScope{Kind: "workspace", ID: "local"}, Prompt: "Create a Team", Actor: WorkforceChangeSetActor{Type: "user", ID: "local"}, IdempotencyKey: "create-refinement",
	})
	if err != nil || created.Status != WorkforceChangeSetBlocked || created.Refinement.NextQuestion() == nil {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	question := created.Refinement.NextQuestion()
	request := AnswerWorkforceChangeSetRefinementRequest{
		Scope: created.Scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, QuestionID: question.ID,
		Value: WorkforceRefinementAnswerValue{Text: "Own product research"}, Actor: WorkforceChangeSetActor{Type: "user", ID: "local"}, IdempotencyKey: "answer-refinement",
	}
	answered, replayed, err := engine.AnswerWorkforceChangeSetRefinement(t.Context(), request)
	if err != nil || replayed || answered.Status != WorkforceChangeSetEvaluating || answered.Generation == nil || answered.Generation.RunID == "" || len(answered.Refinement.Answers) != 1 {
		t.Fatalf("answered=%#v replayed=%t err=%v", answered, replayed, err)
	}
	replay, replayed, err := engine.AnswerWorkforceChangeSetRefinement(t.Context(), request)
	if err != nil || !replayed || replay.ID != answered.ID || replay.Generation.RunID != answered.Generation.RunID || len(replay.Refinement.Answers) != 1 {
		t.Fatalf("replay=%#v replayed=%t err=%v", replay, replayed, err)
	}
}
