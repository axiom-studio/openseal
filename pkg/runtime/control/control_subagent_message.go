package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/google/uuid"
)

// message_subagent (settle phase) sends a subagent a follow-up.
//   - Still running: the message is steering on its Run (the intervene
//     command a new chat message uses); it reads it at its next step.
//   - Ended: a new child Run under the same id continues the subagent's
//     session, which is handed over to it in the transaction that creates it,
//     so its context is intact; the message is its first steering entry.
//
// With wait (the default) the parent parks until the subagent ends again and
// gets its answer, as with agent_delegate_reply.

// maximumSubagentMessageBytes is the intervene command's guidance limit.
const maximumSubagentMessageBytes = 16000

type messageSubagentArgs struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Wait    *bool  `json:"wait,omitempty"`
}

type messageSubagentHandler struct{}

func (messageSubagentHandler) Spec(agentrun.ControlAuthority) ToolSpec {
	return ToolSpec{
		Description: "Send a subagent a follow-up. A running one reads it at its next step; one that ended continues with its context intact.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"id", "message"},
			"properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "string", "description": "The subagent id (s1, ...)."},
				"message": map[string]interface{}{
					"type": "string", "minLength": 1, "maxLength": maximumSubagentMessageBytes,
				},
				"wait": map[string]interface{}{"type": "boolean", "description": "Wait for its answer (default true)."},
			},
		},
	}
}

func (messageSubagentHandler) Decode(raw json.RawMessage) (interface{}, error) {
	value, err := models.DecodeControlInto[messageSubagentArgs](raw)
	if err != nil {
		return nil, err
	}
	args := value.(*messageSubagentArgs)
	args.ID, args.Message = strings.TrimSpace(args.ID), strings.TrimSpace(args.Message)
	if args.ID == "" || args.Message == "" {
		return nil, errors.New("id and message are required")
	}
	if len(args.Message) > maximumSubagentMessageBytes || !utf8.ValidString(args.Message) {
		return nil, fmt.Errorf("message must be valid text of at most %d bytes", maximumSubagentMessageBytes)
	}
	return args, nil
}

func (a *messageSubagentArgs) waits() bool { return a.Wait == nil || *a.Wait }

func (messageSubagentHandler) Run(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Result, error) {
	args := cc.Arguments.(*messageSubagentArgs)
	subagents, err := LoadSubagents(ctx, x.Sessions, x.Portfolio, cc.Run, cc.Entry.ID)
	if err != nil {
		return Result{}, err
	}
	ref, child, ok := subagents.lookup(args.ID)
	if !ok {
		return x.refuse(ctx, cc, subagents.unknown(args.ID))
	}
	// A stable intervention id makes a repeated settle send the message once.
	messageID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("openseal:subagent-message:"+cc.Entry.ID)).String()
	actor := models.ActivityActor{Type: "agent", ID: cc.Run.AssignedAgentID}
	if !models.IsTerminalAgentRunStatus(child.Status) {
		steered, err := x.steerSubagent(ctx, child, actor, messageID, args.Message)
		if err != nil {
			return Result{}, err
		}
		if steered != nil {
			subagents.runs[steered.ID] = steered
			if !args.waits() {
				return x.complete(ctx, cc, models.ControlJSONResult(models.ToolResultSucceeded, map[string]interface{}{
					"id": ref.ID, "status": "running", "message": "Sent; " + ref.ID + " reads it at its next step.",
				}), nil)
			}
			refs := []SubagentRef{ref}
			return x.parkOnSubagents(ctx, cc, models.FanInModeAll, refs, subagents, &EntryMeta{Waited: refs})
		}
		// It ended before the message landed: continue it instead.
		if child, err = x.Portfolio.GetAgentRun(ctx, cc.Run.Scope, child.ID); err != nil {
			return Result{}, err
		}
	}
	return x.continueSubagent(ctx, cc, subagents, ref, child, models.AgentRunIntervention{ID: messageID, Actor: actor, Instruction: args.Message}, args.waits())
}

