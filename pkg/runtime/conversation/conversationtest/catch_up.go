package conversationtest

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

// CatchUpStore is a thread start store that also stores agent sessions.
type CatchUpStore interface {
	ThreadStartStore
	models.AgentSessionStore
}

// CatchUpFixture is a conversation in tenant catch-up whose agent AgentID
// catches up on channel messages through CatchUp.
type CatchUpFixture struct {
	T            *testing.T
	Store        CatchUpStore
	Service      *conversation.Service
	Conversation *models.Conversation
	AgentID      string
	Now          time.Time
	CatchUp      *conversation.CatchUp
}

// ForCatchUpStores runs test with a fixture whose conversation is owned by
// owner, on each store ForThreadStartStores runs.
func ForCatchUpStores(t *testing.T, owner models.ObjectiveOwner, test func(*testing.T, *CatchUpFixture)) {
	t.Helper()
	ForThreadStartStores(t, func(t *testing.T, store ThreadStartStore, _ func(*testing.T) ThreadStartStore) {
		sessions, ok := store.(CatchUpStore)
		if !ok {
			t.Fatalf("%T is not an agent session store", store)
		}
		service := conversation.NewService(store)
		SeedSpace(t, store.(models.SpaceStore), models.Scope{Kind: "tenant", ID: "catch-up"})
		created, _, err := service.CreateConversation(t.Context(), conversation.CreateConversationRequest{
			Scope: models.Scope{Kind: "tenant", ID: "catch-up"}, Owner: owner, Title: "Catch-up", IdempotencyKey: "catch-up",
		})
		if err != nil {
			t.Fatal(err)
		}
		test(t, &CatchUpFixture{
			T: t, Store: sessions, Service: service, Conversation: created, AgentID: "agent",
			Now:     time.Now().UTC().Truncate(time.Millisecond),
			CatchUp: conversation.NewCatchUp(service, store, nil),
		})
	})
}

// Post posts req to the fixture's conversation.
func (f *CatchUpFixture) Post(req conversation.PostChannelMessageRequest) *models.ChannelMessage {
	f.T.Helper()
	return ThreadStartPost(f.T, f.Service, f.Conversation, req)
}

// Say posts an update with content under idempotency key key.
func (f *CatchUpFixture) Say(key, content string) *models.ChannelMessage {
	f.T.Helper()
	return f.Post(conversation.PostChannelMessageRequest{IdempotencyKey: key, Content: content, Intent: models.MessageIntentUpdate})
}

// Answer posts content as sender's answer for runID.
func (f *CatchUpFixture) Answer(key, content string, sender models.ConversationParticipant, runID string) *models.ChannelMessage {
	f.T.Helper()
	return f.Post(conversation.PostChannelMessageRequest{
		IdempotencyKey: key, Content: content, Intent: models.MessageIntentUpdate, Sender: sender,
		References: []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: runID}},
	})
}

// Agent is the fixture's agent as a participant.
func (f *CatchUpFixture) Agent() models.ConversationParticipant {
	return models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: f.AgentID}
}

// Viewer is the fixture's agent as a viewer.
func (f *CatchUpFixture) Viewer() models.ConversationViewer {
	return models.ConversationViewer{Participant: f.Agent()}
}

// LaneSessionID is the agent's session of the lane of threadRoot.
func (f *CatchUpFixture) LaneSessionID(threadRoot string) string {
	return models.ConversationSessionID(f.Conversation.Scope, models.ConversationRunLane(f.Conversation.ID, threadRoot, f.AgentID))
}

