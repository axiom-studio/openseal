package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/google/uuid"
)

// RunLoopCutoverCode names the cutover: the FailureExplanation.Code of every
// Run it cancels (SPEC §2.8) and the actor of its adopt and cancel events.
const RunLoopCutoverCode = "run_loop_cutover"

// RunLoopCutoverMessage answers each unanswered chat request the cutover
// stopped. It is also the canceled Run's FailureExplanation.Message.
const RunLoopCutoverMessage = "This request was stopped by a platform update. Send it again to continue."

// runLoopCutoverRetries bounds the re-reads after a revision race on one Run
// or one conversation.
const runLoopCutoverRetries = 5

// RunLoopCutoverConversations posts the visible cancel message. It is
// satisfied by *ConversationService.
type RunLoopCutoverConversations interface {
	GetConversation(ctx context.Context, scope Scope, conversationID string) (*Conversation, error)
	GetChannelMessage(ctx context.Context, scope Scope, conversationID, messageID string) (*ChannelMessage, error)
	ListChannelMessages(ctx context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error)
	PostChannelMessage(ctx context.Context, req runtimeconversation.PostChannelMessageRequest) (*ChannelMessageCommitResult, error)
}

// RunLoopCutoverReport counts what one cutover pass did.
type RunLoopCutoverReport struct {
	Adopted  int
	Canceled int
	Messages int
	Skipped  int
}

// RunLoopCutover moves the store onto the run loop (A16, SPEC §4.8). It must
// run before any worker starts. Every non-terminal Run without an Engine
// marker is either adopted (queued, never claimed, no step row: the loop can
// start it fresh) or canceled through CommandAgentRun, which closes its
// running step and open tool entries. A canceled Run carries
// FailureExplanation{Code: run_loop_cutover}. Each unanswered chat request of
// a canceled Run gets one visible system message, keyed like the canceled-run
// reconciler's so neither doubles the other. A lane holds at most one
// unanswered request per Run, so this is one message per request, not per
// lane: two old Runs of one lane with different open requests each get one.
//
// It is idempotent: after a full pass no Run qualifies. It fails closed: any
// error is returned and the caller must not start workers.
func RunLoopCutover(ctx context.Context, store RunLoopCutoverStore, conversations RunLoopCutoverConversations) (RunLoopCutoverReport, error) {
	var report RunLoopCutoverReport
	if store == nil {
		return report, errors.New("run loop cutover: store is required")
	}
	err := store.WithRunLoopCutoverLock(ctx, func(ctx context.Context) error {
		refs, err := store.ListRunLoopCutoverCandidates(ctx)
		if err != nil {
			return fmt.Errorf("list cutover candidates: %w", err)
		}
		c := &runLoopCutover{store: store, conversations: conversations, commands: agentrun.NewRunCommandService(store), posted: map[string]bool{}}
		for _, ref := range refs {
			if err := c.cutRun(ctx, ref, &report); err != nil {
				return fmt.Errorf("cut over Run %s: %w", ref.ID, err)
			}
		}
		return nil
	})
	if err != nil {
		return report, fmt.Errorf("run loop cutover: %w", err)
	}
	return report, nil
}

type runLoopCutover struct {
	store         RunLoopCutoverStore
	conversations RunLoopCutoverConversations
	commands      *agentrun.RunCommandService
	// posted holds the requests answered in this pass, so a lane whose
	// request also reached a child Run gets one message.
	posted map[string]bool
}

var runLoopCutoverActor = ActivityActor{Type: "runtime", ID: RunLoopCutoverCode}

func (c *runLoopCutover) cutRun(ctx context.Context, ref RunLoopCutoverRef, report *RunLoopCutoverReport) error {
	for attempt := 0; ; attempt++ {
		run, err := c.store.GetAgentRun(ctx, ref.Scope, ref.ID)
		if err != nil {
			return err
		}
		if run == nil || isTerminalAgentRunStatus(run.Status) || run.Engine != "" {
			report.Skipped++
			return nil
		}
		adopt, err := c.adoptable(ctx, run)
		if err != nil {
			return err
		}
		if adopt {
			err = c.adopt(ctx, run)
		} else {
			err = c.cancel(ctx, run, report)
		}
		if errors.Is(err, ErrRevisionConflict) && attempt+1 < runLoopCutoverRetries {
			continue
		}
		if err != nil {
			return err
		}
		if adopt {
			report.Adopted++
		} else {
			report.Canceled++
		}
		return nil
	}
}

// adoptable reports whether the loop can start run fresh: it is queued, never
// applied a Turn, has no agent_turns row and has the session binding the loop
// needs, or one adopt can derive. A conversation task Run without a session
// cannot be bound here (its child session forks the chat at promotion), so
// it is canceled.
func (c *runLoopCutover) adoptable(ctx context.Context, run *AgentRun) (bool, error) {
	if run.Status != AgentRunStatusQueued || run.LastAppliedTurn != 0 {
		return false, nil
	}
	if needsLaneSession(run) {
		if task, _ := run.Context[ConversationTaskContextKey].(string); task != "" {
			return false, nil
		}
	}
	turns, err := c.store.ListAgentTurns(ctx, AgentTurnFilter{Scope: run.Scope, RunID: run.ID, Limit: 1})
	if err != nil {
		return false, err
	}
	return len(turns) == 0, nil
}

