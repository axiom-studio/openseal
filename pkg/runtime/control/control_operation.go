package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

// run_operation (section 04 §9): a settle-phase control tool that starts one
// of the Agent's callable operations (an already reviewed runbook entrypoint)
// as a deterministic child Run. The fork coordinator moves the tool entry to
// awaiting_child, creates the child and parks the source on its dependency
// group in one transaction, so the park is committed. Each call has its own
// fork id ("op-" + entry id), so a Run may start several operations. When the
// child ends, the resume turns its saved output into this entry's result; a
// failed operation is an is_error result, never a Run failure (D6).

const (
	operationNotAuthorizedCode = "operation_not_authorized"
	operationInputInvalidCode  = "operation_input_invalid"
	operationRecursiveCode     = "operation_recursive"
	operationBudgetCode        = "budget_insufficient"
	operationLostCode          = "operation_lost"

	operationBranchID = "operation"
)

type operationArgs struct {
	Entrypoint string                 `json:"entrypoint"`
	Arguments  map[string]interface{} `json:"arguments"`
}

type operationHandler struct{}

// Spec carries no operation list: the operations and their input schemas are
// in the capabilities note, and the handler authorizes the entrypoint.
func (operationHandler) Spec(a agentrun.ControlAuthority) ControlToolSpec {
	return ControlToolSpec{
		Description: "Start one of your callable operations as a separate Run; its result comes back to you as this tool's result. The operations and their input schemas are listed in the capabilities note.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"entrypoint", "arguments"},
			"properties": map[string]interface{}{
				"entrypoint": map[string]interface{}{"type": "string", "minLength": 1, "description": "An operation entrypoint from the capabilities note."},
				"arguments": map[string]interface{}{
					"type":        "object",
					"description": "Must match the operation's input schema listed in the capabilities note.",
				},
			},
		},
	}
}

func (operationHandler) Decode(raw json.RawMessage) (interface{}, error) {
	value, err := models.DecodeControlInto[operationArgs](raw)
	if err != nil {
		return nil, err
	}
	args := value.(*operationArgs)
	args.Entrypoint = strings.TrimSpace(args.Entrypoint)
	if args.Entrypoint == "" {
		return nil, errors.New("entrypoint is required")
	}
	if args.Arguments == nil {
		return nil, errors.New("arguments must be an object")
	}
	return args, nil
}

func (operationHandler) Run(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext) (ControlResult, error) {
	args := cc.Arguments.(*operationArgs)
	run := cc.Run
	op, ok := authorizedOperation(cc.Authority.Operations, args.Entrypoint)
	if !ok {
		return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, operationNotAuthorizedCode,
			"That operation is not available to you; use one listed in this tool's description.",
			map[string]interface{}{"entrypoint": args.Entrypoint}))
	}
	schema := op.InputSchema
	if len(schema) == 0 {
		schema = map[string]interface{}{"type": "object"}
	}
	if err := runbook.ValidateInterfaceInput(schema, args.Arguments); err != nil {
		return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, operationInputInvalidCode,
			fmt.Sprintf("The arguments do not match the input schema of %s: %v.", op.Entrypoint, err), nil))
	}
	if err := controlRejectRecursiveRunbook(ctx, x.Portfolio, run, op); err != nil {
		if errors.Is(err, errRecursiveOperation) {
			return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, operationRecursiveCode,
				"This operation is already running above you; it cannot start itself again.", nil))
		}
		return ControlResult{}, err
	}
	// A callable operation is already reviewed work: its budget is the
	// kernel's, not the model's. An unbounded Run starts an unbounded
	// operation; a bounded Run gives it all its remaining child capacity.
	var budget *models.BudgetPolicy
	if run.Budget != nil {
		allocated, err := models.CompleteChildBudgetAllocation(run, &models.BudgetPolicy{})
		if err != nil {
			return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, operationBudgetCode,
				"This Run's remaining budget cannot fund the operation.", nil))
		}
		budget = allocated
	}
	summary := "Run " + operationName(op)
	childContext, err := x.operationChildContext(ctx, run, op, args.Arguments, summary)
	if err != nil {
		return ControlResult{}, err
	}
	forkID := "op-" + cc.Entry.ID
	result, err := x.Fork(ctx, cc, ForkSpec{
		ForkID: forkID,
		Policy: models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Branches: []agentrun.RunForkBranch{{
			ID: operationBranchID, Goal: summary, AssignedAgentID: run.AssignedAgentID,
			Entrypoint: op.Entrypoint, Context: childContext, Checkpoint: map[string]interface{}{}, Budget: budget,
		}},
		NoSessionForks: true, // deterministic runbook Run: no session (D3)
	})
	if err != nil || result.Kind != ControlParked {
		return result, err
	}
	if result.Park != nil {
		result.Park.Summary = "Waiting for " + operationName(op)
	}
	x.projectOperationStart(ctx, run, summary, agentrun.StableForkIdentifier(run.ID, forkID, "child:"+operationBranchID))
	return result, nil
}

