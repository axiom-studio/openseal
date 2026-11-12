package conversationrun

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// The helpers in this file are for the in-package conversation run tests,
// which cannot import conversationruntest (it imports conversationrun) or
// use the test helpers of runtime. They copy conversationruntest's run
// scheduler and message post, and runtime's coordinator, SQLite store
// opener, visual content store and continuation fixture, and a trimmed chat
// loop fixture of the run loop tests. Change the copies together.

// newTestScheduler is a run scheduler over store with the default config.
func newTestScheduler(t *testing.T, store *sqlite.Store) *ConversationRunScheduler {
	t.Helper()
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

// newTestCoordinator routes every unaddressed message to agent-1.
func newTestCoordinator(t *testing.T, service *conversation.ConversationService) *conversation.ConversationCoordinator {
	t.Helper()
	participants := conversation.ConversationParticipantSourceFunc(func(context.Context, conversation.ConversationParticipantQuery) ([]conversation.ConversationParticipantBinding, error) {
		return []conversation.ConversationParticipantBinding{{
			Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "agent-1"}, SemanticRoles: []string{"developer"},
		}}, nil
	})
	router := conversation.ConversationRouterFunc(func(context.Context, conversation.ConversationRouteRequest) (conversation.ConversationRouteDecision, error) {
		return conversation.ConversationRouteDecision{AgentID: "agent-1", Reason: "owns the canary"}, nil
	})
	coordinator, err := conversation.NewConversationCoordinator(service, participants, router, conversation.DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

// postTestMessage posts content to the channel from a sender of senderType.
func postTestMessage(
	t *testing.T,
	service *conversation.ConversationService,
	target *models.Conversation,
	senderType models.ConversationParticipantType,
	intent models.ConversationMessageIntent,
	content string,
	key string,
) *models.ChannelMessage {
	t.Helper()
	result, err := service.PostChannelMessage(t.Context(), conversation.PostChannelMessageRequest{
		Scope: target.Scope, ConversationID: target.ID, ExpectedRevision: target.Revision,
		Sender: models.ConversationParticipant{Type: senderType, ID: "sender-" + string(senderType)},
		Intent: intent, Content: content, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

// newFileSQLiteStore opens a SQLite store on a fresh file and the test's own
// connection to that file, both closed when the test ends.
func newFileSQLiteStore(t testing.TB) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, conversationtest.OpenSQLiteDB(t, path)
}

// storedRun reads a Run that the test knows exists.
func storedRun(t testing.TB, store *sqlite.Store, scope models.Scope, id string) *models.AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return run
}

// visualTestContent is the artifact content store visuals are written to.
type visualTestContent struct {
	puts int
	data []byte
}

func (s *visualTestContent) Put(_ context.Context, req models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	s.puts++
	s.data, _ = io.ReadAll(req.Reader)
	return models.ArtifactStoredContent{ContentRef: "visual-content", SizeBytes: int64(len(s.data)), Digest: models.SHA256Digest(s.data)}, nil
}

func (s *visualTestContent) Open(context.Context, models.Scope, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

// continuationFixture schedules the conversation Run that answers a user's
// request in a new agent chat.
func continuationFixture(t *testing.T, store *sqlite.Store, id string) (*models.AgentRun, *models.Conversation, *models.ChannelMessage) {
	t.Helper()
	service := conversation.NewConversationService(store)
	scope := models.Scope{Kind: "tenant", ID: id}
	conversationtest.SeedSpace(t, store, scope)
	target, _, err := service.CreateConversation(t.Context(), conversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Title: "Assistant", IdempotencyKey: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := postTestMessage(t, service, target, models.ConversationParticipantUser, models.MessageIntentQuestion, "Review the release", id+"-message")
	result, _, err := newTestScheduler(t, store).ScheduleMessage(t.Context(), scope, target.ID, message.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule conversation: %#v %v", result, err)
	}
	return result.Run, target, message
}

// backdateContinuationRun makes the Run look created at createdAt. No store
// method changes a Run's creation time, so it rewrites the row on db, the
// test's own connection to the store's file.
func backdateContinuationRun(t *testing.T, store *sqlite.Store, db *sql.DB, run *models.AgentRun, createdAt time.Time) *models.AgentRun {
	t.Helper()
	storetest.ExecSQL(t, db, `UPDATE agent_runs SET created_at = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`, createdAt, run.Scope.Kind, run.Scope.ID, run.ID)
	storetest.RewriteRowPayload(t, db, "agent_runs", run.Scope, run.ID, func(stored *models.AgentRun) { stored.CreatedAt = createdAt })
	return storedRun(t, store, run.Scope, run.ID)
}

const bindingWorker = "chat-loop-worker"

// bindingFixture is the run loop tests' chat loop fixture trimmed to what a
// binding check needs: a chat on each thread start store and the
// conversation turn runner that adds the chat seams to an Agent's binding.
type bindingFixture struct {
	t            *testing.T
	ctx          context.Context
	store        conversationtest.ThreadStartStore
	service      *conversation.ConversationService
	conversation *models.Conversation
	runner       *ConversationRunTurnRunner
	now          time.Time
	agents       bindingAgentTurns
}

func forBindingStores(t *testing.T, test func(*testing.T, *bindingFixture)) {
	t.Helper()
	conversationtest.ForThreadStartStores(t, func(t *testing.T, base conversationtest.ThreadStartStore, _ func(*testing.T) conversationtest.ThreadStartStore) {
		test(t, newBindingFixture(t, base))
	})
}

func newBindingFixture(t *testing.T, store conversationtest.ThreadStartStore) *bindingFixture {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "chat-loop-" + uuid.NewString()[:8]}
	service := conversation.NewConversationService(store)
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	target, _, err := service.CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}, Title: "Chat loop", IdempotencyKey: "chat-loop",
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &bindingFixture{
		t: t, ctx: ctx, store: store, service: service, conversation: target,
		now: time.Now().UTC().Truncate(time.Millisecond), agents: bindingAgentTurns{},
	}
	f.runner, err = NewConversationRunTurnRunner(store, newTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: f.agents})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// bindingAgentTurns is the bare Agent binding bind registered for each Run,
// by Run id.
type bindingAgentTurns map[string]*agentrun.TurnRunnerBinding

func (a bindingAgentTurns) ResolveTurnRunner(_ context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
	binding, ok := a[run.ID]
	if !ok {
		return nil, fmt.Errorf("no Agent binding registered for Run %s", run.ID)
	}
	return binding, nil
}

func (f *bindingFixture) post(key, userID, content string) *models.ChannelMessage {
	f.t.Helper()
	current, err := f.service.GetConversation(f.ctx, f.conversation.Scope, f.conversation.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	posted, err := f.service.PostChannelMessage(f.ctx, conversation.PostChannelMessageRequest{
		Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: userID}, Intent: models.MessageIntentQuestion,
		Content: content, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: key,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.conversation, err = f.service.GetConversation(f.ctx, current.Scope, current.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return posted.Message
}

// startRun creates and claims the chat Run that answers trigger.
func (f *bindingFixture) startRun(trigger *models.ChannelMessage) *models.AgentRun {
	f.t.Helper()
	request := conversation.ConversationAgentRunRequest(f.conversation, trigger)
	commands := agentrun.NewRunCommandService(f.store, models.WithClock(func() time.Time { return f.now }))
	result, err := commands.CreateAgentRun(f.ctx, request)
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	claimed, err := f.store.ClaimNextAgentRun(f.ctx, models.AgentRunClaim{
		Scope: f.conversation.Scope, WorkerID: bindingWorker, Now: f.now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != result.Run.ID {
		f.t.Fatalf("claim = %#v, %v (want %s)", claimed, err, result.Run.ID)
	}
	return claimed
}

// bind resolves agent-1's binding for run through the conversation turn
// runner, as the worker does: the runner adds the chat seams to a bare
// binding whose only preparer is runStart, the Agent's run-start preparer.
func (f *bindingFixture) bind(run *models.AgentRun, producer agentrun.StepProducer, runStart agentrun.StepPreparer) *agentrun.TurnRunnerBinding {
	f.t.Helper()
	f.agents[run.ID] = &agentrun.TurnRunnerBinding{
		DeploymentID: "agent-1", DefinitionID: "agent-definition", DefinitionVersion: "1",
		ModelProvider: string(models.ModelStepProviderAnthropic), Model: "chat-model",
		Producer: producer, Preparers: []agentrun.StepPreparer{runStart},
		// What ResolveControlAuthority grants the speaking Agent of a chat.
		Authority: agentrun.ControlAuthority{AgentID: "agent-1", CanAskUser: true, CanReact: true},
	}
	binding, err := f.runner.ResolveTurnRunner(f.ctx, run)
	if err != nil {
		f.t.Fatalf("resolve the chat binding of %s: %v", run.ID, err)
	}
	return binding
}

// bindingProducer is a model producer the binding checks never call.
type bindingProducer struct{}

func (bindingProducer) Kind() models.StepProducerKind { return models.StepProducerModel }

func (bindingProducer) Produce(context.Context, agentrun.StepInput) (agentrun.StepOutput, error) {
	return agentrun.StepOutput{}, nil
}

// runStartStandIn stands in for the run loop's run-start preparer, which
// the Agent's binding lists first.
type runStartStandIn struct{}

func (runStartStandIn) BeforeStep(context.Context, agentrun.StepInput, agentrun.SteeringPlan) (*agentrun.StepContextAppend, error) {
	return nil, nil
}
