package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skillerror"
	"github.com/google/uuid"
)

// sourceFailureStore is a canonical store with the session log.
type sourceFailureStore interface {
	KernelStore
	ConversationStore
	AgentSessionStore
}

// sourceFailureRun is a claimed, private-thread chat Run of finn whose lane
// session is open, so its steps can record tool entries.
func sourceFailureRun(t *testing.T, store sourceFailureStore, at time.Time) (*AgentRun, *ChannelMessage) {
	t.Helper()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "source-failure-" + uuid.NewString()[:8]}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: owner, Title: "Finn", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "kev"}, Intent: MessageIntentQuestion,
		Content: "Check my recent emails", StartThread: true, ResponseMode: "spoken", RequiresResponse: true,
		Audience:       ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantAgent, ID: owner.ID}}},
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := posted.Message
	lane := conversationRunLane(conversation.ID, trigger.ID, owner.ID)
	commands := NewRunCommandService(store)
	commands.now = func() time.Time { return at }
	created, err := commands.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Kind: RunKindConversation, Source: RunSourceChat,
		Goal: "Check the recent email request", ConcurrencyKey: lane, SessionID: ConversationAgentSessionID(scope, lane),
		Context:        map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: trigger.ID, "threadRootMessageId": trigger.ID},
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "w", Now: at, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.Run.ID {
		t.Fatalf("claim = %+v %v", run, err)
	}
	if _, err := store.OpenConversationAgentSession(ctx, OpenConversationAgentSessionRequest{
		Scope: scope, ConversationID: conversation.ID, ThreadRootID: trigger.ID, AgentID: owner.ID, RunID: run.ID,
		Lease: AgentRunLeaseGuard{WorkerID: "w", Now: at}, Now: at,
	}); err != nil {
		t.Fatal(err)
	}
	run, err = store.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run, trigger
}