func authorizedOperation(operations []agentrun.HostedRunbookOperation, entrypoint string) (agentrun.HostedRunbookOperation, bool) {
	for _, op := range operations {
		if op.Entrypoint == entrypoint {
			return op, true
		}
	}
	return agentrun.HostedRunbookOperation{}, false
}

func operationName(op agentrun.HostedRunbookOperation) string {
	if name := strings.TrimSpace(op.Name); name != "" {
		return name
	}
	return op.Entrypoint
}

// operationChildContext is the child's context: the arguments, the
// invocation record and the runbook definition identity.
func (x *ControlExecutor) operationChildContext(ctx context.Context, run *models.AgentRun, op agentrun.HostedRunbookOperation, arguments map[string]interface{}, summary string) (map[string]interface{}, error) {
	childContext := maps.Clone(arguments)
	if childContext == nil {
		childContext = map[string]interface{}{}
	}
	invocation := map[string]interface{}{"summary": summary, "arguments": maps.Clone(arguments)}
	var reader runbookMessageReader
	if x.Reporting != nil {
		reader = x.Reporting
	}
	sourceMessage, err := runbookSourceMessage(ctx, reader, run)
	if err != nil {
		return nil, err
	}
	if sourceMessage != nil {
		invocation["sourceMessage"] = sourceMessage
	}
	childContext[agentrun.RunbookInvocationContextKey] = invocation
	if id, version := strings.TrimSpace(op.DefinitionID), strings.TrimSpace(op.DefinitionVersion); id != "" && version != "" {
		childContext["runbookDefinitionId"] = id
		childContext["runbookDefinitionVersion"] = version
		childContext["runbookEntrypoint"] = strings.TrimSpace(op.Entrypoint)
	}
	return childContext, nil
}

// projectOperationStart posts "Started: …" for the child. The child is
// already committed, so a failure here is only logged: it must never fail the
// source or start the operation twice.
func (x *ControlExecutor) projectOperationStart(ctx context.Context, run *models.AgentRun, summary, childID string) {
	if x.Reporting == nil || run.Kind != models.RunKindConversation {
		return
	}
	child, err := x.Portfolio.GetAgentRun(ctx, run.Scope, childID)
	if err == nil && child == nil {
		err = models.ErrRunNotFound
	}
	if err == nil {
		err = conversationrun.ProjectConversationRunbookStart(ctx, x.Reporting, run, summary, child)
	}
	if err != nil {
		slog.Warn("failed to project operation start into conversation", "runId", run.ID, "childRunId", childID, "error", err)
	}
}

var errRecursiveOperation = errors.New("recursive operation invocation")

// controlRejectRecursiveRunbook refuses an operation that is already running
// in the Run's ancestry (same definition and version, or, for operations
// without a definition identity, the same entrypoint on the same Agent).
// Refusals wrap errRecursiveOperation; other errors are store errors.
func controlRejectRecursiveRunbook(ctx context.Context, portfolio models.PortfolioStore, run *models.AgentRun, op agentrun.HostedRunbookOperation) error {
	if run == nil {
		return nil
	}
	wantedID := strings.TrimSpace(op.DefinitionID)
	wantedVersion := strings.TrimSpace(op.DefinitionVersion)
	wantedEntrypoint := strings.TrimSpace(op.Entrypoint)
	visited := make(map[string]struct{})
	current := run
	for depth := 0; current != nil && depth < 256; depth++ {
		if _, duplicate := visited[current.ID]; duplicate {
			return fmt.Errorf("%w: %q: Run ancestry contains a cycle at %s", errRecursiveOperation, wantedEntrypoint, current.ID)
		}
		visited[current.ID] = struct{}{}
		ancestorID, ancestorVersion, ancestorEntrypoint := runbookInvocationIdentity(current)
		exactIdentity := wantedID != "" && wantedVersion != "" &&
			ancestorID == wantedID && ancestorVersion == wantedVersion &&
			(ancestorEntrypoint == "" || ancestorEntrypoint == wantedEntrypoint)
		entrypointIdentity := wantedEntrypoint != "" && ancestorID == "" &&
			strings.TrimSpace(current.Entrypoint) == wantedEntrypoint &&
			strings.TrimSpace(current.AssignedAgentID) == strings.TrimSpace(run.AssignedAgentID)
		if exactIdentity || entrypointIdentity {
			return fmt.Errorf("%w: %q is already active in ancestor Run %s", errRecursiveOperation, wantedEntrypoint, current.ID)
		}
		if strings.TrimSpace(current.ParentRunID) == "" {
			return nil
		}
		if portfolio == nil {
			return errors.New("operation ancestry check needs the portfolio store")
		}
		parent, err := portfolio.GetAgentRun(ctx, current.Scope, current.ParentRunID)
		if err != nil {
			return fmt.Errorf("resolve operation ancestry at %s: %w", current.ID, err)
		}
		current = parent
	}
	if current != nil {
		return fmt.Errorf("%w: %q: Run ancestry exceeds the supported depth", errRecursiveOperation, wantedEntrypoint)
	}
	return nil
}

