package runloop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/google/uuid"
)

// CutoverCode names the cutover: the FailureExplanation.Code of every
// Run it cancels (SPEC §2.8) and the actor of its adopt and cancel events.
const CutoverCode = "run_loop_cutover"

// CutoverMessage answers each unanswered chat request the cutover
// stopped. It is also the canceled Run's FailureExplanation.Message.
const CutoverMessage = "This request was stopped by a platform update. Send it again to continue."

// runLoopCutoverRetries bounds the re-reads after a revision race on one Run
// or one conversation.
const runLoopCutoverRetries = 5

// CutoverConversations posts the visible cancel message. It is
// satisfied by *conversation.Service.
type CutoverConversations interface {
	GetConversation(ctx context.Context, scope models.Scope, conversationID string) (*models.Conversation, error)
	GetChannelMessage(ctx context.Context, scope models.Scope, conversationID, messageID string) (*models.ChannelMessage, error)
	ListChannelMessages(ctx context.Context, filter models.ChannelMessageFilter) ([]*models.ChannelMessage, error)
	PostChannelMessage(ctx context.Context, req runtimeconversation.PostChannelMessageRequest) (*models.ChannelMessageCommitResult, error)
}

// CutoverReport counts what one cutover pass did.
type CutoverReport struct {
	Adopted  int
	Canceled int
	Messages int
	Skipped  int
}

// Cutover moves the store onto the run loop (A16, SPEC §4.8). It must
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
func Cutover(ctx context.Context, store models.RunLoopCutoverStore, conversations CutoverConversations) (CutoverReport, error) {
	var report CutoverReport
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
	store         models.RunLoopCutoverStore
	conversations CutoverConversations
	commands      *agentrun.RunCommandService
	// posted holds the requests answered in this pass, so a lane whose
	// request also reached a child Run gets one message.
	posted map[string]bool
}

var runLoopCutoverActor = models.ActivityActor{Type: "runtime", ID: CutoverCode}

