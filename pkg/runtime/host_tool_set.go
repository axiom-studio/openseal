package runtime

import (
	"context"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// ChatWorkspaces decides the chat whose files a Run works in. The kernel calls
// PrepareChatWorkspace once per claim, before the claim's first step.
type ChatWorkspaces interface {
	// PrepareChatWorkspace returns the conversation whose files run works in:
	// its own conversationId, or for a child Run (fork, task, delegation) its
	// root Run's chat. It returns "" when the Run works in no chat, or when this
	// host keeps no chat files. It may make the triggering message's
	// attachments available as uploads/ (idempotent). A non-nil error fails
	// nothing: the claim runs without host tools and logs it.
	PrepareChatWorkspace(ctx context.Context, run *AgentRun) (conversationID string, err error)
}

// prepareChatWorkspace resolves a claim's chat Workspace. A nil resolver, a
// Run with no chat, or a resolver error gives nil (no host tools); the error is
// returned for the caller to log and never fails the claim.
func prepareChatWorkspace(ctx context.Context, resolver ChatWorkspaces, run *AgentRun) (*workspace.Authority, error) {
	if resolver == nil || run == nil {
		return nil, nil
	}
	conversationID, err := resolver.PrepareChatWorkspace(ctx, run)
	if err != nil {
		return nil, err
	}
	authority := workspace.ChatAuthority(conversationID)
	if authority.Validate() != nil {
		return nil, nil
	}
	return authority, nil
}

// HostToolSpec is one host tool the kernel offers and dispatches to the step host.
type HostToolSpec struct {
	Operation   string                 // canonical op, e.g. workspace.OperationReadFile
	ToolName    string                 // provider-safe name: Operation with "." replaced by "_"
	Description string                 // workspace.Operation.Description verbatim
	InputSchema map[string]interface{} // workspace.Operation.InputSchema verbatim
	SideEffect  capability.SideEffect  // SideEffectRead or SideEffectWrite
	Concurrent  bool                   // true for the four reads
}

// hostToolReads are the host tools that only read; they run concurrently.
var hostToolReads = map[string]bool{
	workspace.OperationListDirectory: true,
	workspace.OperationReadFile:      true,
	workspace.OperationSearchFiles:   true,
	workspace.OperationListChats:     true,
}

// HostToolSpecs are the host tools of a chat Workspace in workspace.Operations
// order, without batch_read. nil or invalid authority gives nil.
func HostToolSpecs(authority *workspace.Authority) []HostToolSpec {
	operations := workspace.Operations(authority)
	if len(operations) == 0 {
		return nil
	}
	specs := make([]HostToolSpec, 0, len(operations))
	for _, operation := range operations {
		if operation.Name == workspace.OperationBatchRead {
			continue
		}
		spec := HostToolSpec{
			Operation:   operation.Name,
			ToolName:    hostToolName(operation.Name),
			Description: operation.Description,
			InputSchema: operation.InputSchema,
			SideEffect:  capability.SideEffectWrite,
		}
		if hostToolReads[operation.Name] {
			spec.SideEffect = capability.SideEffectRead
			spec.Concurrent = true
		}
		specs = append(specs, spec)
	}
	return specs
}

// LookupHostTool finds a spec by provider tool name or canonical op.
func LookupHostTool(specs []HostToolSpec, name string) (HostToolSpec, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return HostToolSpec{}, false
	}
	for _, spec := range specs {
		if spec.ToolName == name || spec.Operation == name {
			return spec, true
		}
	}
	return HostToolSpec{}, false
}

func hostToolName(operation string) string {
	return strings.ReplaceAll(operation, ".", "_")
}
