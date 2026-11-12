package conversation

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Router picks the one Team member who answers a channel message
// that addressed nobody (no @mention, no @everyone, no quoted or replied-to
// Agent). The host implements it with one small, fast model call and posts
// nothing. The router encodes, in order: the topic's owner, then whoever the
// user was just talking to, then whoever is free. The lead answers itself only
// for plan/priority questions or when nobody fits. Small talk never pulls a
// busy member.
//
// A router error, or a decision naming someone outside Members, is not
// retried: OpenSeal routes to LastAgentWithUser, else Lead, else the first
// member who is not busy.
type Router interface {
	Route(ctx context.Context, req RouteRequest) (RouteDecision, error)
}

// RouterFunc adapts a function to Router.
type RouterFunc func(context.Context, RouteRequest) (RouteDecision, error)

func (f RouterFunc) Route(ctx context.Context, req RouteRequest) (RouteDecision, error) {
	return f(ctx, req)
}

// RouteRequest is the lead's view of the Team for one message.
type RouteRequest struct {
	Scope          models.Scope
	ConversationID string
	TeamID         string
	// Lead is the Agent id of the Team's lead role; may be "".
	Lead string
	// RoutingNotes is the Team's free-text note on who owns what; may be "".
	RoutingNotes string
	Members      []RouteMember
	// Recent is the last ~10 messages the sender can see, oldest first,
	// ending before Trigger.
	Recent []models.ChannelMessage
	// LastAgentWithUser is the Agent that last answered this user; may be "".
	LastAgentWithUser string
	Trigger           models.ChannelMessage
}

// RouteMember is one Team member the router may pick.
type RouteMember struct {
	AgentID string
	Name    string
	Role    string
	Owns    []string
	// Busy: the member has an active Run in this Team's conversations.
	Busy bool
	// BusyWith is a short label of that Run's work; "" when not busy.
	BusyWith string
}

// RouteDecision names the responder. Reason is short and
// user-facing ("owns the press list"); it is shown with the member's answer.
type RouteDecision struct {
	AgentID string
	Reason  string
}
