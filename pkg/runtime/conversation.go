package runtime

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

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
	ID           string                 `json:"id"`
	Scope        Scope                  `json:"scope"`
	Owner        ObjectiveOwner         `json:"owner"`
	Title        string                 `json:"title"`
	Origin       *ConversationReference `json:"origin,omitempty"`
	Status       ConversationStatus     `json:"status"`
	LastSequence int64                  `json:"lastSequence"`
	Revision     int64                  `json:"revision"`
	CreatedAt    time.Time              `json:"createdAt"`
	UpdatedAt    time.Time              `json:"updatedAt"`
	ArchivedAt   *time.Time             `json:"archivedAt,omitempty"`
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
	if c.Origin != nil {
		if err := c.Origin.Validate(); err != nil {
			return err
		}
	}
	if c.Revision <= 0 || c.LastSequence < 0 || c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: positive revision, sequence, and timestamps are required", ErrInvalidConversation)
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
	case ConversationAudienceChannel:
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
	case ConversationAudienceChannel:
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
	ConversationReferenceProject        ConversationReferenceKind = "project"
	ConversationReferenceRun            ConversationReferenceKind = "run"
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
)

type ConversationReference struct {
	Kind    ConversationReferenceKind `json:"kind"`
	ID      string                    `json:"id"`
	Version int64                     `json:"version,omitempty"`
}

func (r ConversationReference) Validate() error {
	switch r.Kind {
	case ConversationReferenceObjective, ConversationReferenceProject, ConversationReferenceRun, ConversationReferenceRequest,
		ConversationReferenceApproval, ConversationReferenceArtifact, ConversationReferenceActivity,
		ConversationReferenceExternalSource, ConversationReferenceAgentControl, ConversationReferenceAgentApprovals,
		ConversationReferenceEmbedSession:
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
	ID                string                    `json:"id"`
	Scope             Scope                     `json:"scope"`
	ConversationID    string                    `json:"conversationId"`
	Sequence          int64                     `json:"sequence"`
	Sender            ConversationParticipant   `json:"sender"`
	SenderDisplayName string                    `json:"senderDisplayName,omitempty"`
	Intent            ConversationMessageIntent `json:"intent"`
	Content           string                    `json:"content"`
	// ContributionKey is a privacy-safe, normalized semantic claim identifier
	// used to suppress paraphrased pile-ons (for example rollout-owner:agent-37).
	// It is user-visible metadata, never hidden reasoning.
	ContributionKey      string                    `json:"contributionKey,omitempty"`
	Audience             ConversationAudience      `json:"audience"`
	ThreadRootID         string                    `json:"threadRootId,omitempty"`
	ReplyToMessageID     string                    `json:"replyToMessageId,omitempty"`
	BroadcastToChannel   bool                      `json:"broadcastToChannel,omitempty"`
	Mentions             []ConversationParticipant `json:"mentions,omitempty"`
	References           []ConversationReference   `json:"references,omitempty"`
	RequiresResponse     bool                      `json:"requiresResponse,omitempty"`
	ResolvesMessageID    string                    `json:"resolvesMessageId,omitempty"`
	SupersedesMessageID  string                    `json:"supersedesMessageId,omitempty"`
	IdempotencyKey       string                    `json:"idempotencyKey,omitempty"`
	ParticipationRoundID string                    `json:"participationRoundId,omitempty"`
	Historical           bool                      `json:"historical,omitempty"`
	CreatedAt            time.Time                 `json:"createdAt"`
}

func (m *ChannelMessage) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: message is required", ErrInvalidConversation)
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
	if len(m.SenderDisplayName) > 160 || strings.ContainsAny(m.SenderDisplayName, "\r\n") {
		return fmt.Errorf("%w: sender display name cannot exceed 160 characters or contain line breaks", ErrInvalidConversation)
	}
	if !validConversationMessageIntent(m.Intent) || strings.TrimSpace(m.Content) == "" || len(m.Content) > 65536 {
		return fmt.Errorf("%w: valid intent and content of at most 64 KiB are required", ErrInvalidConversation)
	}
	if !validConversationContributionKey(m.ContributionKey) {
		return fmt.Errorf("%w: contribution key must be normalized lowercase metadata", ErrInvalidConversation)
	}
	if err := m.Audience.Validate(); err != nil {
		return err
	}
	for _, id := range []string{m.ThreadRootID, m.ReplyToMessageID, m.ResolvesMessageID, m.SupersedesMessageID, m.ParticipationRoundID} {
		if id != "" && !validOpaqueIdentifier(id, 128) {
			return fmt.Errorf("%w: linked message and round ids must be portable", ErrInvalidConversation)
		}
	}
	if m.BroadcastToChannel && m.ThreadRootID == "" {
		return fmt.Errorf("%w: only a thread reply can be broadcast to its channel", ErrInvalidConversation)
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