// Resume: a group still waiting re-parks; otherwise the child's saved output
// becomes the entry's result.
func (operationHandler) Resume(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext) (ControlResume, error) {
	if !x.Forks.Configured() || x.Portfolio == nil {
		return ControlResume{}, errors.New("control executor has no fork coordinator")
	}
	args := cc.Arguments.(*operationArgs)
	meta, err := DecodeControlEntryMeta(cc.Entry)
	if err != nil {
		return ControlResume{}, err
	}
	groupID := ""
	if meta != nil {
		groupID = meta.GroupID
	}
	if groupID == "" && cc.Entry.Tool.Wait != nil {
		groupID = cc.Entry.Tool.Wait.GroupID
	}
	var group *models.RunDependencyGroup
	if groupID != "" {
		group, err = x.Forks.DependencyGroup(ctx, cc.Run.Scope, groupID)
		if err != nil && !errors.Is(err, models.ErrDependencyGroupNotFound) {
			return ControlResume{}, err
		}
	}
	var child *models.AgentRun
	if group != nil && meta != nil && len(meta.ChildRunIDs) > 0 {
		if group.Status == models.RunDependencyGroupWaiting || group.Status == models.RunDependencyGroupOpen {
			return ControlResume{Park: &agentrun.LoopPark{
				Status: models.AgentRunStatusWaitingForDependency, Wake: &models.WakeCondition{Type: "run_dependencies", Reference: group.ID},
				Summary: "Waiting for operation " + args.Entrypoint,
			}}, nil
		}
		child, err = x.Portfolio.GetAgentRun(ctx, cc.Run.Scope, meta.ChildRunIDs[0])
		if err != nil {
			return ControlResume{}, err
		}
	}
	if child == nil {
		result, err := x.complete(ctx, cc, ErrorResult(models.ToolResultFailed, operationLostCode,
			"The operation can no longer be found; start it again if it is still needed.", nil), nil)
		return ControlResume{Resolved: true, Result: result}, err
	}
	output, _ := conversationrun.ConversationOperationSavedOutput(ctx, x.Portfolio, child)
	value := map[string]interface{}{
		"status": string(child.Status), "entrypoint": args.Entrypoint, "runId": child.ID,
		"output": truncateControlText(output, child.ID),
	}
	if message := strings.TrimSpace(child.Error); message != "" {
		value["error"] = truncateControlText(message, child.ID)
	}
	status := models.ToolResultSucceeded
	switch child.Status {
	case models.AgentRunStatusCompleted:
	case models.AgentRunStatusCanceled:
		status = models.ToolResultCanceled
	default:
		status = models.ToolResultFailed
	}
	result, err := x.complete(ctx, cc, models.ControlJSONResult(status, value), nil)
	return ControlResume{Resolved: true, Result: result}, err
}

func runbookInvocationIdentity(run *models.AgentRun) (string, string, string) {
	if run == nil {
		return "", "", ""
	}
	contextValues := run.Context
	if triggerInput, ok := contextValues["triggerInput"].(map[string]interface{}); ok {
		contextValues = triggerInput
	}
	definitionID, _ := contextValues["runbookDefinitionId"].(string)
	definitionVersion, _ := contextValues["runbookDefinitionVersion"].(string)
	entrypoint, _ := contextValues["runbookEntrypoint"].(string)
	if strings.TrimSpace(entrypoint) == "" {
		entrypoint = run.Entrypoint
	}
	return strings.TrimSpace(definitionID), strings.TrimSpace(definitionVersion), strings.TrimSpace(entrypoint)
}
