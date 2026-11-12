package models

import (
	"context"
	"strings"
	"unicode/utf8"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
)

// StartAgentBuilderRequest opens a builder conversation and posts the request
// as its first message. Name and model travel in Parameters, never in Content.
type StartAgentBuilderRequest struct {
	Scope Scope
	// Requester is the host-verified person asking (Type user).
	Requester ConversationParticipant
	// Prompt is the request as typed; it becomes the message Content.
	Prompt string
	// Parameters are the form fields; an empty Kind means "agent".
	Parameters AgentBuilderParameters
	// SpaceID places the conversation; "" uses the builder's first space,
	// else the requester's default space.
	SpaceID        string
	IdempotencyKey string
}

// AgentBuilderDraftReader is the read surface ListAgentBuilderDrafts needs.
type AgentBuilderDraftReader interface {
	ListConversations(context.Context, ConversationFilter) ([]*Conversation, error)
	ListChannelMessages(context.Context, ChannelMessageFilter) ([]*ChannelMessage, error)
	ListAgentRuns(context.Context, AgentRunFilter) ([]*AgentRun, error)
	ListApprovals(context.Context, ApprovalFilter) ([]*ApprovalCheckpoint, error)
}

// AgentBuilderDraftStarterID is the person whose first message makes a
// builder conversation their draft, else "".
func AgentBuilderDraftStarterID(conversation *Conversation, message *ChannelMessage) string {
	if conversation.Origin == nil || conversation.Origin.Kind != ConversationReferenceAgentBuilder ||
		message.Sequence != 1 || message.Sender.Type != ConversationParticipantUser {
		return ""
	}
	return message.Sender.ID
}

const (
	// agentBuilderCutoverRequestMarker precedes the typed request in a
	// change-set draft prompt.
	agentBuilderCutoverRequestMarker = "Request: "
)

// AgentBuilderCutoverDraft is one leaf change-set draft left by migration 76.
type AgentBuilderCutoverDraft struct {
	Scope          Scope
	ID             string
	IdempotencyKey string
	ActorType      string
	ActorID        string
	Mode           string
	// Prompt is the draft's own prompt; CutoverPrompts are its ancestors'
	// prompts, oldest first.
	Prompt         string
	CutoverPrompts []string
	// Agent is the drafted profile (result.candidate.agents[0]), if any.
	Agent     *kernelagent.AgentDefinition
	Placement AgentBuilderCutoverPlacement
	// Readable is false when the payload could not be decoded.
	Readable bool
}

// AgentBuilderCutoverPlacement is the part of a draft's placement the
// cutover reads.
type AgentBuilderCutoverPlacement struct {
	AgentDeploymentIDs map[string]string `json:"agentDeploymentIds,omitempty"`
}

// AgentBuilderCutoverStore holds the drafts migration 76 kept. The
// PostgreSQL and SQLite stores read agent_builder_cutover_drafts.
type AgentBuilderCutoverStore interface {
	// ListAgentBuilderCutoverDrafts lists every kept draft across scopes,
	// oldest first. It returns none when the table is absent.
	ListAgentBuilderCutoverDrafts(ctx context.Context) ([]AgentBuilderCutoverDraft, error)
	DeleteAgentBuilderCutoverDraft(ctx context.Context, scope Scope, id string) error
	// DropAgentBuilderCutoverTable drops the table when it exists and is
	// empty. It reports whether this call dropped it, so exactly one caller
	// sees true across processes.
	DropAgentBuilderCutoverTable(ctx context.Context) (bool, error)
}

// StartRequest is the builder conversation the draft becomes (D7, D12): the
// content is only what the person typed, oldest request first and each
// later revision on its own line; name, model and the drafted profile travel
// in Parameters, fitted to the parameter bounds. A field that cannot be
// fitted (sensitive text) is left out rather than failing the conversion.
func (d AgentBuilderCutoverDraft) StartRequest(model, spaceID string) StartAgentBuilderRequest {
	prompts := append(append([]string{}, d.CutoverPrompts...), d.Prompt)
	lines := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		line := agentBuilderCutoverTypedRequest(prompt)
		if line == "" || (len(lines) > 0 && lines[len(lines)-1] == line) {
			continue
		}
		lines = append(lines, line)
	}
	parameters := AgentBuilderParameters{Kind: AgentBuilderKindAgent}
	parameters.Model = fitAgentBuilderParameter("model", strings.TrimSpace(model), MaximumAgentBuilderModelBytes, true)
	if agent := d.Agent; agent != nil {
		parameters.Name = fitAgentBuilderParameter("name", agent.DisplayName, MaximumAgentBuilderNameBytes, true)
		draft := &AgentBuilderDraftProfile{
			Purpose:      fitAgentBuilderParameter("draft purpose", agent.Purpose, MaximumAgentBuilderPurposeBytes, true),
			SystemPrompt: fitAgentBuilderParameter("draft behaviour", agent.SystemPrompt, MaximumAgentBuilderSystemPromptBytes, false),
			Personality:  fitAgentBuilderParameter("draft personality", agent.Personality, maximumAgentBuilderPersonalityBytes, false),
		}
		seen := map[string]struct{}{}
		for _, principle := range agent.OperatingPrinciples {
			if len(draft.OperatingPrinciples) == MaximumAgentBuilderOperatingPrinciples {
				break
			}
			fitted := fitAgentBuilderParameter("draft operating principle", principle, maximumAgentBuilderOperatingPrincipleBytes, false)
			if _, duplicate := seen[fitted]; fitted == "" || duplicate {
				continue
			}
			seen[fitted] = struct{}{}
			draft.OperatingPrinciples = append(draft.OperatingPrinciples, fitted)
		}
		if !draft.IsEmpty() {
			parameters.Draft = draft
		}
	}
	return StartAgentBuilderRequest{
		Scope:          d.Scope,
		Requester:      ConversationParticipant{Type: ConversationParticipantUser, ID: strings.TrimSpace(d.ActorID)},
		Prompt:         strings.Join(lines, "\n"),
		Parameters:     parameters,
		SpaceID:        spaceID,
		IdempotencyKey: "cutover-" + d.ID,
	}
}

// agentBuilderCutoverTypedRequest is the text after the last "Request: " of
// a draft prompt, else the whole prompt.
func agentBuilderCutoverTypedRequest(prompt string) string {
	if index := strings.LastIndex(prompt, agentBuilderCutoverRequestMarker); index >= 0 {
		prompt = prompt[index+len(agentBuilderCutoverRequestMarker):]
	}
	return strings.TrimSpace(prompt)
}

// fitAgentBuilderParameter trims value into a builder parameter: one line
// when oneLine, at most limit bytes at a rune boundary. It returns "" when
// the result is still refused.
func fitAgentBuilderParameter(field, value string, limit int, oneLine bool) string {
	value = strings.TrimSpace(value)
	if oneLine {
		value = strings.Join(strings.Fields(value), " ")
	}
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "")
	}
	if len(value) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(value[cut]) {
			cut--
		}
		value = strings.TrimSpace(value[:cut])
	}
	if value == "" {
		return ""
	}
	if err := agentBuilderText(field, value, limit); err != nil {
		return ""
	}
	return value
}
