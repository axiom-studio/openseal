package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type heldCancellationGenerator struct {
	started chan struct{}
	release chan struct{}
	payload []byte
}

func (g *heldCancellationGenerator) Generate(context.Context, GenerateRequest) ([]byte, error) {
	close(g.started)
	<-g.release
	return g.payload, nil
}

func TestCancelPreparedGenerationFencesLateProviderResult(t *testing.T) {
	payload, _ := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	g := &heldCancellationGenerator{started: make(chan struct{}), release: make(chan struct{}), payload: payload}
	compiler, _ := NewCompiler(g)
	s, _ := NewChangeSetService(compiler, NewMemoryChangeSetStore())
	actor := ChangeSetActor{Type: "user", ID: "7"}
	prepared, _, err := s.Prepare(t.Context(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "preserve this prompt",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}},
		Actor:   actor, IdempotencyKey: "cancel-race",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.GeneratePrepared(t.Context(), prepared.Scope, prepared.ID, prepared.Revision)
		result <- err
	}()
	<-g.started
	stopped, err := s.CancelPreparedGeneration(t.Context(), prepared.Scope, prepared.ID, prepared.Revision, actor)
	close(g.release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrChangeSetRevision) {
		t.Fatalf("late result error=%v", err)
	}
	current, err := s.Get(t.Context(), prepared.Scope, prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != stopped.Revision || current.Status != ChangeSetFailed || current.Generation.FailureCode != "canceled" || current.CandidateDigest != "" {
		t.Fatalf("late result changed canceled generation: %#v", current)
	}
	if current.Generation.Request.Prompt != "preserve this prompt" {
		t.Fatal("original prompt lost")
	}
	if event := current.Lifecycle[len(current.Lifecycle)-1]; event.Actor != actor || event.Reason != "candidate_generation_canceled" {
		t.Fatalf("audit=%#v", event)
	}
	if _, err := s.CancelPreparedGeneration(t.Context(), capability.ScopeReference{Kind: "tenant", ID: "other"}, prepared.ID, current.Revision, actor); !errors.Is(err, ErrChangeSetNotFound) {
		t.Fatalf("foreign tenant cancellation=%v", err)
	}
	if _, err := s.CancelPreparedGeneration(t.Context(), prepared.Scope, prepared.ID, prepared.Revision, actor); !errors.Is(err, ErrChangeSetRevision) {
		t.Fatalf("stale cancellation=%v", err)
	}
	if _, err := s.CancelPreparedGeneration(t.Context(), prepared.Scope, prepared.ID, current.Revision, actor); !errors.Is(err, ErrChangeSetTransition) {
		t.Fatalf("terminal cancellation=%v", err)
	}
}