// Run stores a Run bound to sessionID and, when claim is set, claims it under
// worker w1. Runs are claimed as they are created, so the claim takes this one.
func (f *CatchUpFixture) Run(sessionID string, claim bool) *models.AgentRun {
	f.T.Helper()
	runs := portfolio.NewService(f.Store, models.WithClock(func() time.Time { return f.Now }))
	run, err := runs.CreateAgentRun(f.T.Context(), models.CreateAgentRunRequest{
		Scope: f.Conversation.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: f.AgentID}, AssignedAgentID: f.AgentID,
		Goal: "answer the chat", Source: models.RunSourceObjective, SessionID: sessionID,
	})
	if err != nil {
		f.T.Fatalf("create run: %v", err)
	}
	if !claim {
		return run
	}
	claimed, err := f.Store.ClaimNextAgentRun(f.T.Context(), models.AgentRunClaim{
		Scope: f.Conversation.Scope, WorkerID: "w1", Now: f.Now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		f.T.Fatalf("claim = %#v, %v (want %s)", claimed, err, run.ID)
	}
	return claimed
}

// Open claims a lane Run and opens (creates) its session.
func (f *CatchUpFixture) Open(threadRoot string) (*models.AgentRun, *models.AgentSession) {
	f.T.Helper()
	run := f.Run(f.LaneSessionID(threadRoot), true)
	opened, err := f.Store.OpenConversationAgentSession(f.T.Context(), models.OpenConversationAgentSessionRequest{
		Scope: f.Conversation.Scope, ConversationID: f.Conversation.ID, ThreadRootID: threadRoot, AgentID: f.AgentID,
		RunID: run.ID, Lease: models.AgentRunLeaseGuard{WorkerID: "w1", Now: f.Now}, Now: f.Now,
	})
	if err != nil {
		f.T.Fatalf("open session: %v", err)
	}
	return run, opened.Session
}

// Request is the catch-up of run's step 1 on session through message
// sequence through.
func (f *CatchUpFixture) Request(run *models.AgentRun, session *models.AgentSession, through int64) conversation.CatchUpRequest {
	return conversation.CatchUpRequest{Run: run, Session: session, Conversation: f.Conversation, Viewer: f.Viewer(), Through: through, Step: 1}
}

// Collect collects req, failing the test on an error.
func (f *CatchUpFixture) Collect(req conversation.CatchUpRequest) *conversation.CatchUpResult {
	f.T.Helper()
	result, err := f.CatchUp.Collect(f.T.Context(), req)
	if err != nil {
		f.T.Fatalf("collect: %v", err)
	}
	return result
}

// Commit appends result as the step context append and returns the session.
func (f *CatchUpFixture) Commit(run *models.AgentRun, session *models.AgentSession, result *conversation.CatchUpResult) *models.AgentSession {
	f.T.Helper()
	part := result.StepContextAppend(session)
	if part == nil {
		return session
	}
	appended, err := f.Store.AppendAgentSessionEntries(f.T.Context(), models.AppendAgentSessionEntriesRequest{
		Scope: session.Scope, SessionID: session.ID, RunID: run.ID, Lease: &models.AgentRunLeaseGuard{WorkerID: "w1", Now: f.Now},
		Entries: part.Entries, ExpectedChannelWatermark: part.ExpectedChannelWatermark, ChannelWatermark: part.ChannelWatermark, Now: f.Now,
	})
	if err != nil {
		f.T.Fatalf("append catch-up: %v", err)
	}
	return appended.Session
}

// SessionEntries lists the entries of session sessionID.
func (f *CatchUpFixture) SessionEntries(sessionID string) []*models.SessionEntry {
	f.T.Helper()
	entries, err := f.Store.ListAgentSessionEntries(f.T.Context(), models.SessionEntryFilter{Scope: f.Conversation.Scope, SessionID: sessionID})
	if err != nil {
		f.T.Fatal(err)
	}
	return entries
}

// CatchUpMessageIDs lists the channel message IDs of the user entries.
func CatchUpMessageIDs(entries []*models.SessionEntry) []string {
	ids := []string{}
	for _, entry := range entries {
		if entry.Type == models.SessionEntryUser && entry.User != nil {
			ids = append(ids, entry.User.MessageID)
		}
	}
	return ids
}