func (c *runLoopCutover) cutRun(ctx context.Context, ref models.RunLoopCutoverRef, report *CutoverReport) error {
	for attempt := 0; ; attempt++ {
		run, err := c.store.GetAgentRun(ctx, ref.Scope, ref.ID)
		if err != nil {
			return err
		}
		if run == nil || models.IsTerminalAgentRunStatus(run.Status) || run.Engine != "" {
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
		if errors.Is(err, models.ErrRevisionConflict) && attempt+1 < runLoopCutoverRetries {
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
func (c *runLoopCutover) adoptable(ctx context.Context, run *models.AgentRun) (bool, error) {
	if run.Status != models.AgentRunStatusQueued || run.LastAppliedTurn != 0 {
		return false, nil
	}
	if needsLaneSession(run) {
		if task, _ := run.Context[models.ConversationTaskContextKey].(string); task != "" {
			return false, nil
		}
	}
	turns, err := c.store.ListAgentTurns(ctx, models.AgentTurnFilter{Scope: run.Scope, RunID: run.ID, Limit: 1})
	if err != nil {
		return false, err
	}
	return len(turns) == 0, nil
}

func (c *runLoopCutover) adopt(ctx context.Context, current *models.AgentRun) error {
	now := time.Now().UTC()
	run := models.CloneAgentRun(current)
	run.Engine = models.RunEngineLoop
	if needsLaneSession(run) {
		// Old chat Runs predate the lane binding; the loop opens the lane
		// session only for a Run bound to it, as the scheduler binds new ones.
		run.SessionID = models.ConversationSessionID(run.Scope, run.ConcurrencyKey)
	}
	run.Revision = current.Revision + 1
	run.UpdatedAt = now
	event := &models.ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.engine_adopted", Severity: models.ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID, TeamID: models.TeamIDForRun(run),
		Actor: runLoopCutoverActor, Summary: "Run moved to the updated agent runtime", Visibility: models.ActivityVisibilityPrivate, CreatedAt: now,
		Payload: map[string]interface{}{"engine": models.RunEngineLoop, "code": CutoverCode, "sessionId": run.SessionID},
	}
	_, err := c.store.UpdateAgentRunWithEvent(ctx, run, current.Revision, event, nil)
	return err
}

// needsLaneSession reports an acting-Agent conversation Run with no session:
// one created before chat Runs were bound to their lane session. Team routing
// Runs (no assigned Agent) have no session.
func needsLaneSession(run *models.AgentRun) bool {
	return run.Kind == models.RunKindConversation && run.SessionID == "" && run.AssignedAgentID != ""
}

func runLoopCutoverExplanation() *models.RunFailureExplanation {
	return &models.RunFailureExplanation{Kind: "execution", Code: CutoverCode, Message: CutoverMessage}
}

// cancel posts the lane's message first and then cancels the Run, so a pass
// interrupted between the two still leaves the message: the next pass finds
// the request answered and only cancels.
func (c *runLoopCutover) cancel(ctx context.Context, run *models.AgentRun, report *CutoverReport) error {
	posted, err := c.postMessage(ctx, run)
	if err != nil {
		return fmt.Errorf("post cutover message: %w", err)
	}
	if posted {
		report.Messages++
	}
	if run.FailureExplanation == nil || run.FailureExplanation.Code != CutoverCode {
		// Record the cause first; the cancel keeps it. A pass interrupted
		// here cancels the Run on the next pass.
		marked, err := c.markCanceledByCutover(ctx, run)
		if err != nil {
			return err
		}
		run = marked
	}
	if _, err := c.commands.CommandAgentRun(ctx, agentrun.CommandRequest{
		Scope: run.Scope, RunID: run.ID, ExpectedRevision: run.Revision, Kind: agentrun.CommandCancel,
		Actor: runLoopCutoverActor, Summary: "Stopped by a platform update", Visibility: models.ActivityVisibilityScope,
	}); err != nil {
		return err
	}
	return nil
}

func (c *runLoopCutover) markCanceledByCutover(ctx context.Context, current *models.AgentRun) (*models.AgentRun, error) {
	now := time.Now().UTC()
	run := models.CloneAgentRun(current)
	run.FailureExplanation = runLoopCutoverExplanation()
	run.Revision = current.Revision + 1
	run.UpdatedAt = now
	event := &models.ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.cutover_stopping", Severity: models.ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID, TeamID: models.TeamIDForRun(run),
		Actor: runLoopCutoverActor, Summary: "Run is being stopped by a platform update", Visibility: models.ActivityVisibilityPrivate, CreatedAt: now,
		Payload: map[string]interface{}{"code": CutoverCode},
	}
	if _, err := c.store.UpdateAgentRunWithEvent(ctx, run, current.Revision, event, nil); err != nil {
		return nil, err
	}
	return run, nil
}

// postMessage answers a chat request the cutover stops, once per request.
// It reports whether a new message was written.
func (c *runLoopCutover) postMessage(ctx context.Context, run *models.AgentRun) (bool, error) {
	if c.conversations == nil || run.Kind != models.RunKindConversation {
		return false, nil
	}
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	if !models.IsValidOpaqueIdentifier(conversationID, 128) || !models.IsValidOpaqueIdentifier(triggerID, 128) {
		return false, nil
	}
	requestKey := run.Scope.Kind + "\x00" + run.Scope.ID + "\x00" + conversationID + "\x00" + triggerID
	if c.posted[requestKey] {
		return false, nil
	}
	trigger, err := c.conversations.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if errors.Is(err, models.ErrChannelMessageNotFound) || errors.Is(err, models.ErrConversationNotFound) {
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
		if errors.Is(err, models.ErrConversationNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if conversation.Status != models.ConversationStatusActive || conversation.Owner != run.Owner {
			return false, nil
		}
		result, err := c.conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
			Scope: run.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "openseal.conversation"},
			Intent: models.MessageIntentSystem, Content: CutoverMessage, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
			ReplyToMessageID: triggerID, ResolvesMessageID: triggerID,
			References:     []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}},
			IdempotencyKey: "conversation-run-canceled-result:" + run.ID,
		})
		if errors.Is(err, models.ErrRevisionConflict) && attempt+1 < runLoopCutoverRetries {
			continue
		}
		if err != nil {
			return false, err
		}
		c.posted[requestKey] = true
		return !result.Replayed, nil
	}
}

func (c *runLoopCutover) requestResolved(ctx context.Context, scope models.Scope, conversationID, triggerID string) (bool, error) {
	const pageSize = 100
	for after := int64(0); ; {
		messages, err := c.conversations.ListChannelMessages(ctx, models.ChannelMessageFilter{
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
