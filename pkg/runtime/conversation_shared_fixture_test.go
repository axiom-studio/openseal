package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type CatchUpStore interface {
	conversationThreadStartTestStore
	models.AgentSessionStore
}

type CatchUpFixture struct {
	t            *testing.T
	store        CatchUpStore
	service      *ConversationService
	conversation *models.Conversation
	agentID      string
	now          time.Time
	catchUp      *conversationCatchUp
}

func ForCatchUpStores(t *testing.T, owner models.ObjectiveOwner, test func(*testing.T, *CatchUpFixture)) {
	t.Helper()
	forConversationThreadStartStores(t, func(t *testing.T, store conversationThreadStartTestStore, _ func(*testing.T) conversationThreadStartTestStore) {
		sessions, ok := store.(CatchUpStore)
		if !ok {
			t.Fatalf("%T is not an agent session store", store)
		}
		service := NewConversationService(store)
		seedTestSpace(t, store.(models.SpaceStore), models.Scope{Kind: "tenant", ID: "catch-up"})
		conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
			Scope: models.Scope{Kind: "tenant", ID: "catch-up"}, Owner: owner, Title: "Catch-up", IdempotencyKey: "catch-up",
		})
		if err != nil {
			t.Fatal(err)
		}
		test(t, &CatchUpFixture{
			t: t, store: sessions, service: service, conversation: conversation, agentID: "agent",
			now:     time.Now().UTC().Truncate(time.Millisecond),
			catchUp: newConversationCatchUp(service, store, nil),
		})
	})
}

func (f *CatchUpFixture) post(req PostChannelMessageRequest) *models.ChannelMessage {
	f.t.Helper()
	return conversationThreadStartPost(f.t, f.service, f.conversation, req)
}

func (f *CatchUpFixture) say(key, content string) *models.ChannelMessage {
	f.t.Helper()
	return f.post(PostChannelMessageRequest{IdempotencyKey: key, Content: content, Intent: models.MessageIntentUpdate})
}

func (f *CatchUpFixture) answer(key, content string, sender models.ConversationParticipant, runID string) *models.ChannelMessage {
	f.t.Helper()
	return f.post(PostChannelMessageRequest{
		IdempotencyKey: key, Content: content, Intent: models.MessageIntentUpdate, Sender: sender,
		References: []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: runID}},
	})
}

func (f *CatchUpFixture) agent() models.ConversationParticipant {
	return models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: f.agentID}
}

func (f *CatchUpFixture) viewer() models.ConversationViewer {
	return models.ConversationViewer{Participant: f.agent()}
}

func (f *CatchUpFixture) laneSessionID(threadRoot string) string {
	return models.ConversationSessionID(f.conversation.Scope, models.ConversationRunLane(f.conversation.ID, threadRoot, f.agentID))
}

// run stores a Run bound to sessionID and, when claim is set, claims it under
// worker w1. Runs are claimed as they are created, so the claim takes this one.
func (f *CatchUpFixture) run(sessionID string, claim bool) *models.AgentRun {
	f.t.Helper()
	portfolio := portfolio.NewPortfolioService(f.store, models.WithClock(func() time.Time { return f.now }))
	run, err := portfolio.CreateAgentRun(f.t.Context(), models.CreateAgentRunRequest{
		Scope: f.conversation.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: f.agentID}, AssignedAgentID: f.agentID,
		Goal: "answer the chat", Source: models.RunSourceObjective, SessionID: sessionID,
	})
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	if !claim {
		return run
	}
	claimed, err := f.store.ClaimNextAgentRun(f.t.Context(), models.AgentRunClaim{
		Scope: f.conversation.Scope, WorkerID: "w1", Now: f.now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		f.t.Fatalf("claim = %#v, %v (want %s)", claimed, err, run.ID)
	}
	return claimed
}

