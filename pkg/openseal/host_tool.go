package openseal

import (
	"errors"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

type (
	ContentPart      = runtime.ContentPart
	HostToolExecutor = runtime.HostToolExecutor
	HostToolRequest  = runtime.HostToolRequest
	HostToolResult   = runtime.HostToolResult
	HostToolFailure  = runtime.HostToolFailure
	HostVMTool       = runtime.HostVMTool
	HostToolSpec     = runtime.HostToolSpec
	ChatWorkspaces   = runtime.ChatWorkspaces
)

const (
	HostToolAPIVersion                  = runtime.HostToolAPIVersion
	MaxHostToolCommandRefBytes          = runtime.MaxHostToolCommandRefBytes
	MaxToolResultContentBytes           = runtime.MaxToolResultContentBytes
	HostToolFailureInvalidRequest       = runtime.HostToolFailureInvalidRequest
	HostToolFailureAccessDenied         = runtime.HostToolFailureAccessDenied
	HostToolFailureRequestTooLarge      = runtime.HostToolFailureRequestTooLarge
	HostToolFailureChatFilesUnavailable = runtime.HostToolFailureChatFilesUnavailable
	HostToolFailureSandboxesUnavailable = runtime.HostToolFailureSandboxesUnavailable
	HostToolFailureUnsupportedOperation = runtime.HostToolFailureUnsupportedOperation
	HostToolFailureUnavailable          = runtime.HostToolFailureUnavailable
	HostToolFailureFailed               = runtime.HostToolFailureFailed
)

var ErrHostToolUnsupported = runtime.ErrHostToolUnsupported

// HostToolCommandRef is the CommandRef of a run_command host tool call.
func HostToolCommandRef(toolEntryID string) string { return runtime.HostToolCommandRef(toolEntryID) }

// WithChatWorkspaces lets the run loop resolve each claim's chat Workspace and
// offer host tools in it. Without it no Run is offered host tools (the OSS and
// desktop daemons).
func WithChatWorkspaces(resolver runtime.ChatWorkspaces) Option {
	return func(e *Engine) error {
		if resolver == nil {
			return errors.New("chat workspaces resolver is required")
		}
		e.chatWorkspaces = resolver
		return nil
	}
}
