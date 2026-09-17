package daemon

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/artifact"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func fileOutput(files ...generatedFile) map[string]interface{} {
	return map[string]interface{}{"reply": "Prepared the requested files.", "generatedFiles": files}
}

func TestGeneratedFilesValidateBeforePublication(t *testing.T) {
	valid := generatedFile{Name: "report.md", MediaType: "text/markdown", Text: "# Report\nUncertainty is explicit."}
	for _, tc := range []struct {
		name   string
		output map[string]interface{}
		status runtime.AgentRunStatus
		ok     bool
	}{
		{"valid", fileOutput(valid), runtime.AgentRunStatusCompleted, true},
		{"empty file", fileOutput(generatedFile{Name: "empty.txt", MediaType: "text/plain"}), runtime.AgentRunStatusCompleted, true},
		{"unfinished", fileOutput(valid), runtime.AgentRunStatusRunning, false},
		{"duplicate", fileOutput(valid, valid), runtime.AgentRunStatusCompleted, false},
		{"directory", fileOutput(generatedFile{Name: "../report.md", MediaType: valid.MediaType}), runtime.AgentRunStatusCompleted, false},
		{"unsupported", fileOutput(generatedFile{Name: "app.html", MediaType: "text/html"}), runtime.AgentRunStatusCompleted, false},
		{"invalid json", fileOutput(generatedFile{Name: "data.json", MediaType: "application/json", Text: "not json"}), runtime.AgentRunStatusCompleted, false},
		{"size", fileOutput(generatedFile{Name: "large.txt", MediaType: "text/plain", Text: strings.Repeat("a", maxGeneratedFileBytes+1)}), runtime.AgentRunStatusCompleted, false},
		{"combined size", fileOutput(generatedFile{Name: "a.txt", MediaType: "text/plain", Text: strings.Repeat("a", maxGeneratedFileBytes)}, valid), runtime.AgentRunStatusCompleted, false},
		{"too many", fileOutput(make([]generatedFile, 9)...), runtime.AgentRunStatusCompleted, false},
		{"foreign metadata", map[string]interface{}{"generatedFiles": []interface{}{map[string]interface{}{"name": "a.txt", "mediaType": "text/plain", "text": "hello", "scope": "other"}}}, runtime.AgentRunStatusCompleted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := generatedFiles(tc.output, tc.status)
			if (err == nil) != tc.ok {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}

type failSecondWrite struct {
	runtime.ArtifactContentStore
	writes int
}

func (s *failSecondWrite) Put(ctx context.Context, write runtime.ArtifactContentWrite) (runtime.ArtifactStoredContent, error) {
	s.writes++
	if s.writes == 2 {
		return runtime.ArtifactStoredContent{}, errors.New("simulated disk interruption")
	}
	return s.ArtifactContentStore.Put(ctx, write)
}

func TestAcceptedGeneratedFilesRecoverWithoutModelReinvocation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	store, err := runtime.NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	content, err := artifact.NewLocalStore(filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	scope := runtime.Scope{Kind: "local", ID: "default"}
	run, err := runtime.NewPortfolioService(store).CreateAgentRun(ctx, runtime.CreateAgentRunRequest{Scope: scope, Owner: runtime.ObjectiveOwner{Type: "agent", ID: "analyst"}, AssignedAgentID: "analyst", Goal: "Create two files", Source: runtime.RunSourceManual})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner := runtime.TurnRunnerFunc(func(context.Context, runtime.TurnExecutionContext) (*runtime.TurnOutcome, error) {
		calls++
		return &runtime.TurnOutcome{NextRunStatus: runtime.AgentRunStatusCompleted, RunOutput: fileOutput(generatedFile{Name: "report.md", MediaType: "text/markdown", Text: "# Report"}, generatedFile{Name: "facts.json", MediaType: "application/json", Text: `{"count":2}`})}, nil
	})
	publisher := &TaskArtifactPublisher{Scope: scope, Content: &failSecondWrite{ArtifactContentStore: content}, Catalog: runtime.NewArtifactCatalog(store)}
	_, err = runtime.NewTurnCoordinator(store, store, store).Advance(ctx, runtime.AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "first", OutputPublisher: publisher}, runner)
	if err == nil {
		t.Fatal("expected interrupted publication")
	}
	saved, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil || saved.Status != runtime.AgentRunStatusPaused {
		t.Fatalf("premature completion: %+v %v", saved, err)
	}
	first, err := runtime.NewArtifactCatalog(store).List(ctx, runtime.ArtifactFilter{Scope: scope, ProducerRunID: run.ID})
	if err != nil || len(first) != 1 {
		t.Fatalf("partial publication: %d %v", len(first), err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := runtime.NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	publisher = &TaskArtifactPublisher{Scope: scope, Content: content, Catalog: runtime.NewArtifactCatalog(reopened)}
	_, err = runtime.NewRunCommandService(reopened).CommandAgentRun(ctx, runtime.AgentRunCommandRequest{Scope: scope, RunID: run.ID, ExpectedRevision: saved.Revision, Kind: runtime.AgentRunCommandResume, Actor: runtime.ActivityActor{Type: "user", ID: "local-operator"}})
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.NewTurnCoordinator(reopened, reopened, reopened).Advance(ctx, runtime.AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "recovery", OutputPublisher: publisher}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !result.Reconciled || result.Run.Status != runtime.AgentRunStatusCompleted {
		t.Fatalf("recovery: calls=%d result=%+v", calls, result)
	}
	if _, found := result.Run.Output["generatedFiles"]; found {
		t.Fatal("run output must hold references instead of file bodies")
	}
	records, err := publisher.Catalog.List(ctx, runtime.ArtifactFilter{Scope: scope, ProducerRunID: run.ID})
	if err != nil || len(records) != 2 {
		t.Fatalf("records: %d %v", len(records), err)
	}
	for _, record := range records {
		if record.Provenance.Producer.ID != "analyst" || record.Provenance.TurnID != result.Turn.ID || record.Classification != runtime.ArtifactClassificationInternal {
			t.Fatalf("forged provenance: %+v", record)
		}
		reader, err := content.Open(ctx, scope, record.ContentRef)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || int64(len(data)) != record.SizeBytes {
			t.Fatal("missing content", err)
		}
	}
	if _, err = publisher.PublishTurnOutput(ctx, result.Run, result.Turn); err != nil {
		t.Fatal("publication replay", err)
	}
	records, _ = publisher.Catalog.List(ctx, runtime.ArtifactFilter{Scope: scope, ProducerRunID: run.ID})
	if len(records) != 2 {
		t.Fatal("duplicate artifacts on replay")
	}
	foreign := *result.Run
	foreign.Scope.ID = "other"
	if _, err = publisher.PublishTurnOutput(ctx, &foreign, result.Turn); err == nil {
		t.Fatal("cross-scope publication accepted")
	}
}

func TestCanceledProviderTurnDoesNotPublishGeneratedFiles(t *testing.T) {
	ctx := context.Background()
	store := runtime.NewMemoryStore()
	scope := runtime.Scope{Kind: "local", ID: "default"}
	run, err := runtime.NewPortfolioService(store).CreateAgentRun(ctx, runtime.CreateAgentRunRequest{Scope: scope, Owner: runtime.ObjectiveOwner{Type: "agent", ID: "analyst"}, AssignedAgentID: "analyst", Goal: "Cancel before result", Source: runtime.RunSourceManual})
	if err != nil {
		t.Fatal(err)
	}
	content, _ := artifact.NewLocalStore(t.TempDir())
	publisher := &TaskArtifactPublisher{Scope: scope, Content: content, Catalog: runtime.NewArtifactCatalog(store)}
	runner := runtime.TurnRunnerFunc(func(ctx context.Context, input runtime.TurnExecutionContext) (*runtime.TurnOutcome, error) {
		_, _, err := runtime.NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, runtime.RunTransitionRequest{ExpectedRevision: input.Run.Revision, Status: runtime.AgentRunStatusCanceled, Actor: runtime.ActivityActor{Type: "user", ID: "local-operator"}})
		if err != nil {
			t.Fatal(err)
		}
		return &runtime.TurnOutcome{NextRunStatus: runtime.AgentRunStatusCompleted, RunOutput: fileOutput(generatedFile{Name: "late.txt", MediaType: "text/plain", Text: "late"})}, nil
	})
	_, err = runtime.NewTurnCoordinator(store, store, store).Advance(ctx, runtime.AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "worker", OutputPublisher: publisher}, runner)
	if !errors.Is(err, runtime.ErrLeaseLost) {
		t.Fatal(err)
	}
	records, err := publisher.Catalog.List(ctx, runtime.ArtifactFilter{Scope: scope, ProducerRunID: run.ID})
	if err != nil || len(records) != 0 {
		t.Fatalf("late artifacts published: %+v %v", records, err)
	}
}
