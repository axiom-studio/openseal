package modelstep

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/workspace"
	"go.uber.org/zap"
)

// HostToolInterruptedMessage is the result of a host write whose outcome is
// unknown: the host may have begun it before it failed or the worker died.
const HostToolInterruptedMessage = "This call was interrupted; its outcome is unknown. Check the files (or the command's effects) before retrying."

// Model-visible results of host failures (section 07 §6.5).
const (
	hostToolUnusableResultMessage = "The tool returned an unusable result."
	hostToolUnavailableMessage    = "The workspace host is unavailable right now; try again shortly."
	hostToolCouldNotRunMessage    = "The tool could not run."
	HostToolUnsupportedMessage    = "This host cannot run this tool."
	maxHostToolFailureMessage     = 2000
)

// hostToolUnavailableRetries are the waits before retrying a call the host
// did not start because it was unavailable (the same InvocationID each time).
var hostToolUnavailableRetries = []time.Duration{time.Second, 3 * time.Second}

// HostToolCall is one host tool call of a running step, as the loop hands it over.
type HostToolCall struct {
	Run         *models.AgentRun            // running, leased by this worker
	Step        *models.AgentTurn           // the running step row (LeaseOwner/LeaseExpiresAt live)
	Binding     *agentrun.TurnRunnerBinding // the claim's resolved binding
	SessionID   string
	ToolEntryID string
	ToolCallID  string
	Spec        HostToolSpec
	Arguments   map[string]interface{} // model arguments, already schema-validated by the loop
	Workspace   workspace.Authority
	CommandPins []workspace.CommandPin // the claim's routine-script pins, may be nil
}

// HostToolOutcome is what the loop records as the tool entry's terminal state.
type HostToolOutcome struct {
	State     models.ToolEntryState // completed | error | interrupted
	Result    *models.ToolResult    // model-visible
	OriginRef string                // HostToolCommandRef(ToolEntryID) for run_command
	// Arguments are the arguments as executed (pin attached, model
	// expectedDigest stripped).
	Arguments map[string]interface{}
	// PinnedRun is set when a pinned run_command ran, was refused, or its
	// outcome is unknown. Validate against the pins already ran: a record that
	// failed it carries why in Refused.
	PinnedRun *workspace.CommandRecord
}

// KernelPatch is the outcome's kernel-owned tool entry fields for the
// terminal transition.
func (o HostToolOutcome) KernelPatch() (*models.ToolEntryKernelPatch, error) {
	patch := &models.ToolEntryKernelPatch{ResultDigest: ToolResultDigest(o.Result), OriginRef: o.OriginRef}
	if o.PinnedRun != nil {
		encoded, err := json.Marshal(o.PinnedRun)
		if err != nil {
			return nil, err
		}
		patch.PinnedRun = encoded
	}
	return patch, nil
}

// HostToolDispatcher runs host tool calls of the loop's steps on the step host.
type HostToolDispatcher struct {
	host    models.HostToolExecutor
	actions *action.ActionCoordinator // OpenNestedActionTurn
	sleep   func(context.Context, time.Duration) error
	logger  *zap.SugaredLogger
}

func NewHostToolDispatcher(host models.HostToolExecutor, actions *action.ActionCoordinator, logger *zap.SugaredLogger) *HostToolDispatcher {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &HostToolDispatcher{host: host, actions: actions, sleep: sleepContext, logger: logger}
}

