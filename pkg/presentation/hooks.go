package presentation

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Binding ties a card to a living entity whose state the host overlays.
type Binding struct {
	Kind BindingKind `json:"kind"`
	ID   string      `json:"id"`
}

type BindingKind string

const (
	BindingRunbook          BindingKind = "runbook"
	BindingConversationTask BindingKind = "conversation_task"
	BindingRun              BindingKind = "run"
	BindingApproval         BindingKind = "approval"
	BindingAgent            BindingKind = "agent"
)

// BindingKinds lists every binding kind.
var BindingKinds = []BindingKind{BindingRunbook, BindingConversationTask, BindingRun, BindingApproval, BindingAgent}

// EntityCommands lists the commands an entity action may send per binding
// kind. Approval decisions use approval actions, not entity commands.
var EntityCommands = map[BindingKind][]string{
	BindingRunbook:          {"pause", "resume", "run_now", "stop"},
	BindingConversationTask: {"cancel"},
	BindingAgent:            {"open_chat"},
}

// BindingResolver is implemented by the host. ResolveBinding returns nil when
// the entity exists, is in the publishing conversation's workspace, and is
// visible from that conversation; otherwise an error the agent can act on,
// e.g. "conversation_task/X is not part of this conversation".
type BindingResolver interface {
	ResolveBinding(ctx context.Context, binding Binding) error
}

// BindingResolverFunc adapts a function to BindingResolver.
type BindingResolverFunc func(ctx context.Context, binding Binding) error

func (f BindingResolverFunc) ResolveBinding(ctx context.Context, binding Binding) error {
	return f(ctx, binding)
}

func (v *validator) binding(binding Binding) error {
	if !validBindingKind(binding.Kind) {
		return fmt.Errorf("kind %q is not one of runbook, conversation_task, run, approval, agent", binding.Kind)
	}
	if !opaqueID.MatchString(binding.ID) {
		return fmt.Errorf("id %q must match %s", binding.ID, opaqueIDPattern)
	}
	if v.opts.SkipHostChecks {
		return nil
	}
	if v.opts.Bindings == nil {
		return errors.New("bindings cannot be checked here; remove the binding")
	}
	if err := v.opts.Bindings.ResolveBinding(v.ctx, binding); err != nil {
		return fmt.Errorf("%s/%s: %w", binding.Kind, binding.ID, err)
	}
	return nil
}

func validBindingKind(kind BindingKind) bool {
	for _, candidate := range BindingKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// LinkProvenance is supplied by the host. It reports whether this exact URL
// appeared in a tool result or browser session of the publishing
// conversation. Only provenance-checked links are ever published.
type LinkProvenance func(ctx context.Context, rawURL string) (bool, error)

// MaxLinkBytes bounds a link URL.
const MaxLinkBytes = 2048

// ValidateLink checks a link field value: an absolute https URL without
// credentials that the host's LinkProvenance has seen in this conversation.
// Link fields exist only on item and place components.
func ValidateLink(ctx context.Context, rawURL string, opts ValidateOptions) error {
	if rawURL == "" || len(rawURL) > MaxLinkBytes || strings.TrimSpace(rawURL) != rawURL {
		return fmt.Errorf("link must be a URL of 1-%d bytes without surrounding spaces", MaxLinkBytes)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Opaque != "" {
		return fmt.Errorf("link %q must be an absolute https URL", rawURL)
	}
	if parsed.User != nil {
		return fmt.Errorf("link %q must not contain credentials", rawURL)
	}
	if opts.SkipHostChecks {
		return nil
	}
	if opts.LinkProvenance == nil {
		return fmt.Errorf("link %q cannot be verified by this host; leave it out", rawURL)
	}
	seen, err := opts.LinkProvenance(ctx, rawURL)
	if err != nil {
		return fmt.Errorf("check link %q: %w", rawURL, err)
	}
	if !seen {
		return fmt.Errorf("link %q did not appear in a tool result or browser page in this conversation; only use links you found", rawURL)
	}
	return nil
}
