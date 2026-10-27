package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

type visualTurnHost struct{ visual *presentation.Visual }

func (h visualTurnHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	return &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "test-model",
		NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Not quite.", Visual: cloneVisual(h.visual),
	}, nil
}

func boundAnswerVisual() *presentation.Visual {
	visual := testAnswerVisual()
	visual.Components = append(visual.Components, presentation.Component{ID: "next", Type: "actions", Actions: []presentation.Action{
		{ID: "stop", Label: "Stop", Kind: presentation.ActionEntity, Command: "stop", Binding: &presentation.Binding{Kind: presentation.BindingRunbook, ID: "rb-1"}},
	}})
	return visual
}

func TestHostedTurnRunnerChecksAnswerVisualWithHostHooks(t *testing.T) {
	run := &AgentRun{ID: "run-visual", Scope: Scope{Kind: "tenant", ID: "7"}, AssignedAgentID: "agent", Goal: "Answer"}
	turn := &AgentTurn{ID: "turn-visual", RunID: run.ID}
	config := HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"}

	runner, err := NewHostedTurnRunner(visualTurnHost{visual: boundAnswerVisual()}, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: turn}); err == nil || !strings.Contains(err.Error(), "bindings cannot be checked") {
		t.Fatalf("bound visual without a host resolver = %v", err)
	}

	var resolvedFor string
	config.VisualBindings = func(run *AgentRun) presentation.BindingResolver {
		return presentation.BindingResolverFunc(func(_ context.Context, binding presentation.Binding) error {
			resolvedFor = run.ID + "/" + binding.ID
			return nil
		})
	}
	runner, err = NewHostedTurnRunner(visualTurnHost{visual: boundAnswerVisual()}, config)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: turn})
	if err != nil || outcome.Visual == nil || len(outcome.Visual.Components) != 2 || resolvedFor != "run-visual/rb-1" {
		t.Fatalf("outcome = %#v, resolved %q, err %v", outcome, resolvedFor, err)
	}

	config.VisualBindings = func(*AgentRun) presentation.BindingResolver {
		return presentation.BindingResolverFunc(func(context.Context, presentation.Binding) error { return errors.New("not in this chat") })
	}
	runner, err = NewHostedTurnRunner(visualTurnHost{visual: boundAnswerVisual()}, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: turn}); err == nil || !strings.Contains(err.Error(), "not in this chat") {
		t.Fatalf("out-of-scope binding = %v", err)
	}
}

type answerVisualContent struct {
	puts int
	data []byte
}

func (s *answerVisualContent) Put(_ context.Context, req ArtifactContentWrite) (ArtifactStoredContent, error) {
	s.puts++
	s.data, _ = io.ReadAll(req.Reader)
	return ArtifactStoredContent{ContentRef: "visual-content", SizeBytes: int64(len(s.data)), Digest: hashBytes(s.data)}, nil
}

func (s *answerVisualContent) Open(context.Context, Scope, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

func TestConversationRunTurnRunnerStoresAnswerVisualOnTheAnswer(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "answer-visual"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"},
		Title: "Phone plans", IdempotencyKey: "answer-visual-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Is my phone plan a good deal?", "answer-visual-trigger")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	agentTurns := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{
			DeploymentID: "agent-42", DefinitionID: "agent-definition", DefinitionVersion: "1",
			Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Not quite."}, Visual: testAnswerVisual()}, nil
			}),
		}, nil
	})
	content := &answerVisualContent{}
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: agentTurns, AttachmentContent: content})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runner.ResolveTurnRunner(ctx, scheduled.Run)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run})
	if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted {
		t.Fatalf("outcome = %#v, err=%v", outcome, err)
	}
	id := answerVisualArtifactID(scheduled.Run.ID, trigger.ID)
	if id != answerVisualArtifactID(scheduled.Run.ID, trigger.ID) || id == answerVisualArtifactID(scheduled.Run.ID, "other") || !validOpaqueIdentifier(id, 128) {
		t.Fatalf("visual id %q is not deterministic per run and trigger", id)
	}
	want := []ConversationReference{{Kind: ConversationReferenceRun, ID: scheduled.Run.ID}, {Kind: ConversationReferenceArtifact, ID: id, Version: 1}}
	assertAnswer := func() {
		t.Helper()
		messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
		if err != nil || len(messages) != 2 {
			t.Fatalf("messages = %#v, err=%v", messages, err)
		}
		answer := messages[1]
		if answer.Intent != MessageIntentAnswer || answer.Content != "Not quite." || len(answer.References) != 2 ||
			answer.References[0] != want[0] || answer.References[1] != want[1] {
			t.Fatalf("answer = %#v", answer)
		}
		for _, message := range messages {
			if message.Sender.Type == ConversationParticipantService {
				t.Fatalf("a receipt was posted for the answer visual: %#v", message)
			}
		}
	}
	assertAnswer()
	artifact, err := store.GetArtifact(ctx, scope, id, 1)
	if err != nil || artifact == nil || artifact.Type != presentation.ArtifactType || artifact.MediaType != presentation.MediaType ||
		artifact.Provenance.RunID != scheduled.Run.ID || artifact.Provenance.ActionID != "" || artifact.Provenance.Producer != (ActivityActor{Type: "agent", ID: "agent-42"}) {
		t.Fatalf("artifact = %#v, err=%v", artifact, err)
	}
	surface, err := presentation.Decode(content.data)
	if err != nil || surface.State != presentation.StateReady || surface.Title != "Your phone plan" {
		t.Fatalf("stored visual = %#v, err=%v", surface, err)
	}

	replayed, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: scheduled.Run})
	if err != nil || replayed.RunOutput["replayed"] != true {
		t.Fatalf("replay = %#v, err=%v", replayed, err)
	}
	assertAnswer()
	if content.puts != 1 {
		t.Fatalf("replay stored the visual again: %d puts", content.puts)
	}
	if second, err := store.GetArtifact(ctx, scope, id, 2); second != nil || (err != nil && !errors.Is(err, ErrArtifactNotFound)) {
		t.Fatalf("replay added a visual version: %#v, err=%v", second, err)
	}
}