func (c *runLoopCutover) adopt(ctx context.Context, current *AgentRun) error {
	now := time.Now().UTC()
	run := cloneAgentRun(current)
	run.Engine = RunEngineLoop
	if needsLaneSession(run) {
		// Old chat Runs predate the lane binding; the loop opens the lane
		// session only for a Run bound to it, as the scheduler binds new ones.
		run.SessionID = conversationSessionID(run.Scope, run.ConcurrencyKey)
	}
	run.Revision = current.Revision + 1
	run.UpdatedAt = now
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.engine_adopted", Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID, TeamID: teamIDForRun(run),
		Actor: runLoopCutoverActor, Summary: "Run moved to the updated agent runtime", Visibility: ActivityVisibilityPrivate, CreatedAt: now,
		Payload: map[string]interface{}{"engine": RunEngineLoop, "code": RunLoopCutoverCode, "sessionId": run.SessionID},
	}
	_, err := c.store.UpdateAgentRunWithEvent(ctx, run, current.Revision, event, nil)
	return err
}

// needsLaneSession reports an acting-Agent conversation Run with no session:
// one created before chat Runs were bound to their lane session. Team routing
// Runs (no assigned Agent) have no session.
func needsLaneSession(run *AgentRun) bool {
	return run.Kind == RunKindConversation && run.SessionID == "" && run.AssignedAgentID != ""
}

func runLoopCutoverExplanation() *RunFailureExplanation {
	return &RunFailureExplanation{Kind: "execution", Code: RunLoopCutoverCode, Message: RunLoopCutoverMessage}
}

// cancel posts the lane's message first and then cancels the Run, so a pass
// interrupted between the two still leaves the message: the next pass finds
// the request answered and only cancels.
func (c *runLoopCutover) cancel(ctx context.Context, run *AgentRun, report *RunLoopCutoverReport) error {
	posted, err := c.postMessage(ctx, run)
	if err != nil {
		return fmt.Errorf("post cutover message: %w", err)
	}
	if posted {
		report.Messages++
	}
	if run.FailureExplanation == nil || run.FailureExplanation.Code != RunLoopCutoverCode {
		// Record the cause first; the cancel keeps it. A pass interrupted
		// here cancels the Run on the next pass.
		marked, err := c.markCanceledByCutover(ctx, run)
		if err != nil {
			return err
		}
		run = marked
	}
	if _, err := c.commands.CommandAgentRun(ctx, agentrun.AgentRunCommandRequest{
		Scope: run.Scope, RunID: run.ID, ExpectedRevision: run.Revision, Kind: agentrun.AgentRunCommandCancel,
		Actor: runLoopCutoverActor, Summary: "Stopped by a platform update", Visibility: ActivityVisibilityScope,
	}); err != nil {
		return err
	}
	return nil
}

func (c *runLoopCutover) markCanceledByCutover(ctx context.Context, current *AgentRun) (*AgentRun, error) {
	now := time.Now().UTC()
	run := cloneAgentRun(current)
	run.FailureExplanation = runLoopCutoverExplanation()
	run.Revision = current.Revision + 1
	run.UpdatedAt = now
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.cutover_stopping", Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID, TeamID: teamIDForRun(run),
		Actor: runLoopCutoverActor, Summary: "Run is being stopped by a platform update", Visibility: ActivityVisibilityPrivate, CreatedAt: now,
		Payload: map[string]interface{}{"code": RunLoopCutoverCode},
	}
	if _, err := c.store.UpdateAgentRunWithEvent(ctx, run, current.Revision, event, nil); err != nil {
		return nil, err
	}
	return run, nil
}

// postMessage answers a chat request the cutover stops, once per request.
// It reports whether a new message was written.
func (c *runLoopCutover) postMessage(ctx context.Context, run *AgentRun) (bool, error) {
	if c.conversations == nil || run.Kind != RunKindConversation {
		return false, nil
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	if !validOpaqueIdentifier(conversationID, 128) || !validOpaqueIdentifier(triggerID, 128) {
		return false, nil
	}
	requestKey := run.Scope.Kind + "\x00" + run.Scope.ID + "\x00" + conversationID + "\x00" + triggerID
	if c.posted[requestKey] {
		return false, nil
	}
	trigger, err := c.conversations.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if errors.Is(err, ErrChannelMessageNotFound) || errors.Is(err, ErrConversationNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !trigger.RequiresResponse {
		return false, nil
	}
	resolved, err := c.requestResolved(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return false, err
	}
	if resolved {
		c.posted[requestKey] = true
		return false, nil
	}
	for attempt := 0; ; attempt++ {
		conversation, err := c.conversations.GetConversation(ctx, run.Scope, conversationID)
		if errors.Is(err, ErrConversationNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if conversation.Status != ConversationStatusActive || conversation.Owner != run.Owner {
			return false, nil
		}
		result, err := c.conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
			Scope: run.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "openseal.conversation"},
			Intent: MessageIntentSystem, Content: RunLoopCutoverMessage, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			ReplyToMessageID: triggerID, ResolvesMessageID: triggerID,
			References:     []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}},
			IdempotencyKey: "conversation-run-canceled-result:" + run.ID,
		})
		if errors.Is(err, ErrRevisionConflict) && attempt+1 < runLoopCutoverRetries {
			continue
		}
		if err != nil {
			return false, err
		}
		c.posted[requestKey] = true
		return !result.Replayed, nil
	}
}

func (c *runLoopCutover) requestResolved(ctx context.Context, scope Scope, conversationID, triggerID string) (bool, error) {
	const pageSize = 100
	for after := int64(0); ; {
		messages, err := c.conversations.ListChannelMessages(ctx, ChannelMessageFilter{
			Scope: scope, ConversationID: conversationID, AfterSequence: after, Limit: pageSize,
		})
		if err != nil {
			return false, err
		}
		for _, message := range messages {
			if message.ResolvesMessageID == triggerID {
				return true, nil
			}
		}
		if len(messages) < pageSize {
			return false, nil
		}
		after = messages[len(messages)-1].Sequence
	}
}
