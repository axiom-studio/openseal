package openseal

import (
	"context"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// engineReactionProjector mirrors reactions through the engine's current
// external conversation transport, which is rebuilt when Skills change.
type engineReactionProjector struct{ engine *Engine }

func (p engineReactionProjector) ProjectMessageReaction(ctx context.Context, event *runtime.ConversationReactionEvent) error {
	if p.engine == nil || p.engine.externalConversations.transport == nil {
		return nil
	}
	return p.engine.externalConversations.transport.ProjectMessageReaction(ctx, event)
}

// ReactionProjector mirrors reactions made in OpenSeal to the external
// conversations their messages came from or were delivered to.
func (e *Engine) ReactionProjector() runtime.MessageReactionProjector {
	return engineReactionProjector{engine: e}
}

// AddMessageReaction adds the actor's emoji to a message the actor can see.
// It is idempotent and never schedules a Run.
func (e *Engine) AddMessageReaction(ctx context.Context, request runtime.MessageReactionRequest) (*runtime.MessageReactionResult, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.AddMessageReaction(ctx, request)
}

// RemoveMessageReaction removes the actor's emoji from a message the actor
// can see. Removing an absent reaction is not an error.
func (e *Engine) RemoveMessageReaction(ctx context.Context, request runtime.MessageReactionRequest) (*runtime.MessageReactionResult, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.RemoveMessageReaction(ctx, request)
}
