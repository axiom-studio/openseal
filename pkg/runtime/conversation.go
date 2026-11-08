package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/validation"
)

// AgentApprovalsConversationReferenceID returns a portable, stable origin ID
// for the approval inbox owned by an Agent deployment.
func AgentApprovalsConversationReferenceID(owner ObjectiveOwner) string {
	digest := sha256.Sum256([]byte(string(owner.Type) + "\x00" + strings.TrimSpace(owner.ID)))
	return "agent-approvals-" + hex.EncodeToString(digest[:16])
}

var (
	ErrConversationNotFound         = errors.New("conversation not found")
	ErrChannelMessageNotFound       = errors.New("channel message not found")
	ErrParticipationRoundNotFound   = errors.New("participation round not found")
	ErrInvalidConversation          = errors.New("invalid conversation")
	ErrMessageConflict              = errors.New("conversation message conflict")
	ErrConversationCursorConflict   = errors.New("conversation cursor conflict")
	ErrConversationPresenceConflict = errors.New("conversation presence lease conflict")
)

type ConversationStatus string

const (
	ConversationStatusActive   ConversationStatus = "active"
	ConversationStatusArchived ConversationStatus = "archived"
)

type ConversationParticipantType string

const (
	ConversationParticipantUser    ConversationParticipantType = "user"
	ConversationParticipantAgent   ConversationParticipantType = "agent"
	ConversationParticipantTeam    ConversationParticipantType = "team"
	ConversationParticipantService ConversationParticipantType = "service"
)

type ConversationParticipant struct {
	Type ConversationParticipantType `json:"type"`
	ID   string                      `json:"id"`
}

func (p ConversationParticipant) Validate() error {
	switch p.Type {
	case ConversationParticipantUser, ConversationParticipantAgent, ConversationParticipantTeam, ConversationParticipantService:
	default:
		return fmt.Errorf("%w: invalid participant type %q", ErrInvalidConversation, p.Type)
	}
	if !validOpaqueIdentifier(p.ID, 128) {
		return fmt.Errorf("%w: participant id must be a portable opaque identifier", ErrInvalidConversation)
	}
	return nil
}

type Conversation struct {
	ID    string         `json:"id"`
	Scope Scope          `json:"scope"`
	Owner ObjectiveOwner `json:"owner"`
	// SpaceID is the space the conversation lives in. Stores resolve an empty
	// value on create; every stored conversation has one.
	SpaceID       string                     `json:"spaceId"`
	Title         string                     `json:"title"`
	Origin        *ConversationReference     `json:"origin,omitempty"`
	Status        ConversationStatus         `json:"status"`
	LastSequence  int64                      `json:"lastSequence"`
	Revision      int64                      `json:"revision"`
	CreatedAt     time.Time                  `json:"createdAt"`
	UpdatedAt     time.Time                  `json:"updatedAt"`
	ArchivedAt    *time.Time                 `json:"archivedAt,omitempty"`
	Participation *ConversationParticipation `json:"participation,omitempty"`
	// ApprovalMode is the review boundary the kernel applies to actions
	// proposed for this conversation. Stores persist it explicitly; it
	// defaults to auto for new and pre-existing conversations.
	ApprovalMode ConversationApprovalMode `json:"approvalMode"`
	// RunbookActivationIDs are the routines (runbook activations) that
	// routine visuals in this conversation show, oldest first, at most
	// MaximumConversationRunbooks. The change stream carries their live
	// state. See ConversationService.LinkRunbooks.
	RunbookActivationIDs []string `json:"runbookActivationIds,omitempty"`
}

// ConversationParticipation records an explicit host-authorized opt-in. The
// sequence boundary is kernel-owned: enabling never replays earlier messages.
type ConversationParticipation struct {
	Enabled       bool  `json:"enabled"`
	AfterSequence int64 `json:"afterSequence"`
}