// open claims a lane Run and opens (creates) its session.
func (f *CatchUpFixture) open(threadRoot string) (*models.AgentRun, *models.AgentSession) {
	f.t.Helper()
	run := f.run(f.laneSessionID(threadRoot), true)
	opened, err := f.store.OpenConversationAgentSession(f.t.Context(), models.OpenConversationAgentSessionRequest{
		Scope: f.conversation.Scope, ConversationID: f.conversation.ID, ThreadRootID: threadRoot, AgentID: f.agentID,
		RunID: run.ID, Lease: models.AgentRunLeaseGuard{WorkerID: "w1", Now: f.now}, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("open session: %v", err)
	}
	return run, opened.Session
}

func (f *CatchUpFixture) request(run *models.AgentRun, session *models.AgentSession, through int64) catchUpRequest {
	return catchUpRequest{Run: run, Session: session, Conversation: f.conversation, Viewer: f.viewer(), Through: through, Step: 1}
}

func (f *CatchUpFixture) collect(req catchUpRequest) *catchUpResult {
	f.t.Helper()
	result, err := f.catchUp.collect(f.t.Context(), req)
	if err != nil {
		f.t.Fatalf("collect: %v", err)
	}
	return result
}

// commit appends result as the step context append and returns the session.
func (f *CatchUpFixture) commit(run *models.AgentRun, session *models.AgentSession, result *catchUpResult) *models.AgentSession {
	f.t.Helper()
	part := result.stepContextAppend(session)
	if part == nil {
		return session
	}
	appended, err := f.store.AppendAgentSessionEntries(f.t.Context(), models.AppendAgentSessionEntriesRequest{
		Scope: session.Scope, SessionID: session.ID, RunID: run.ID, Lease: &models.AgentRunLeaseGuard{WorkerID: "w1", Now: f.now},
		Entries: part.Entries, ExpectedChannelWatermark: part.ExpectedChannelWatermark, ChannelWatermark: part.ChannelWatermark, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("append catch-up: %v", err)
	}
	return appended.Session
}

func (f *CatchUpFixture) sessionEntries(sessionID string) []*models.SessionEntry {
	f.t.Helper()
	entries, err := f.store.ListAgentSessionEntries(f.t.Context(), models.SessionEntryFilter{Scope: f.conversation.Scope, SessionID: sessionID})
	if err != nil {
		f.t.Fatal(err)
	}
	return entries
}

func CatchUpMessageIDs(entries []*models.SessionEntry) []string {
	ids := []string{}
	for _, entry := range entries {
		if entry.Type == models.SessionEntryUser && entry.User != nil {
			ids = append(ids, entry.User.MessageID)
		}
	}
	return ids
}

type CountedPointStore struct {
	models.ConversationStore
	points int
	failID string
	err    error
}

func (s *CountedPointStore) GetChannelMessage(ctx context.Context, scope models.Scope, conversationID, id string) (*models.ChannelMessage, error) {
	s.points++
	if id == s.failID && s.err != nil {
		return nil, s.err
	}
	return s.ConversationStore.GetChannelMessage(ctx, scope, conversationID, id)
}

type CountedBatchStore struct {
	*CountedPointStore
	batch     models.ChannelMessageBatchStore
	batches   [][]string
	calls     int
	countOnly bool
	failID    string
	err       error
}

func (s *CountedBatchStore) GetChannelMessagesByIDs(ctx context.Context, scope models.Scope, conversationID string, ids []string) ([]*models.ChannelMessage, error) {
	s.calls++
	if !s.countOnly {
		s.batches = append(s.batches, append([]string(nil), ids...))
	}
	for _, id := range ids {
		if id == s.failID && s.err != nil {
			return nil, s.err
		}
	}
	return s.batch.GetChannelMessagesByIDs(ctx, scope, conversationID, ids)
}

func NewCountedBatchStore(store models.ConversationStore) *CountedBatchStore {
	return &CountedBatchStore{CountedPointStore: &CountedPointStore{ConversationStore: store}, batch: store.(models.ChannelMessageBatchStore)}
}

func MessageBatchStores(t *testing.T, test func(*testing.T, models.KernelStore)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "message-batch.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		test(t, store)
	})
}

