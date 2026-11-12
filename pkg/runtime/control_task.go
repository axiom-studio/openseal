package runtime

import (
	"context"
	"encoding/json"
	"errors"
)

// conversation_start_task (section 04 §7, A4): a settle-phase control tool
// that admits independent work from a foreground conversation. The admission
// transaction creates the task and its work Run (whose session forks the chat
// session just before this step's assistant entry), completes this tool entry
// with the started result and completes the source Run with the
// acknowledgment as its reply. There is no park: the Run ends here.

type startTaskHandler struct{}

func (startTaskHandler) spec(ControlAuthority) ControlToolSpec {
	return ControlToolSpec{
		Description: "Start one durable background task for substantial requested work and reply with a brief acknowledgment. " +
			"Call it alone; it ends your reply to this message.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"goal", "acknowledgment"},
			"properties": map[string]interface{}{
				"goal": map[string]interface{}{
					"type": "string", "minLength": 1, "maxLength": maximumStartTaskGoalBytes,
					"description": "Self-contained work requested by the user, with the requested result and only non-secret supplied context.",
				},
				"acknowledgment": map[string]interface{}{
					"type": "string", "minLength": 1, "maxLength": maximumStartTaskAcknowledgmentBytes,
					"description": "One brief natural sentence in your persona acknowledging the work. It is posted only after the task is saved and is your whole reply to this message.",
				},
			},
		},
	}
}

func (startTaskHandler) decode(raw json.RawMessage) (interface{}, error) {
	args, err := parseStartTaskArgs(raw)
	if err != nil {
		return nil, err
	}
	return args, nil
}

func startTaskRejected(message string) *ToolResult {
	return controlErrorResult(ToolResultRefused, "task_rejected", message, nil)
}

func startTaskNewMessage() *ToolResult {
	return controlErrorResult(ToolResultRefused, "new_message",
		"A new message arrived before the task started; read it and decide again.", nil)
}

func (startTaskHandler) run(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResult, error) {
	args := cc.Arguments.(*startTaskArgs)
	// Rule 1 (§7.2): the only tool call of its assistant entry. The loop
	// refuses mixed entries before the inline phase; this keeps a resumed
	// or directly settled entry to the same rule.
	if cc.Assistant == nil || cc.Assistant.Assistant == nil || len(cc.Assistant.Assistant.ToolCalls) != 1 {
		return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, controlTaskNotAloneCode,
			"Call conversation_start_task alone; other work belongs inside the task.", nil))
	}
	if x.Tasks == nil {
		return x.refuse(ctx, cc, startTaskRejected("Background tasks are not available here; answer in this conversation instead."))
	}
	if err := validateConversationTaskStartAuthority(cc.Run, cc.Authority.TaskStart); err != nil {
		return x.refuse(ctx, cc, startTaskRejected("A background task cannot be started from this conversation; answer here instead."))
	}
	run := cc.Run
	for attempt := 0; ; attempt++ {
		// Rule 2: steering the loop has not read yet.
		if len(run.PendingInterventions) > run.ConsumedInterventions {
			return x.refuse(ctx, cc, startTaskNewMessage())
		}
		budget, err := controlRunBudget(run, "")
		if err != nil {
			return ControlResult{}, err
		}
		result, err := x.Tasks.Start(ctx, StartConversationTaskRequest{
			Scope: run.Scope, SourceRunID: run.ID, ExpectedSourceRevision: run.Revision, WorkerID: cc.WorkerID,
			TurnID: cc.Step.ID, ToolEntryID: cc.Entry.ID, AssignedAgentID: cc.Authority.AgentID,
			TaskKey: startTaskKey(cc.Entry.ID), Goal: args.Goal, Acknowledgment: args.Acknowledgment,
			Budget: controlTaskBudget(budget),
		})
		switch {
		case err == nil:
			return x.startTaskResult(ctx, cc, result)
		case errors.Is(err, ErrSteeringArrived):
			return x.refuse(ctx, cc, startTaskNewMessage())
		case errors.Is(err, ErrRevisionConflict) && attempt == 0:
			// One reload retry (§3.2 rule 5): another write moved the Run
			// revision under the claim (a heartbeat, a steering append).
			reloaded, getErr := x.Portfolio.GetAgentRun(ctx, run.Scope, run.ID)
			if getErr != nil {
				return ControlResult{}, getErr
			}
			if reloaded == nil || reloaded.Status != AgentRunStatusRunning || reloaded.LeaseOwner != cc.WorkerID {
				return ControlResult{}, ErrLeaseLost
			}
			run = reloaded
			cc.Run = reloaded
			continue
		case errors.Is(err, ErrInvalidConversationTask), errors.Is(err, ErrConversationTaskConflict):
			return x.refuse(ctx, cc, startTaskRejected("The task could not be started: "+err.Error()+". Answer in this conversation instead."))
		default:
			return ControlResult{}, err
		}
	}
}

// startTaskResult maps an admission to the loop outcome. A fresh or replayed
// independent task ended the source Run in its transaction. A replayed
// continuation means this Run was already promoted to background work: the
// redundant call gets an error result and the Run continues as that work.
func (x *ControlExecutor) startTaskResult(ctx context.Context, cc ControlContext, result *ConversationTaskResult) (ControlResult, error) {
	if result == nil || result.Task == nil {
		return ControlResult{}, errors.New("conversation task admission returned no task")
	}
	if result.Task.Mode == ConversationTaskModeContinuation {
		return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, "already_background",
			"This conversation already continues as a background task; keep working on it here.", map[string]interface{}{"taskId": result.Task.ID}))
	}
	if result.SourceRun == nil || result.SourceRun.Status != AgentRunStatusCompleted {
		return ControlResult{}, errors.New("conversation task admission returned no committed source completion")
	}
	entry, err := x.Sessions.GetSessionEntry(ctx, cc.Run.Scope, cc.Entry.ID)
	if err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Kind: ControlEnded, Run: result.SourceRun, Entry: entry}, nil
}