func (c *Conversation) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: conversation is required", ErrInvalidConversation)
	}
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if err := c.Owner.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(c.ID, 128) || strings.TrimSpace(c.Title) == "" || len(c.Title) > 240 {
		return fmt.Errorf("%w: portable id and title of at most 240 characters are required", ErrInvalidConversation)
	}
	if c.SpaceID != "" && !validOpaqueIdentifier(c.SpaceID, 128) {
		return fmt.Errorf("%w: space id must be a portable opaque identifier", ErrInvalidConversation)
	}
	if c.Origin != nil {
		if err := c.Origin.Validate(); err != nil {
			return err
		}
	}
	if len(c.RunbookActivationIDs) > MaximumConversationRunbooks {
		return fmt.Errorf("%w: at most %d linked routines", ErrInvalidConversation, MaximumConversationRunbooks)
	}
	linked := make(map[string]bool, len(c.RunbookActivationIDs))
	for _, id := range c.RunbookActivationIDs {
		if !validOpaqueIdentifier(id, 128) || linked[id] {
			return fmt.Errorf("%w: linked routine ids must be distinct portable identifiers", ErrInvalidConversation)
		}
		linked[id] = true
	}
	if c.Revision <= 0 || c.LastSequence < 0 || c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: positive revision, sequence, and timestamps are required", ErrInvalidConversation)
	}
	if c.ApprovalMode != "" && !c.ApprovalMode.Valid() {
		return fmt.Errorf("%w: invalid approval mode %q", ErrInvalidConversation, c.ApprovalMode)
	}
	if c.Participation != nil && (c.Participation.AfterSequence < 0 || c.Participation.AfterSequence > c.LastSequence) {
		return fmt.Errorf("%w: participation boundary must be within saved message history", ErrInvalidConversation)
	}
	switch c.Status {
	case ConversationStatusActive:
		if c.ArchivedAt != nil {
			return fmt.Errorf("%w: active conversation cannot be archived", ErrInvalidConversation)
		}
	case ConversationStatusArchived:
		if c.ArchivedAt == nil {
			return fmt.Errorf("%w: archived conversation requires archive time", ErrInvalidConversation)
		}
	default:
		return fmt.Errorf("%w: invalid status %q", ErrInvalidConversation, c.Status)
	}
	return nil
}

type ConversationMessageIntent string

const (
	MessageIntentQuestion        ConversationMessageIntent = "question"
	MessageIntentAnswer          ConversationMessageIntent = "answer"
	MessageIntentUpdate          ConversationMessageIntent = "update"
	MessageIntentProposal        ConversationMessageIntent = "proposal"
	MessageIntentDecision        ConversationMessageIntent = "decision"
	MessageIntentObjection       ConversationMessageIntent = "objection"
	MessageIntentHandoff         ConversationMessageIntent = "handoff"
	MessageIntentEscalation      ConversationMessageIntent = "escalation"
	MessageIntentApprovalRequest ConversationMessageIntent = "approval_request"
	MessageIntentAcknowledgment  ConversationMessageIntent = "acknowledgment"
	MessageIntentSystem          ConversationMessageIntent = "system"
)

type ConversationAudienceKind string

const (
	ConversationAudienceChannel      ConversationAudienceKind = "channel"
	ConversationAudienceParticipants ConversationAudienceKind = "participants"
	ConversationAudienceRoles        ConversationAudienceKind = "roles"
	// ConversationAudienceEveryone is visible like the channel and asks every
	// non-observer Team member to answer (@everyone).
	ConversationAudienceEveryone ConversationAudienceKind = "everyone"
)

type ConversationAudience struct {
	Kind         ConversationAudienceKind  `json:"kind"`
	Participants []ConversationParticipant `json:"participants,omitempty"`
	Roles        []string                  `json:"roles,omitempty"`
}

type ConversationViewer struct {
	Participant ConversationParticipant `json:"participant"`
	Roles       []string                `json:"roles,omitempty"`
}

func (v ConversationViewer) Validate() error {
	if err := v.Participant.Validate(); err != nil {
		return err
	}
	for _, role := range v.Roles {
		if strings.TrimSpace(role) == "" || len(role) > 80 {
			return fmt.Errorf("%w: viewer role must be 1-80 characters", ErrInvalidConversation)
		}
	}
	return nil
}

// CanViewChannelMessage fails closed for targeted audiences. The caller must
// first authorize the viewer as a channel participant. Mentions never widen
// the durable audience.
func CanViewChannelMessage(message *ChannelMessage, viewer ConversationViewer) bool {
	if message == nil || viewer.Validate() != nil {
		return false
	}
	if message.Sender == viewer.Participant {
		return true
	}
	switch message.Audience.Kind {
	case ConversationAudienceChannel, ConversationAudienceEveryone:
		return true
	case ConversationAudienceParticipants:
		for _, participant := range message.Audience.Participants {
			if participant == viewer.Participant {
				return true
			}
		}
	case ConversationAudienceRoles:
		for _, audienceRole := range message.Audience.Roles {
			for _, viewerRole := range viewer.Roles {
				if strings.EqualFold(strings.TrimSpace(audienceRole), strings.TrimSpace(viewerRole)) {
					return true
				}
			}
		}
	}
	return false
}