// failedSourceCall records one step of the Run that made a governed call
// which failed with callError (and code/details when typed): the stored
// failed loop call, the assistant entry and the error tool entry naming it.
func failedSourceCall(t *testing.T, store KernelStore, sessions AgentSessionStore, run *AgentRun, step, skillID, callError, code string, details map[string]string, at time.Time) {
	t.Helper()
	ctx := t.Context()
	current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := run.ID + ":" + step
	call := &ActionCall{
		ID: id, Scope: run.Scope, RunID: run.ID, TurnID: step, DeploymentID: run.AssignedAgentID,
		SkillID: skillID, SkillVersion: "1.0.0", Action: "read", Origin: ActionOriginLoop, Execution: ActionExecutionLoop,
		Status: ActionCallStatusReady, Risk: "read", SideEffect: "read", Arguments: map[string]interface{}{},
		IdempotencyKey: "loop:" + step + ":" + id, MaxAttempts: 1, AvailableAt: at, Revision: 1, CreatedAt: at, UpdatedAt: at,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	if _, err := store.CreateActionProposal(ctx, ActionProposalRecord{
		Call: call, ExpectedRunRevision: current.Revision, Lease: &AgentRunLeaseGuard{WorkerID: "w", Now: at},
		Event: &ActivityEvent{ID: "proposed-" + id, Scope: run.Scope, EventType: "action.proposed", Severity: ActivitySeverityInfo, RunID: run.ID, TurnID: step,
			Actor: ActivityActor{Type: "worker", ID: "w"}, Summary: "Proposed read", Visibility: ActivityVisibilityScope, CreatedAt: at},
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAction(ctx, ActionClaim{Scope: run.Scope, WorkerID: "w", Now: at, LeaseDuration: LoopActionLease, OwnActionID: id})
	if err != nil || claimed == nil {
		t.Fatalf("own claim = %#v, %v", claimed, err)
	}
	failed := cloneActionCall(claimed)
	failed.Status, failed.Error, failed.ErrorCode, failed.ErrorDetails = ActionCallStatusFailed, callError, code, details
	failed.CompletedAt, failed.LeaseOwner, failed.LeaseExpiresAt, failed.UpdatedAt = &at, "", nil, at
	failed.Revision++
	if _, err := store.PersistActionExecution(ctx, ActionExecutionRecord{
		Call: failed, ExpectedCallRevision: claimed.Revision, ExpectedRunRevision: current.Revision, WorkerID: "w", Now: at,
		Event: &ActivityEvent{ID: "failed-" + id, Scope: run.Scope, RunID: run.ID, EventType: "action.failed", Summary: "Action failed", CreatedAt: at},
	}); err != nil {
		t.Fatal(err)
	}
	tool := testToolEntry(id+":tool", step, "call-"+step, ToolEntryError)
	tool.ActionCallID = id
	appendSourceStep(t, sessions, run, step, at, tool)
}

func appendSourceStep(t *testing.T, sessions AgentSessionStore, run *AgentRun, step string, at time.Time, tool *SessionEntry) {
	t.Helper()
	assistant := testAssistantEntry(run.ID+":"+step+":assistant", step, tool.ToolCallID)
	if _, err := sessions.AppendAgentSessionEntries(t.Context(), AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: run.SessionID, RunID: run.ID, Lease: &AgentRunLeaseGuard{WorkerID: "w", Now: at}, Now: at,
		Entries: []*SessionEntry{assistant, tool},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalSourceFailureLatestOutboxReportsExactCauseOnce(t *testing.T) {
	for _, code := range []string{"source_rate_limited", "source_reads_failed", "browser_proxy_authentication_failed", "browser_proxy_unavailable", "file_too_large", "stale rate limit", "recovered rate limit", "raw error"} {
		t.Run(code, func(t *testing.T) {
			eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
				store := terminalReportingContractFixture(t, fixture)
				kernel, ok := fixture.store.(sourceFailureStore)
				if !ok {
					t.Skip("store-5/store-6b add the session log this report reads to SQLite and Postgres")
				}
				sessions := AgentSessionStore(kernel)
				now := eventWaitContractEpoch
				run, trigger := sourceFailureRun(t, kernel, now)
				kind := code
				if code == "stale rate limit" || code == "recovered rate limit" || code == "raw error" {
					kind = "source_rate_limited"
				}
				details := map[string]string{"url": "PRIVATE_URL", "token": "PRIVATE_TOKEN"}
				if code == "file_too_large" {
					details["fileMegabytes"], details["limitMegabytes"] = "140", "100"
				}
				if code == "source_reads_failed" {
					details["failures"] = `[{"index":0,"failureKind":"source_rate_limited","httpStatus":429},{"index":1,"failureKind":"source_http_error","httpStatus":503}]`
				}
				failure := skillerror.NewActionError(kind, "PRIVATE_ERROR", details)
				callError := failure.Error()
				if code == "raw error" {
					// A stored error that is not the type's sanitized text is
					// model or tool text, never trusted as that type.
					callError = "PRIVATE_RAW HTTP 429"
				}
				failedSourceCall(t, kernel, sessions, run, "step-1", "skill-live-browser", callError, failure.Code(), failure.Details(), now)
				// The loop stops on the step's error streak; the failure names no call.
				explanation := beginRunFailureExplanation(run, RunFailureKindRepeatLimit, RunFailureCodeErrorStreak, "The same error kept repeating.", "", "")
				switch code {
				case "stale rate limit":
					// A newer untyped failure in a later step stopped the attempt.
					failedSourceCall(t, kernel, sessions, run, "step-2", "skill-gmail", "execution failed: PRIVATE_CURRENT", "", nil, now)
					explanation = beginRunFailureExplanation(run, RunFailureKindExecution, "action_failed", "A tool failed.", "", "")
				case "recovered rate limit":
					// The model moved past the 429 and later steps succeeded.
					appendSourceStep(t, sessions, run, "step-2", now, testToolEntry(run.ID+":step-2:tool", "step-2", "call-step-2", ToolEntryCompleted))
				}
				current, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				activity := NewRunActivityService(store, store)
				activity.now = func() time.Time { return now }
				failed, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{
					ExpectedRevision: current.Revision, Status: AgentRunStatusFailed, LeaseOwner: "w", Error: "PRIVATE_RUN_ERROR",
					FailureExplanation: explanation, EventType: "run.failed", Summary: "Run failed",
				})
				if err != nil {
					t.Fatal(err)
				}
				if fixture.reopen != nil {
					fixture.store = fixture.reopen()
					store = terminalReportingContractFixture(t, fixture)
				}
				if count, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 1 {
					t.Fatalf("outbox: count=%d err=%v", count, err)
				}
				message, err := store.FindChannelMessageByIdempotencyKey(t.Context(), run.Scope, trigger.ConversationID, terminalConversationFailureReplyKey(failed))
				if err != nil || message == nil || message.Scope != run.Scope || message.ThreadRootID != trigger.ID || message.ReplyToMessageID != trigger.ID || message.ResolvesMessageID != trigger.ID || message.ResponseMode != trigger.ResponseMode {
					t.Fatalf("source failure lost provenance: %#v %v", message, err)
				}
				switch {
				case code == "stale rate limit":
					// The newer untyped failure stopped the attempt; its answer,
					// not the stale typed one, is the cause the reply names.
					if message.Content != "I couldn't finish this request because Gmail reported: PRIVATE_CURRENT. I stopped this attempt. Ask me to try again when you're ready." {
						t.Fatalf("current failure cause not named: %q", message.Content)
					}
				case strings.Contains(message.Content, "PRIVATE"):
					t.Fatalf("untrusted diagnostic leaked: %q", message.Content)
				case code == "source_rate_limited" || code == "source_reads_failed":
					if !strings.Contains(message.Content, "HTTP 429") || !strings.Contains(message.Content, "Try again later") {
						t.Fatalf("source rate limit hidden: %q", message.Content)
					}
				case strings.HasPrefix(code, "browser_proxy"):
					if !strings.Contains(message.Content, "platform's browser connection") || strings.Contains(message.Content, "HTTP 429") {
						t.Fatalf("proxy cause misattributed: %q", message.Content)
					}
				case code == "file_too_large":
					if !strings.Contains(message.Content, "the file is 140 MB") || !strings.Contains(message.Content, "up to 100 MB") {
						t.Fatalf("file size limit hidden: %q", message.Content)
					}
				default:
					if message.Content != TerminalFailureReply("execution_failed") {
						t.Fatalf("stale or raw evidence attributed: %q", message.Content)
					}
				}
				if count, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 0 {
					t.Fatalf("duplicate failure: count=%d err=%v", count, err)
				}
			})
		})
	}
}