func sleepContext(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run executes one host tool call. Every host failure becomes a tool entry
// state; a host tool never fails the Run. ctx cancellation (lease lost, Run
// canceled) returns ok=false: the loop leaves the entry running for resume.
func (d *HostToolDispatcher) Run(ctx context.Context, call HostToolCall) (outcome HostToolOutcome, ok bool) {
	request := d.request(call)
	command := request.Operation == workspace.OperationRunCommand
	outcome = HostToolOutcome{Arguments: request.Arguments}
	if command {
		outcome.OriginRef = request.CommandRef
	}
	if len(request.VMTools) > 0 && d.actions != nil {
		closeNested := d.actions.OpenNestedActionTurn(call.Run, call.Step, call.Binding)
		defer closeNested()
	}

	result, err := d.execute(ctx, request)
	if ctx.Err() != nil {
		return HostToolOutcome{}, false
	}
	write := call.Spec.SideEffect != capability.SideEffectRead
	var usable map[string]interface{}
	switch {
	case err == nil:
		if verr := result.Validate(request); verr != nil {
			d.logger.Warnw("host tool returned an unusable result", "runId", request.RunID, "stepId", request.StepID,
				"toolEntryId", request.ToolEntryID, "operation", request.Operation, "error", verr,
				"tooLarge", errors.Is(verr, models.ErrSessionEntryTooLarge))
			outcome.State, outcome.Result = hostToolFailureResult(write, hostToolUnusableResultMessage)
			break
		}
		usable = hostToolResultObject(result.Content)
		status, state := models.ToolResultSucceeded, models.ToolEntryCompleted
		if result.IsError {
			status, state = models.ToolResultFailed, models.ToolEntryError
		}
		outcome.State = state
		outcome.Result = &models.ToolResult{Status: status, IsError: result.IsError, Content: models.CloneContentParts(result.Content)}
	case errors.Is(err, models.ErrHostToolUnsupported):
		outcome.State, outcome.Result = models.ToolEntryError, HostToolErrorResult(models.ToolResultFailed, HostToolUnsupportedMessage)
	default:
		var failure *models.HostToolFailure
		switch {
		case errors.As(err, &failure) && failure.NotStarted && failure.Code == models.HostToolFailureUnavailable:
			outcome.State, outcome.Result = models.ToolEntryError, HostToolErrorResult(models.ToolResultFailed, hostToolUnavailableMessage)
		case errors.As(err, &failure) && failure.NotStarted:
			message := boundHostToolMessage(failure.Message)
			if message == "" {
				message = hostToolCouldNotRunMessage
			}
			outcome.State, outcome.Result = models.ToolEntryError, HostToolErrorResult(models.ToolResultFailed, message)
		default:
			d.logger.Warnw("host tool failed", "runId", request.RunID, "stepId", request.StepID,
				"toolEntryId", request.ToolEntryID, "operation", request.Operation, "error", err)
			outcome.State, outcome.Result = hostToolFailureResult(write, hostToolCouldNotRunMessage)
		}
	}
	if command {
		outcome.PinnedRun = pinnedRunRecord(call.CommandPins, request.Arguments, usable)
	}
	return outcome, true
}

// execute calls the host, retrying a call the host did not start because it
// was unavailable. A request the kernel built wrong never reaches the host.
func (d *HostToolDispatcher) execute(ctx context.Context, request models.HostToolRequest) (models.HostToolResult, error) {
	if err := request.Validate(); err != nil {
		d.logger.Errorw("kernel built an invalid host tool request", "runId", request.RunID, "toolEntryId", request.ToolEntryID, "error", err)
		return models.HostToolResult{}, err
	}
	if d == nil || d.host == nil {
		return models.HostToolResult{}, models.ErrHostToolUnsupported
	}
	for attempt := 0; ; attempt++ {
		result, err := d.host.ExecuteHostTool(ctx, request)
		var failure *models.HostToolFailure
		if err == nil || ctx.Err() != nil || attempt >= len(hostToolUnavailableRetries) ||
			!errors.As(err, &failure) || !failure.NotStarted || failure.Code != models.HostToolFailureUnavailable {
			return result, err
		}
		if serr := d.sleep(ctx, hostToolUnavailableRetries[attempt]); serr != nil {
			return models.HostToolResult{}, serr
		}
	}
}

// request builds the host request of call (section 07 §6.2).
func (d *HostToolDispatcher) request(call HostToolCall) models.HostToolRequest {
	request := models.HostToolRequest{
		APIVersion:   models.HostToolAPIVersion,
		InvocationID: call.ToolEntryID,
		SessionID:    call.SessionID,
		ToolEntryID:  call.ToolEntryID,
		ToolCallID:   call.ToolCallID,
		Operation:    call.Spec.Operation,
		Arguments:    call.Arguments,
		Workspace:    call.Workspace,
	}
	if call.Run != nil {
		request.Scope = call.Run.Scope
		request.RunID = call.Run.ID
		request.AgentID = call.Run.AssignedAgentID
	}
	if call.Step != nil {
		request.StepID = call.Step.ID
	}
	if call.Binding != nil {
		request.DefinitionID = call.Binding.DefinitionID
		request.DefinitionVersion = call.Binding.DefinitionVersion
	}
	if call.Spec.Operation == workspace.OperationRunCommand {
		request.Arguments, _ = workspace.PinRunCommand(call.CommandPins, call.Arguments)
		request.CommandRef = models.HostToolCommandRef(call.ToolEntryID)
		if call.Binding != nil {
			request.VMTools = hostVMTools(call.Binding.ModelActions)
		}
	}
	if request.Arguments == nil {
		request.Arguments = map[string]interface{}{}
	}
	return request
}

// hostVMTools lists the binding's governed actions (the full catalog, not
// only the loaded set) as the VM lists them, sorted by name.
func hostVMTools(actions []capability.ModelAction) []models.HostVMTool {
	if len(actions) == 0 {
		return nil
	}
	tools := make([]models.HostVMTool, 0, len(actions))
	seen := make(map[string]bool, len(actions))
	for _, action := range actions {
		if strings.TrimSpace(action.Name) == "" || seen[action.Name] {
			continue
		}
		seen[action.Name] = true
		schema := action.InputSchema
		if schema == nil {
			schema = map[string]interface{}{"type": "object"}
		}
		tools = append(tools, models.HostVMTool{Name: action.Name, Description: action.Description, Risk: action.Risk, InputSchema: schema})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

// pinnedRunRecord records a pinned run_command from its executed arguments
// and the host's result object (nil when the host returned none usable). It
// is nil when the command ran no pinned file.
func pinnedRunRecord(pins []workspace.CommandPin, executed, result map[string]interface{}) *workspace.CommandRecord {
	record, pinned := workspace.RecordPinnedCommand(executed, result)
	if !pinned {
		return nil
	}
	if err := record.Validate(pins); err != nil && strings.TrimSpace(record.Refused) == "" {
		record.Refused = err.Error()
	}
	return &record
}

// hostToolResultObject is the JSON object of a host result's first json part:
// the shape run_command reports (exitStatus, stdout, expectedDigest, ...).
func hostToolResultObject(content []models.ContentPart) map[string]interface{} {
	for _, part := range content {
		if part.Type != models.ContentPartJSON || len(part.Value) == 0 {
			continue
		}
		var object map[string]interface{}
		if json.Unmarshal(part.Value, &object) == nil && object != nil {
			return object
		}
	}
	return nil
}

// HostToolResumeState is the D14 rule for a host tool entry found running on
// claim: reads run again (pending, no result), writes become interrupted.
func HostToolResumeState(spec HostToolSpec) (models.ToolEntryState, *models.ToolResult) {
	if spec.SideEffect == capability.SideEffectRead {
		return models.ToolEntryPending, nil
	}
	return models.ToolEntryInterrupted, HostToolErrorResult(models.ToolResultInterrupted, HostToolInterruptedMessage)
}

// hostToolFailureResult is the outcome of a call whose host outcome is
// unknown: a read is an error the model may retry, a write is interrupted.
func hostToolFailureResult(write bool, readMessage string) (models.ToolEntryState, *models.ToolResult) {
	if write {
		return models.ToolEntryInterrupted, HostToolErrorResult(models.ToolResultInterrupted, HostToolInterruptedMessage)
	}
	return models.ToolEntryError, HostToolErrorResult(models.ToolResultFailed, readMessage)
}

func HostToolErrorResult(status models.ToolResultStatus, message string) *models.ToolResult {
	value, _ := json.Marshal(map[string]string{"error": message})
	return &models.ToolResult{Status: status, IsError: true, Content: []models.ContentPart{{Type: models.ContentPartJSON, Value: value}}}
}

func boundHostToolMessage(message string) string {
	return strings.TrimSpace(models.TruncateUTF8(strings.TrimSpace(message), maxHostToolFailureMessage))
}