func (a ConversationAudience) Validate() error {
	switch a.Kind {
	case ConversationAudienceChannel, ConversationAudienceEveryone:
		if len(a.Participants) > 0 || len(a.Roles) > 0 {
			return fmt.Errorf("%w: channel audience cannot contain targets", ErrInvalidConversation)
		}
	case ConversationAudienceParticipants:
		if len(a.Participants) == 0 || len(a.Roles) > 0 {
			return fmt.Errorf("%w: participant audience requires participants only", ErrInvalidConversation)
		}
		seen := make(map[ConversationParticipant]struct{}, len(a.Participants))
		for _, participant := range a.Participants {
			if err := participant.Validate(); err != nil {
				return err
			}
			if _, exists := seen[participant]; exists {
				return fmt.Errorf("%w: duplicate audience participant", ErrInvalidConversation)
			}
			seen[participant] = struct{}{}
		}
	case ConversationAudienceRoles:
		if len(a.Roles) == 0 || len(a.Participants) > 0 {
			return fmt.Errorf("%w: role audience requires roles only", ErrInvalidConversation)
		}
		for _, role := range a.Roles {
			if strings.TrimSpace(role) == "" || len(role) > 80 {
				return fmt.Errorf("%w: audience role must be 1-80 characters", ErrInvalidConversation)
			}
		}
	default:
		return fmt.Errorf("%w: invalid audience kind %q", ErrInvalidConversation, a.Kind)
	}
	return nil
}

type ConversationReferenceKind string

const (
	ConversationReferenceObjective      ConversationReferenceKind = "objective"
	ConversationReferenceRun            ConversationReferenceKind = "run"
	ConversationReferenceTask           ConversationReferenceKind = "conversation_task"
	ConversationReferenceRequest        ConversationReferenceKind = "agent_request"
	ConversationReferenceApproval       ConversationReferenceKind = "approval"
	ConversationReferenceArtifact       ConversationReferenceKind = "artifact"
	ConversationReferenceActivity       ConversationReferenceKind = "activity"
	ConversationReferenceExternalSource ConversationReferenceKind = "external_source"
	// ConversationReferenceAgentControl identifies the one kernel-owned control
	// channel attached to an Agent deployment. It is a durable command and audit
	// surface, not an Objective or an implementation-specific settings page.
	ConversationReferenceAgentControl ConversationReferenceKind = "agent_control"
	// ConversationReferenceAgentApprovals identifies the kernel-owned approval
	// inbox attached to an Agent. External approval destinations are optional;
	// this conversation is the durable, always-present review surface.
	ConversationReferenceAgentApprovals ConversationReferenceKind = "agent_approvals"
	ConversationReferenceEmbedSession   ConversationReferenceKind = "embed_session"
	// ConversationReferenceAgentBuilder marks a builder conversation: a person
	// asking the builder Agent to create or change an Agent or Team. Its id is
	// the client's idempotency key.
	ConversationReferenceAgentBuilder ConversationReferenceKind = "agent_builder"
)

type ConversationReference struct {
	Kind    ConversationReferenceKind `json:"kind"`
	ID      string                    `json:"id"`
	Version int64                     `json:"version,omitempty"`
}

func (r ConversationReference) Validate() error {
	switch r.Kind {
	case ConversationReferenceObjective, ConversationReferenceRun, ConversationReferenceTask, ConversationReferenceRequest,
		ConversationReferenceApproval, ConversationReferenceArtifact, ConversationReferenceActivity,
		ConversationReferenceExternalSource, ConversationReferenceAgentControl, ConversationReferenceAgentApprovals,
		ConversationReferenceEmbedSession, ConversationReferenceAgentBuilder:
	default:
		return fmt.Errorf("%w: invalid reference kind %q", ErrInvalidConversation, r.Kind)
	}
	if !validOpaqueIdentifier(r.ID, 256) || r.Version < 0 {
		return fmt.Errorf("%w: reference id must be portable and version cannot be negative", ErrInvalidConversation)
	}
	return nil
}