func MessageBatchConversation(t testing.TB, store models.ConversationStore, scope models.Scope, id string) *models.Conversation {
	t.Helper()
	seedTestSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := NewConversationService(store).CreateConversation(context.Background(), CreateConversationRequest{
		ID: id, Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}, Title: "Batch visibility", IdempotencyKey: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func CommitChannelMessage(t testing.TB, store models.ConversationStore, conversation **models.Conversation, message *models.ChannelMessage) {
	t.Helper()
	current := *conversation
	message.Scope, message.ConversationID = current.Scope, current.ID
	message.Sequence, message.CreatedAt, message.IdempotencyKey = current.LastSequence+1, time.Now().UTC(), message.ID
	message.Sender = models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "sender"}
	message.Intent, message.Content = models.MessageIntentUpdate, "Visibility fixture "+message.ID
	if message.Audience.Kind == "" {
		message.Audience.Kind = models.ConversationAudienceChannel
	}
	next := models.CloneConversation(current)
	next.Revision, next.LastSequence, next.UpdatedAt = current.Revision+1, message.Sequence, message.CreatedAt
	committed, err := store.CommitChannelMessage(context.Background(), models.ChannelMessageCommitRecord{Conversation: next, ExpectedRevision: current.Revision, Message: message})
	if err != nil {
		t.Fatal(err)
	}
	*conversation = committed.Conversation
}

func AgentParticipant(id string) models.ConversationParticipant {
	return models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: id}
}

type TextContent struct {
	text  string
	opens int
	scope models.Scope
}

func (s *TextContent) Put(context.Context, models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	return models.ArtifactStoredContent{}, errors.New("not supported")
}

func (s *TextContent) Open(_ context.Context, scope models.Scope, _ string) (io.ReadCloser, error) {
	s.opens++
	s.scope = scope
	return io.NopCloser(strings.NewReader(s.text)), nil
}

func Digest(content string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(content)))
}

func CSVArtifact() *models.Artifact {
	return &models.Artifact{ID: "file", Version: 1, Scope: models.Scope{Kind: "tenant", ID: "one"},
		Name: "file.csv", ContentRef: "object-store:file", Classification: models.ArtifactClassificationInternal,
		Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "user", ID: "owner"}}}
}

// RunsByID is a Run reader over the Runs a test puts in it, for code that
// only reads Runs.
type RunsByID map[string]*models.AgentRun

func (r RunsByID) GetAgentRun(_ context.Context, scope models.Scope, runID string) (*models.AgentRun, error) {
	run, ok := r[runID]
	if !ok || run.Scope != scope {
		return nil, models.ErrRunNotFound
	}
	return models.CloneAgentRun(run), nil
}

var ApprovalModeActor = models.ActivityActor{Type: "user", ID: "user-7"}

func ApprovalModeConversation(t *testing.T, store *sqlite.Store) *models.Conversation {
	return ApprovalModeConversationWithKey(t, store, "chat")
}