// steerSubagent adds message to a running child's interventions. It returns
// nil when the child ended first.
func (x *Executor) steerSubagent(ctx context.Context, child *models.AgentRun, actor models.ActivityActor, messageID, message string) (*models.AgentRun, error) {
	commands, err := x.runCommands()
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 4; attempt++ {
		result, err := commands.CommandAgentRun(ctx, agentrun.CommandRequest{
			Scope: child.Scope, RunID: child.ID, ExpectedRevision: child.Revision, Kind: agentrun.CommandIntervene,
			Actor: actor, Instruction: message, InterventionID: messageID, Summary: "Parent sent a follow-up",
		})
		switch {
		case err == nil:
			return result.Run, nil
		case errors.Is(err, models.ErrInvalidRunTransition):
			return nil, nil
		case !errors.Is(err, models.ErrRevisionConflict):
			return nil, err
		}
		if child, err = x.Portfolio.GetAgentRun(ctx, child.Scope, child.ID); err != nil {
			return nil, err
		}
		if child == nil || models.IsTerminalAgentRunStatus(child.Status) {
			return nil, nil
		}
	}
	return nil, fmt.Errorf("steer subagent run %s: %w", child.ID, models.ErrRevisionConflict)
}

// continueSubagent starts the next Run of an ended subagent on its session.
func (x *Executor) continueSubagent(ctx context.Context, cc agentrun.ControlContext, subagents *Subagents, ref SubagentRef, child *models.AgentRun, message models.AgentRunIntervention, wait bool) (Result, error) {
	if child.SessionID == "" {
		return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, "subagent_not_started",
			ref.ID+" ended before it started; start a new subagent instead.", nil))
	}
	if refusal := subagentCapacityRefusal(subagents, 1); refusal != nil {
		return x.refuse(ctx, cc, refusal)
	}
	forkID := "follow-up-" + cc.Entry.ID
	agentID := ""
	if child.AssignedAgentID != cc.Run.AssignedAgentID {
		agentID = child.AssignedAgentID
	}
	branches := []agentrun.RunForkBranch{{
		ID: ref.ID, Goal: message.Instruction, AssignedAgentID: agentID, Checkpoint: map[string]interface{}{},
		Continues: &agentrun.ForkContinuation{SessionID: child.SessionID, FromRunID: child.ID, Message: message},
	}}
	if err := x.fundBranches(cc, branches); err != nil {
		var insufficient *modelstep.ControlBudgetInsufficientError
		if errors.As(err, &insufficient) {
			return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, insufficient.Code(), insufficient.Message(), nil))
		}
		return Result{}, err
	}
	next := SubagentRef{ID: ref.ID, RunID: agentrun.StableForkIdentifier(cc.Run.ID, forkID, "child:"+ref.ID), Background: !wait}
	spec := ForkSpec{
		ForkID: forkID, Branches: branches, ChildKind: models.RunKindAgentWork, NoSessionForks: true,
		Policy:    models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureWait},
		Subagents: []SubagentRef{next}, Background: !wait,
	}
	if !wait {
		spec.BackgroundResult = models.ControlJSONResult(models.ToolResultSucceeded, map[string]interface{}{
			"id": ref.ID, "status": "running", "message": ref.ID + " continues in the background; a note tells you when it ends.",
		})
	}
	return x.Fork(ctx, cc, spec)
}

func (messageSubagentHandler) Resume(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Resume, error) {
	return x.resumeSubagentWait(ctx, cc, false)
}

// runCommands is the Run command service over the executor's store: the
// cancel and intervene paths every other caller uses.
func (x *Executor) runCommands() (*agentrun.RunCommandService, error) {
	store, ok := x.Portfolio.(models.RunCommandStore)
	if !ok {
		return nil, errors.New("control executor portfolio is not a run command store; subagent follow-ups need it")
	}
	return agentrun.NewRunCommandService(store, models.WithClock(x.now)), nil
}