// ChannelMessage is a durable, user-visible collaboration fact. It never
// stores hidden reasoning; explanations belong in concise content and linked
// evidence, decisions, Runs, requests, approvals, and artifacts.
type ChannelMessage struct {
	ID                string                  `json:"id"`
	Scope             Scope                   `json:"scope"`
	ConversationID    string                  `json:"conversationId"`
	Sequence          int64                   `json:"sequence"`
	Sender            ConversationParticipant `json:"sender"`
	SenderDisplayName string                  `json:"senderDisplayName,omitempty"`
	// Initiator records the authenticated user who requested a host-generated
	// service event. Hosts stamp it from verified identity, never client input.
	// It preserves provenance without changing the service sender's authority.
	Initiator      *ConversationParticipant  `json:"initiator,omitempty"`
	ExternalSource *ExternalMessageSource    `json:"externalSource,omitempty"`
	Intent         ConversationMessageIntent `json:"intent"`
	Content        string                    `json:"content"`
	// ResponseMode describes presentation only; it never changes sender authority.
	ResponseMode string `json:"responseMode,omitempty"`
	// ContributionKey is a privacy-safe, normalized semantic claim identifier
	// used to suppress paraphrased pile-ons (for example rollout-owner:agent-37).
	// It is user-visible metadata, never hidden reasoning.
	ContributionKey string               `json:"contributionKey,omitempty"`
	Audience        ConversationAudience `json:"audience"`
	// StartsThread marks a host-created thread root without turning that root
	// into a reply. It affects conversation routing, never sender authority.
	StartsThread     bool   `json:"startsThread,omitempty"`
	ThreadRootID     string `json:"threadRootId,omitempty"`
	ReplyToMessageID string `json:"replyToMessageId,omitempty"`
	// Quote shows the earlier message this one answers, like a chat-app reply.
	// It never changes thread placement; ReplyToMessageID does that.
	Quote *ChannelMessageQuote `json:"quote,omitempty"`
	// Question lists the choices of an Agent's question (only with intent
	// question). SelectedOptionIDs are the choices a reply to it picked.
	Question          *ChannelQuestion `json:"question,omitempty"`
	SelectedOptionIDs []string         `json:"selectedOptionIds,omitempty"`
	// BuilderParameters are the form fields (kind, name, model, migrated
	// draft) a person's message in an agent builder conversation carries.
	// They are never written into Content.
	BuilderParameters    *AgentBuilderParameters   `json:"builderParameters,omitempty"`
	BroadcastToChannel   bool                      `json:"broadcastToChannel,omitempty"`
	Mentions             []ConversationParticipant `json:"mentions,omitempty"`
	References           []ConversationReference   `json:"references,omitempty"`
	RequiresResponse     bool                      `json:"requiresResponse,omitempty"`
	ResolvesMessageID    string                    `json:"resolvesMessageId,omitempty"`
	SupersedesMessageID  string                    `json:"supersedesMessageId,omitempty"`
	IdempotencyKey       string                    `json:"idempotencyKey,omitempty"`
	ParticipationRoundID string                    `json:"participationRoundId,omitempty"`
	// RoutingReason says why the router picked this member to answer ("owns
	// the press list"). Set only on a routed member's answer.
	RoutingReason string    `json:"routingReason,omitempty"`
	Historical    bool      `json:"historical,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	// Reactions are filled on read from the reaction store. They are never
	// part of the stored message, its validation or its idempotency.
	Reactions []MessageReaction `json:"reactions,omitempty"`
}

// ChannelMessageQuote is a snapshot of a quoted message taken when the reply
// is posted. Readers who cannot see the quoted message get only MessageID and
// Unavailable, never its sender or excerpt.
type ChannelMessageQuote struct {
	MessageID         string                   `json:"messageId"`
	Sender            *ConversationParticipant `json:"sender,omitempty"`
	SenderDisplayName string                   `json:"senderDisplayName,omitempty"`
	Excerpt           string                   `json:"excerpt,omitempty"`
	Unavailable       bool                     `json:"unavailable,omitempty"`
}

const channelMessageQuoteExcerptRunes = 200

// ChannelQuestion is the set of choices a question message offers. Option ids
// are kernel-assigned ("o1".."o6"); an answer names the ids it picked.
type ChannelQuestion struct {
	Options       []ChannelQuestionOption `json:"options"`
	AllowMultiple bool                    `json:"allowMultiple,omitempty"`
	// AllowOther accepts a free-text answer alongside the options.
	AllowOther bool `json:"allowOther,omitempty"`
}

type ChannelQuestionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

func validChannelQuestionOptionText(label, description string) error {
	if label == "" || len(label) > maximumAskUserOptionLabelBytes || !utf8.ValidString(label) || strings.ContainsAny(label, "\r\n") {
		return fmt.Errorf("option label must be one line of 1 to %d bytes", maximumAskUserOptionLabelBytes)
	}
	if len(description) > maximumAskUserOptionDescriptionBytes || !utf8.ValidString(description) {
		return fmt.Errorf("option description must be valid text of at most %d bytes", maximumAskUserOptionDescriptionBytes)
	}
	return nil
}

func (q *ChannelQuestion) validate() error {
	if q == nil {
		return nil
	}
	if len(q.Options) < minimumAskUserOptions || len(q.Options) > maximumAskUserOptions {
		return fmt.Errorf("%w: a question offers %d to %d options", ErrInvalidConversation, minimumAskUserOptions, maximumAskUserOptions)
	}
	ids := make(map[string]struct{}, len(q.Options))
	labels := make(map[string]struct{}, len(q.Options))
	for _, option := range q.Options {
		if !validOpaqueIdentifier(option.ID, 16) {
			return fmt.Errorf("%w: question option ids must be portable", ErrInvalidConversation)
		}
		if err := validChannelQuestionOptionText(option.Label, option.Description); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidConversation, err)
		}
		if _, exists := ids[option.ID]; exists {
			return fmt.Errorf("%w: duplicate question option id", ErrInvalidConversation)
		}
		if _, exists := labels[option.Label]; exists {
			return fmt.Errorf("%w: duplicate question option label", ErrInvalidConversation)
		}
		ids[option.ID], labels[option.Label] = struct{}{}, struct{}{}
	}
	return nil
}

func (q *ChannelQuestion) hasOption(id string) bool {
	for _, option := range q.Options {
		if option.ID == id {
			return true
		}
	}
	return false
}

func cloneChannelQuestion(in *ChannelQuestion) *ChannelQuestion {
	if in == nil {
		return nil
	}
	out := *in
	out.Options = append([]ChannelQuestionOption(nil), in.Options...)
	return &out
}

// Builder parameter kinds and limits. Draft field limits match the
// create_agent schema.
const (
	AgentBuilderKindAgent = "agent"
	AgentBuilderKindTeam  = "team"

	maximumAgentBuilderNameBytes               = 80
	maximumAgentBuilderModelBytes              = 200
	maximumAgentBuilderPurposeBytes            = 2000
	maximumAgentBuilderSystemPromptBytes       = 16384
	maximumAgentBuilderPersonalityBytes        = 16384
	maximumAgentBuilderOperatingPrinciples     = 16
	maximumAgentBuilderOperatingPrincipleBytes = 2048
)

// AgentBuilderParameters are the structured fields of a person's message in a
// builder conversation (D7): what the form set, never message text.
type AgentBuilderParameters struct {
	Kind  string `json:"kind"`            // "agent" | "team"
	Name  string `json:"name,omitempty"`  // trimmed, at most 80 bytes
	Model string `json:"model,omitempty"` // "" is the default; the host validates it when the builder proposes
	// Draft is the drafted profile of a migrated change-set draft (cutover only).
	Draft *AgentBuilderDraftProfile `json:"draft,omitempty"`
}

type AgentBuilderDraftProfile struct {
	Purpose             string   `json:"purpose,omitempty"`
	SystemPrompt        string   `json:"systemPrompt,omitempty"`
	Personality         string   `json:"personality,omitempty"`
	OperatingPrinciples []string `json:"operatingPrinciples,omitempty"`
}

// empty reports parameters that set nothing beyond their kind.
func (p *AgentBuilderParameters) empty() bool {
	return p.Name == "" && p.Model == "" && (p.Draft == nil || p.Draft.empty())
}

func (d *AgentBuilderDraftProfile) empty() bool {
	return d.Purpose == "" && d.SystemPrompt == "" && d.Personality == "" && len(d.OperatingPrinciples) == 0
}

func (p *AgentBuilderParameters) validate() error {
	if p == nil {
		return nil
	}
	if p.Kind != AgentBuilderKindAgent && p.Kind != AgentBuilderKindTeam {
		return fmt.Errorf("%w: builder parameters kind must be %q or %q", ErrInvalidConversation, AgentBuilderKindAgent, AgentBuilderKindTeam)
	}
	if err := agentBuilderLine("name", p.Name, maximumAgentBuilderNameBytes); err != nil {
		return err
	}
	if err := agentBuilderLine("model", p.Model, maximumAgentBuilderModelBytes); err != nil {
		return err
	}
	if d := p.Draft; d != nil {
		if err := agentBuilderLine("draft purpose", d.Purpose, maximumAgentBuilderPurposeBytes); err != nil {
			return err
		}
		if err := agentBuilderText("draft behaviour", d.SystemPrompt, maximumAgentBuilderSystemPromptBytes); err != nil {
			return err
		}
		if err := agentBuilderText("draft personality", d.Personality, maximumAgentBuilderPersonalityBytes); err != nil {
			return err
		}
		if len(d.OperatingPrinciples) > maximumAgentBuilderOperatingPrinciples {
			return fmt.Errorf("%w: a draft has at most %d operating principles", ErrInvalidConversation, maximumAgentBuilderOperatingPrinciples)
		}
		seen := make(map[string]struct{}, len(d.OperatingPrinciples))
		for _, principle := range d.OperatingPrinciples {
			if strings.TrimSpace(principle) == "" {
				return fmt.Errorf("%w: draft operating principles cannot be empty", ErrInvalidConversation)
			}
			if _, exists := seen[principle]; exists {
				return fmt.Errorf("%w: duplicate draft operating principle", ErrInvalidConversation)
			}
			seen[principle] = struct{}{}
			if err := agentBuilderText("draft operating principle", principle, maximumAgentBuilderOperatingPrincipleBytes); err != nil {
				return err
			}
		}
	}
	return nil
}

func agentBuilderLine(field, value string, limit int) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%w: builder parameter %s must be one line", ErrInvalidConversation, field)
	}
	return agentBuilderText(field, value, limit)
}

func agentBuilderText(field, value string, limit int) error {
	if value != strings.TrimSpace(value) || len(value) > limit || !utf8.ValidString(value) {
		return fmt.Errorf("%w: builder parameter %s must be trimmed text of at most %d bytes", ErrInvalidConversation, field, limit)
	}
	if err := validation.ValidateNoSensitiveInput(value); err != nil {
		return fmt.Errorf("%w: builder parameter %s: %w", ErrInvalidConversation, field, err)
	}
	return nil
}

// normalizeAgentBuilderParameters trims a request's parameters into the form
// the message stores. It returns a copy; nil stays nil.
func normalizeAgentBuilderParameters(in *AgentBuilderParameters) *AgentBuilderParameters {
	if in == nil {
		return nil
	}
	out := &AgentBuilderParameters{Kind: strings.TrimSpace(in.Kind), Name: strings.TrimSpace(in.Name), Model: strings.TrimSpace(in.Model)}
	if d := in.Draft; d != nil {
		draft := &AgentBuilderDraftProfile{Purpose: strings.TrimSpace(d.Purpose), SystemPrompt: strings.TrimSpace(d.SystemPrompt), Personality: strings.TrimSpace(d.Personality)}
		for _, principle := range d.OperatingPrinciples {
			draft.OperatingPrinciples = append(draft.OperatingPrinciples, strings.TrimSpace(principle))
		}
		out.Draft = draft
	}
	return out
}

func cloneAgentBuilderParameters(in *AgentBuilderParameters) *AgentBuilderParameters {
	if in == nil {
		return nil
	}
	out := *in
	if in.Draft != nil {
		draft := *in.Draft
		draft.OperatingPrinciples = append([]string(nil), in.Draft.OperatingPrinciples...)
		if len(draft.OperatingPrinciples) == 0 {
			draft.OperatingPrinciples = nil
		}
		out.Draft = &draft
	}
	return &out
}

func newChannelMessageQuote(message *ChannelMessage) *ChannelMessageQuote {
	sender := message.Sender
	excerpt := strings.Join(strings.Fields(message.Content), " ")
	if runes := []rune(excerpt); len(runes) > channelMessageQuoteExcerptRunes {
		excerpt = strings.TrimSpace(string(runes[:channelMessageQuoteExcerptRunes])) + "\u2026"
	}
	return &ChannelMessageQuote{MessageID: message.ID, Sender: &sender, SenderDisplayName: message.SenderDisplayName, Excerpt: excerpt}
}

func (q *ChannelMessageQuote) unavailable() *ChannelMessageQuote {
	return &ChannelMessageQuote{MessageID: q.MessageID, Unavailable: true}
}

func (q *ChannelMessageQuote) validate(messageID string) error {
	if q == nil {
		return nil
	}
	if !validOpaqueIdentifier(q.MessageID, 128) || q.MessageID == messageID {
		return fmt.Errorf("%w: a quote must name another portable message id", ErrInvalidConversation)
	}
	if q.Unavailable {
		if q.Sender != nil || q.SenderDisplayName != "" || q.Excerpt != "" {
			return fmt.Errorf("%w: an unavailable quote cannot carry its sender or excerpt", ErrInvalidConversation)
		}
		return nil
	}
	if q.Sender == nil {
		return fmt.Errorf("%w: a quote requires its sender", ErrInvalidConversation)
	}
	if err := q.Sender.Validate(); err != nil {
		return err
	}
	if len(q.SenderDisplayName) > 160 || len(q.Excerpt) > 1024 {
		return fmt.Errorf("%w: quote sender name or excerpt is too long", ErrInvalidConversation)
	}
	return nil
}

func cloneChannelMessageQuote(in *ChannelMessageQuote) *ChannelMessageQuote {
	if in == nil {
		return nil
	}
	out := *in
	out.Sender = cloneConversationParticipant(in.Sender)
	return &out
}

func (m *ChannelMessage) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: message is required", ErrInvalidConversation)
	}
	if m.ResponseMode != "" && m.ResponseMode != "spoken" {
		return fmt.Errorf("%w: unsupported response mode", ErrInvalidConversation)
	}
	if err := m.Scope.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(m.ID, 128) || !validOpaqueIdentifier(m.ConversationID, 128) || m.Sequence <= 0 || m.CreatedAt.IsZero() {
		return fmt.Errorf("%w: message, conversation, positive sequence, and timestamp are required", ErrInvalidConversation)
	}
	if err := m.Sender.Validate(); err != nil {
		return err
	}
	if m.Initiator != nil {
		if m.Sender.Type != ConversationParticipantService || m.Initiator.Type != ConversationParticipantUser {
			return fmt.Errorf("%w: only service events can record a user initiator", ErrInvalidConversation)
		}
		if err := m.Initiator.Validate(); err != nil {
			return err
		}
	}
	if len(m.SenderDisplayName) > 160 || strings.ContainsAny(m.SenderDisplayName, "\r\n") {
		return fmt.Errorf("%w: sender display name cannot exceed 160 characters or contain line breaks", ErrInvalidConversation)
	}
	if !validConversationMessageIntent(m.Intent) || len(m.Content) > 65536 {
		return fmt.Errorf("%w: valid intent and content of at most 64 KiB are required", ErrInvalidConversation)
	}
	if m.BuilderParameters != nil {
		if m.Sender.Type != ConversationParticipantUser {
			return fmt.Errorf("%w: only a person's message can carry builder parameters", ErrInvalidConversation)
		}
		if err := m.BuilderParameters.validate(); err != nil {
			return err
		}
	}
	if strings.TrimSpace(m.Content) == "" && (m.BuilderParameters == nil || m.BuilderParameters.empty()) {
		return fmt.Errorf("%w: content is required unless the message sets builder parameters", ErrInvalidConversation)
	}
	if !validConversationContributionKey(m.ContributionKey) {
		return fmt.Errorf("%w: contribution key must be normalized lowercase metadata", ErrInvalidConversation)
	}
	if err := m.Audience.Validate(); err != nil {
		return err
	}
	if len(m.RoutingReason) > MaximumRoutingReasonBytes || strings.ContainsAny(m.RoutingReason, "\r\n") ||
		m.RoutingReason != "" && m.Sender.Type != ConversationParticipantAgent {
		return fmt.Errorf("%w: a routing reason is one short line on an Agent's answer", ErrInvalidConversation)
	}
	for _, id := range []string{m.ThreadRootID, m.ReplyToMessageID, m.ResolvesMessageID, m.SupersedesMessageID, m.ParticipationRoundID} {
		if id != "" && !validOpaqueIdentifier(id, 128) {
			return fmt.Errorf("%w: linked message and round ids must be portable", ErrInvalidConversation)
		}
	}
	if err := m.Quote.validate(m.ID); err != nil {
		return err
	}
	if m.Question != nil && m.Intent != MessageIntentQuestion {
		return fmt.Errorf("%w: only a question message can offer options", ErrInvalidConversation)
	}
	if err := m.Question.validate(); err != nil {
		return err
	}
	if len(m.SelectedOptionIDs) > 0 {
		if m.ReplyToMessageID == "" && m.ResolvesMessageID == "" {
			return fmt.Errorf("%w: selected options must answer a question", ErrInvalidConversation)
		}
		if len(m.SelectedOptionIDs) > maximumAskUserOptions {
			return fmt.Errorf("%w: too many selected options", ErrInvalidConversation)
		}
		seen := make(map[string]struct{}, len(m.SelectedOptionIDs))
		for _, id := range m.SelectedOptionIDs {
			if _, exists := seen[id]; exists || !validOpaqueIdentifier(id, 16) {
				return fmt.Errorf("%w: selected option ids must be portable and unique", ErrInvalidConversation)
			}
			seen[id] = struct{}{}
		}
	}
	if m.BroadcastToChannel && m.ThreadRootID == "" {
		return fmt.Errorf("%w: only a thread reply can be broadcast to its channel", ErrInvalidConversation)
	}
	if m.StartsThread && m.ThreadRootID != "" {
		return fmt.Errorf("%w: a new thread root cannot belong to another thread", ErrInvalidConversation)
	}
	seenMentions := make(map[ConversationParticipant]struct{}, len(m.Mentions))
	for _, mention := range m.Mentions {
		if err := mention.Validate(); err != nil {
			return err
		}
		if _, exists := seenMentions[mention]; exists {
			return fmt.Errorf("%w: duplicate mention", ErrInvalidConversation)
		}
		seenMentions[mention] = struct{}{}
	}
	for _, reference := range m.References {
		if err := reference.Validate(); err != nil {
			return err
		}
	}
	if strings.ContainsAny(m.IdempotencyKey, "\r\n") || len(m.IdempotencyKey) > 256 {
		return fmt.Errorf("%w: idempotency key cannot exceed 256 characters or contain line breaks", ErrInvalidConversation)
	}
	return nil
}

func validConversationContributionKey(value string) bool {
	if value == "" {
		return true
	}
	if value != strings.TrimSpace(value) || len(value) > 160 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || strings.ContainsRune("._:-", character) {
			continue
		}
		return false
	}
	return true
}

type ConversationCursor struct {
	Scope             Scope                   `json:"scope"`
	ConversationID    string                  `json:"conversationId"`
	Participant       ConversationParticipant `json:"participant"`
	DeliveredSequence int64                   `json:"deliveredSequence"`
	ReadSequence      int64                   `json:"readSequence"`
	Revision          int64                   `json:"revision"`
	UpdatedAt         time.Time               `json:"updatedAt"`
}

func (c *ConversationCursor) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: cursor is required", ErrInvalidConversation)
	}
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if err := c.Participant.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(c.ConversationID, 128) || c.DeliveredSequence < 0 || c.ReadSequence < 0 ||
		c.ReadSequence > c.DeliveredSequence || c.Revision <= 0 || c.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: cursor sequences, revision, and timestamp are invalid", ErrInvalidConversation)
	}
	return nil
}

type ConversationPresenceState string

const (
	// Thinking means the participant is evaluating visible channel context and
	// deciding whether it has a relevant contribution.
	ConversationPresenceThinking ConversationPresenceState = "thinking"
	// Typing means a participant has selected a user-visible contribution and is
	// actively composing or delivering it.
	ConversationPresenceTyping ConversationPresenceState = "typing"
	// Working is reserved for durable work linked through RunID.
	ConversationPresenceWorking ConversationPresenceState = "working"
)

// ConversationPresence is truthful only while its renewable lease is valid.
// Durable work remains represented by Runs; presence is a short projection.
type ConversationPresence struct {
	Scope          Scope                     `json:"scope"`
	ConversationID string                    `json:"conversationId"`
	Participant    ConversationParticipant   `json:"participant"`
	State          ConversationPresenceState `json:"state"`
	Summary        string                    `json:"summary,omitempty"`
	RunID          string                    `json:"runId,omitempty"`
	LeaseID        string                    `json:"leaseId"`
	Revision       int64                     `json:"revision"`
	UpdatedAt      time.Time                 `json:"updatedAt"`
	ExpiresAt      time.Time                 `json:"expiresAt"`
}

func (p *ConversationPresence) Validate() error {
	if p == nil {
		return fmt.Errorf("%w: presence is required", ErrInvalidConversation)
	}
	if err := p.Scope.Validate(); err != nil {
		return err
	}
	if err := p.Participant.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(p.ConversationID, 128) || !validOpaqueIdentifier(p.LeaseID, 128) ||
		(p.RunID != "" && !validOpaqueIdentifier(p.RunID, 128)) || p.Revision <= 0 || p.UpdatedAt.IsZero() || !p.ExpiresAt.After(p.UpdatedAt) {
		return fmt.Errorf("%w: presence identity, lease, revision, and expiry are invalid", ErrInvalidConversation)
	}
	if p.State != ConversationPresenceThinking && p.State != ConversationPresenceTyping && p.State != ConversationPresenceWorking {
		return fmt.Errorf("%w: invalid presence state %q", ErrInvalidConversation, p.State)
	}
	if p.State == ConversationPresenceWorking && strings.TrimSpace(p.RunID) == "" {
		return fmt.Errorf("%w: working presence requires a durable Run", ErrInvalidConversation)
	}
	if p.State != ConversationPresenceWorking && strings.TrimSpace(p.RunID) != "" {
		return fmt.Errorf("%w: only working presence can reference a durable Run", ErrInvalidConversation)
	}
	if len(p.Summary) > 240 {
		return fmt.Errorf("%w: presence summary cannot exceed 240 characters", ErrInvalidConversation)
	}
	return nil
}

func validConversationMessageIntent(intent ConversationMessageIntent) bool {
	switch intent {
	case MessageIntentQuestion, MessageIntentAnswer, MessageIntentUpdate, MessageIntentProposal, MessageIntentDecision,
		MessageIntentObjection, MessageIntentHandoff, MessageIntentEscalation, MessageIntentApprovalRequest, MessageIntentAcknowledgment, MessageIntentSystem:
		return true
	default:
		return false
	}
}
