package models

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// HostToolAPIVersion versions the host-tool wire contract (both directions).
const HostToolAPIVersion = "openseal.host-tool/v1"

// MaxHostToolCommandRefBytes bounds HostToolRequest.CommandRef.
const MaxHostToolCommandRefBytes = 128

// HostToolCommandRef is the CommandRef of a run_command call: it groups the
// VM's nested calls (action_calls.originRef) under that call's tool entry.
func HostToolCommandRef(toolEntryID string) string {
	return "command:" + toolEntryID
}

// HostToolExecutor is the host half of the step host. StepHost carries the
// same method next to ExecuteModelStep.
type HostToolExecutor interface {
	ExecuteHostTool(ctx context.Context, request HostToolRequest) (HostToolResult, error)
}

// HostToolRequest runs one host tool call of one step.
type HostToolRequest struct {
	APIVersion        string                 `json:"apiVersion"`
	InvocationID      string                 `json:"invocationId"` // == ToolEntryID
	Scope             Scope                  `json:"scope"`
	RunID             string                 `json:"runId"`
	StepID            string                 `json:"stepId"` // agent_turns.id of the running step
	SessionID         string                 `json:"sessionId"`
	AgentID           string                 `json:"agentId"` // Run.AssignedAgentID
	DefinitionID      string                 `json:"definitionId"`
	DefinitionVersion string                 `json:"definitionVersion"`
	ToolEntryID       string                 `json:"toolEntryId"`
	ToolCallID        string                 `json:"toolCallId"` // provider tool-call id
	Operation         string                 `json:"operation"`  // canonical op (HostToolSpec.Operation)
	Arguments         map[string]interface{} `json:"arguments"`  // model args; run_command may carry the kernel pin
	Workspace         workspace.Authority    `json:"workspace"`
	// CommandRef groups the VM's nested calls under this tool entry in activity
	// (action_calls.originRef). Set only for run_command: HostToolCommandRef(ToolEntryID).
	CommandRef string `json:"commandRef,omitempty"`
	// VMTools are the governed actions code in the VM may call (seal tools /
	// seal call). Set only for run_command. Empty means no nested bridge.
	VMTools []HostVMTool `json:"vmTools,omitempty"`
}

// HostVMTool is one governed action as the VM lists it.
type HostVMTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Risk        capability.RiskLevel   `json:"risk"`
	InputSchema map[string]interface{} `json:"inputSchema"` // {"type":"object"} when the action has none
}

// HostToolResult is the model-visible outcome of one host tool call.
type HostToolResult struct {
	APIVersion   string `json:"apiVersion"`
	InvocationID string `json:"invocationId"`
	// Content is text/json at most MaxToolResultContentBytes, plus file parts
	// for kept output. Parts never carry DataBase64.
	Content []ContentPart `json:"content"`
	IsError bool          `json:"isError,omitempty"` // the tool reported a failure the model can act on
}

// Host tool failure codes (HostToolFailure.Code).
const (
	HostToolFailureInvalidRequest       = "invalid_host_tool_request"
	HostToolFailureAccessDenied         = "host_tool_access_denied"
	HostToolFailureRequestTooLarge      = "host_tool_request_too_large"
	HostToolFailureChatFilesUnavailable = "chat_files_unavailable"
	HostToolFailureSandboxesUnavailable = "sandboxes_unavailable"
	HostToolFailureUnsupportedOperation = "unsupported_operation"
	HostToolFailureUnavailable          = "host_tool_unavailable"
	HostToolFailureFailed               = "host_tool_failed"
)

// HostToolFailure is an ExecuteHostTool error with a typed reason.
type HostToolFailure struct {
	Code       string
	Message    string // bounded and user-safe
	NotStarted bool   // the host provably did not begin the call (drain 503, 4xx validation)
}

func (f *HostToolFailure) Error() string {
	if f == nil {
		return "host tool failed"
	}
	message := strings.TrimSpace(f.Message)
	switch {
	case f.Code == "" && message == "":
		return "host tool failed"
	case f.Code == "":
		return message
	case message == "":
		return f.Code
	}
	return f.Code + ": " + message
}

// ErrHostToolUnsupported: the host runs no host tools. The daemon returns it.
var ErrHostToolUnsupported = errors.New("this host runs no host tools")

// Validate checks the request's identity and shape. It does not check that
// Operation is a host tool the host knows; the host answers that.
func (r HostToolRequest) Validate() error {
	invalid := func(format string, args ...interface{}) error {
		return &HostToolFailure{Code: HostToolFailureInvalidRequest, Message: fmt.Sprintf(format, args...), NotStarted: true}
	}
	if r.APIVersion != HostToolAPIVersion {
		return invalid("unsupported host tool apiVersion %q", r.APIVersion)
	}
	if err := r.Scope.Validate(); err != nil {
		return invalid("host tool scope is invalid")
	}
	for _, field := range []struct{ name, value string }{
		{"invocationId", r.InvocationID}, {"runId", r.RunID}, {"stepId", r.StepID},
		{"agentId", r.AgentID}, {"toolEntryId", r.ToolEntryID}, {"toolCallId", r.ToolCallID},
		{"operation", r.Operation},
	} {
		if strings.TrimSpace(field.value) == "" {
			return invalid("host tool request needs %s", field.name)
		}
	}
	if r.InvocationID != r.ToolEntryID {
		return invalid("host tool invocationId must equal toolEntryId")
	}
	if err := r.Workspace.Validate(); err != nil {
		return invalid("%s", err.Error())
	}
	if r.Operation != workspace.OperationRunCommand {
		if r.CommandRef != "" || len(r.VMTools) > 0 {
			return invalid("commandRef and vmTools are only for %s", workspace.OperationRunCommand)
		}
		return nil
	}
	if len(r.CommandRef) > MaxHostToolCommandRefBytes {
		return invalid("commandRef is %d bytes (max %d)", len(r.CommandRef), MaxHostToolCommandRefBytes)
	}
	seen := make(map[string]struct{}, len(r.VMTools))
	for _, tool := range r.VMTools {
		if strings.TrimSpace(tool.Name) == "" {
			return invalid("vm tool needs a name")
		}
		if _, ok := seen[tool.Name]; ok {
			return invalid("vm tool %q is listed twice", tool.Name)
		}
		seen[tool.Name] = struct{}{}
	}
	return nil
}

// Validate checks a host's answer to request: identity, and the content caps
// a tool entry stores. A too-large result wraps ErrSessionEntryTooLarge.
func (r HostToolResult) Validate(request HostToolRequest) error {
	if r.APIVersion != HostToolAPIVersion {
		return fmt.Errorf("host tool result has apiVersion %q", r.APIVersion)
	}
	if r.InvocationID != request.InvocationID {
		return fmt.Errorf("host tool result answers invocation %q, not %q", r.InvocationID, request.InvocationID)
	}
	if len(r.Content) == 0 {
		return fmt.Errorf("host tool result has no content")
	}
	if _, err := ValidateContentParts(r.Content, ContentPartLimits{
		Field:      "host tool result",
		MaxParts:   MaxToolResultParts,
		MaxContent: MaxToolResultContentBytes,
	}); err != nil {
		return fmt.Errorf("host tool result: %w", err)
	}
	return nil
}