func ApprovalModeConversationWithKey(t *testing.T, store *sqlite.Store, key string) *models.Conversation {
	t.Helper()
	seedTestSpace(t, store, models.Scope{Kind: "tenant", ID: "one"})
	conversation, _, err := NewConversationService(store).CreateConversation(context.Background(), CreateConversationRequest{
		Scope: models.Scope{Kind: "tenant", ID: "one"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "assistant"}, Title: "Chat " + key, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func SetApprovalMode(t *testing.T, service *ConversationApprovalModeService, conversation *models.Conversation, mode models.ConversationApprovalMode) *SetConversationApprovalModeResult {
	t.Helper()
	result, err := service.SetApprovalMode(context.Background(), SetConversationApprovalModeRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Mode: mode, Actor: ApprovalModeActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func ApprovalModeCatalog(t *testing.T) *skill.Catalog {
	t.Helper()
	ctx := context.Background()
	catalog := skill.NewCatalog()
	object := map[string]interface{}{"type": "object"}
	definition := &skill.Definition{
		ID: "workspace", Version: "1.0.0", Name: "Workspace", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://workspace.invalid"},
		Actions: map[string]skill.Action{
			"note": {Name: "note", Description: "Write a note", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, InputSchema: object, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden},
			"send": {Name: "send", Description: "Send a message", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, InputSchema: object, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationOptional},
			"pay": {Name: "pay", Description: "Pay for the checkout", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Review: skill.ActionReviewAlways,
				Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden, InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"amount": map[string]interface{}{"type": "string"}, "currency": map[string]interface{}{"type": "string"},
						"merchant": map[string]interface{}{"type": "string"}, "cardToken": map[string]interface{}{"type": "string", "x-sensitive": true},
					},
				}},
		},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "workspace-binding", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "assistant",
		SkillID: "workspace", SkillVersion: "1.0.0", AllowedActions: []string{"note", "send", "pay"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}

var approvalModeRuns int

func ProposeForApprovalMode(t *testing.T, store *sqlite.Store, coordinator *runtimeaction.ActionCoordinator, conversation *models.Conversation, action string) *models.ApprovalCheckpoint {
	t.Helper()
	approvalModeRuns++
	return ProposeForApprovalModeInConversation(t, store, coordinator, conversation.ID, action, fmt.Sprintf("run-%s-%d", action, approvalModeRuns))
}

// ProposeForApprovalModeInConversation proposes action as a loop step of a
// new Run of the conversation and parks the Run on its approval, as the loop
// does at step end.
func ProposeForApprovalModeInConversation(t *testing.T, store *sqlite.Store, coordinator *runtimeaction.ActionCoordinator, conversationID, action, key string) *models.ApprovalCheckpoint {
	t.Helper()
	result, f, err := ProposeLoopForApprovalMode(t, store, coordinator, conversationID, action, key, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Approval == nil {
		t.Fatalf("%s proposal did not wait for approval: %#v", action, result.Call)
	}
	f.AssertUntouched()
	f.ParkOnApproval(result.Approval.ID)
	return result.Approval
}

// ProposeLoopForApprovalMode proposes action as tool call key of a loop step
// of a new Run acting for conversationID.
func ProposeLoopForApprovalMode(t *testing.T, store *sqlite.Store, coordinator *runtimeaction.ActionCoordinator, conversationID, action, key string, arguments map[string]interface{}) (*models.ActionProposalResult, *actiontest.LoopProposalFixture, error) {
	t.Helper()
	return ProposeLoopForApprovalModeWithContext(t, store, coordinator, map[string]interface{}{"conversationId": conversationID}, action, key, arguments)
}

// ProposeLoopForApprovalModeWithContext is ProposeLoopForApprovalMode for a
// new Run with this Context.
func ProposeLoopForApprovalModeWithContext(t *testing.T, store *sqlite.Store, coordinator *runtimeaction.ActionCoordinator, runContext map[string]interface{}, action, key string, arguments map[string]interface{}) (*models.ActionProposalResult, *actiontest.LoopProposalFixture, error) {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "one"}
	now := time.Now().UTC()
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "assistant"}, AssignedAgentID: "assistant", Goal: key, Source: models.RunSourceObjective,
		Context: runContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := actiontest.NewLoopProposalFixture(t, store, run, "worker", now)
	result, err := coordinator.Propose(ctx, f.Request(runtimeaction.ProposeActionRequest{
		DeploymentID: "assistant", SkillID: "workspace", SkillVersion: "1.0.0", Action: action, Arguments: arguments,
		Summary: "Run " + action + " for " + key,
	}, key))
	return result, f, err
}
