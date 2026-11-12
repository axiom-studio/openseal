package openseal

import (
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

type (
	ContentPart      = models.ContentPart
	HostToolExecutor = models.HostToolExecutor
	HostToolRequest  = models.HostToolRequest
	HostToolResult   = models.HostToolResult
	HostToolFailure  = models.HostToolFailure
	HostVMTool       = models.HostVMTool
	HostToolSpec     = modelstep.HostToolSpec
	ChatWorkspaces   = modelstep.ChatWorkspaces
)

const (
	HostToolAPIVersion                  = models.HostToolAPIVersion
	MaxHostToolCommandRefBytes          = models.MaxHostToolCommandRefBytes
	MaxToolResultContentBytes           = models.MaxToolResultContentBytes
	HostToolFailureInvalidRequest       = models.HostToolFailureInvalidRequest
	HostToolFailureAccessDenied         = models.HostToolFailureAccessDenied
	HostToolFailureRequestTooLarge      = models.HostToolFailureRequestTooLarge
	HostToolFailureChatFilesUnavailable = models.HostToolFailureChatFilesUnavailable
	HostToolFailureSandboxesUnavailable = models.HostToolFailureSandboxesUnavailable
	HostToolFailureUnsupportedOperation = models.HostToolFailureUnsupportedOperation
	HostToolFailureUnavailable          = models.HostToolFailureUnavailable
	HostToolFailureFailed               = models.HostToolFailureFailed
)

var ErrHostToolUnsupported = models.ErrHostToolUnsupported

// HostToolCommandRef is the CommandRef of a run_command host tool call.
func HostToolCommandRef(toolEntryID string) string { return models.HostToolCommandRef(toolEntryID) }

// WithChatWorkspaces lets the run loop resolve each claim's chat Workspace and
// offer host tools in it. Without it no Run is offered host tools (the OSS and
// desktop daemons).
func WithChatWorkspaces(resolver modelstep.ChatWorkspaces) Option {
	return func(e *Engine) error {
		if resolver == nil {
			return errors.New("chat workspaces resolver is required")
		}
		e.chatWorkspaces = resolver
		return nil
	}
}
