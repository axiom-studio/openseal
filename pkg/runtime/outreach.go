package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

var (
	ErrOutreachThreadNotFound    = errors.New("outreach thread not found")
	ErrOutreachThreadConflict    = errors.New("outreach thread revision conflict")
	ErrOutreachThreadIdempotency = errors.New("outreach idempotency key was already used with different input")
	ErrInvalidOutreachThread     = errors.New("invalid outreach thread")
)

type OutreachThreadStatus string

const (
	OutreachThreadOpen     OutreachThreadStatus = "open"
	OutreachThreadClosed   OutreachThreadStatus = "closed"
	OutreachThreadCanceled OutreachThreadStatus = "canceled"
)

type OutreachMessageDirection string

const (
	OutreachMessageOutbound OutreachMessageDirection = "outbound"
	OutreachMessageInbound  OutreachMessageDirection = "inbound"
)

type OutreachMessageIntent string

const (
	OutreachIntentClarify         OutreachMessageIntent = "clarify"
	OutreachIntentRequestFeedback OutreachMessageIntent = "request_feedback"
	OutreachIntentAnswer          OutreachMessageIntent = "answer"
	OutreachIntentFollowUp        OutreachMessageIntent = "follow_up"
)

type OutreachMessageStatus string

const (
	OutreachMessageDraft           OutreachMessageStatus = "draft"
	OutreachMessagePendingApproval OutreachMessageStatus = "pending_approval"
	OutreachMessageReady           OutreachMessageStatus = "ready"
	OutreachMessageDelivered       OutreachMessageStatus = "delivered"
	OutreachMessageReceived        OutreachMessageStatus = "received"
	OutreachMessageDeclined        OutreachMessageStatus = "declined"
	OutreachMessageFailed          OutreachMessageStatus = "failed"
	OutreachMessageCanceled        OutreachMessageStatus = "canceled"
)

// OutreachIdentity is public identity information that must accompany an
// outbound engagement. ProfileRef is an opaque host-owned account/profile
// binding; it is never a credential and is safe to expose in audit records.
type OutreachIdentity struct {
	ProfileRef  string `json:"profileRef"`
	DisplayName string `json:"displayName"`
	Affiliation string `json:"affiliation"`
	Disclosure  string `json:"disclosure"`
}

// OutreachCapability selects the exact typed Skill action used to dispatch a
// message. Argument mappings prove which action inputs carry the immutable
// thread target and reviewed body while allowing connector-specific schemas.
type OutreachCapability struct {
	SkillID        string                 `json:"skillId"`
	SkillVersion   string                 `json:"skillVersion"`
	Action         string                 `json:"action"`
	Arguments      map[string]interface{} `json:"arguments"`
	TargetArgument string                 `json:"targetArgument"`
	BodyArgument   string                 `json:"bodyArgument"`
}

type OutreachReceipt struct {
	Provider    string    `json:"provider"`
	ExternalID  string    `json:"externalId"`
	ExternalURI string    `json:"externalUri,omitempty"`
	Digest      string    `json:"digest,omitempty"`
	DeliveredAt time.Time `json:"deliveredAt"`
}

type OutreachMessage struct {
	ID           string                   `json:"id"`
	Direction    OutreachMessageDirection `json:"direction"`
	Intent       OutreachMessageIntent    `json:"intent,omitempty"`
	Body         string                   `json:"body"`
	Status       OutreachMessageStatus    `json:"status"`
	Capability   *OutreachCapability      `json:"capability,omitempty"`
	RunID        string                   `json:"runId,omitempty"`
	ActionCallID string                   `json:"actionCallId,omitempty"`
	ApprovalID   string                   `json:"approvalId,omitempty"`
	Receipt      *OutreachReceipt         `json:"receipt,omitempty"`
	Outcome      string                   `json:"outcome,omitempty"`
	CreatedAt    time.Time                `json:"createdAt"`
	UpdatedAt    time.Time                `json:"updatedAt"`
}

// OutreachThread is a durable, tenant-scoped conversation rooted in immutable
// source evidence. Transport credentials remain in Skill bindings and raw
// provider payloads remain in artifacts; this record contains only reviewed
// content, public provenance, governed action identity, and receipts.
type OutreachThread struct {
	ID                  string               `json:"id"`
	Scope               Scope                `json:"scope"`
	InitiativeID        string               `json:"initiativeId"`
	SourceObservationID string               `json:"sourceObservationId"`
	MonitorID           string               `json:"monitorId"`
	StableSourceID      string               `json:"stableSourceId,omitempty"`
	TargetURI           string               `json:"targetUri"`
	Owner               ObjectiveOwner       `json:"owner"`
	AssignedAgentID     string               `json:"assignedAgentId"`
	SourcePolicyRef     string               `json:"sourcePolicyRef"`
	ApprovalPolicyRef   string               `json:"approvalPolicyRef"`
	Identity            OutreachIdentity     `json:"identity"`
	Status              OutreachThreadStatus `json:"status"`
	Messages            []OutreachMessage    `json:"messages"`
	Revision            int64                `json:"revision"`
	CreatedAt           time.Time            `json:"createdAt"`
	UpdatedAt           time.Time            `json:"updatedAt"`
	IdempotencyKeyHash  string               `json:"idempotencyKeyHash,omitempty"`
	CreationFingerprint string               `json:"creationFingerprint,omitempty"`
}

func (t *OutreachThread) Validate() error {
	if t == nil {
		return errors.New("outreach thread is required")
	}
	if err := t.Scope.Validate(); err != nil {
		return err
	}
	if err := t.Owner.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(t.ID, 128) || !validOpaqueIdentifier(t.InitiativeID, 128) ||
		!validOpaqueIdentifier(t.SourceObservationID, 128) || !validOpaqueIdentifier(t.MonitorID, 128) ||
		!validOpaqueIdentifier(t.AssignedAgentID, 128) {
		return errors.New("outreach thread, Initiative, observation, monitor, and assigned Agent ids are required")
	}
	if err := validateOutreachTarget(t.TargetURI); err != nil {
		return err
	}
	if strings.TrimSpace(t.SourcePolicyRef) == "" || len(t.SourcePolicyRef) > 256 ||
		strings.TrimSpace(t.ApprovalPolicyRef) == "" || len(t.ApprovalPolicyRef) > 256 {
		return errors.New("outreach source and approval policy references are required")
	}
	if err := t.Identity.Validate(); err != nil {
		return err
	}
	switch t.Status {
	case OutreachThreadOpen, OutreachThreadClosed, OutreachThreadCanceled:
	default:
		return errors.New("outreach thread status is invalid")
	}
	if t.Revision < 1 || t.CreatedAt.IsZero() || t.UpdatedAt.IsZero() || t.UpdatedAt.Before(t.CreatedAt) {
		return errors.New("outreach thread revision and timestamps are invalid")
	}
	if len(t.Messages) == 0 || len(t.Messages) > 1000 {
		return errors.New("outreach thread requires between 1 and 1000 messages")
	}
	seen := make(map[string]bool, len(t.Messages))
	for index := range t.Messages {
		message := &t.Messages[index]
		if !validOpaqueIdentifier(message.ID, 128) || seen[message.ID] {
			return errors.New("outreach message ids must be unique portable identifiers")
		}
		seen[message.ID] = true
		if err := message.validate(t); err != nil {
			return fmt.Errorf("outreach message %s: %w", message.ID, err)
		}
	}
	return nil
}

func (i OutreachIdentity) Validate() error {
	if !validOpaqueIdentifier(i.ProfileRef, 256) || strings.TrimSpace(i.DisplayName) == "" || len(i.DisplayName) > 256 ||
		strings.TrimSpace(i.Affiliation) == "" || len(i.Affiliation) > 1000 || strings.TrimSpace(i.Disclosure) == "" || len(i.Disclosure) > 2000 {
		return errors.New("outreach identity requires a profile reference, display name, affiliation, and disclosure")
	}
	return nil
}

func (m *OutreachMessage) validate(thread *OutreachThread) error {
	if strings.TrimSpace(m.Body) == "" || len(m.Body) > 20000 || m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() || m.UpdatedAt.Before(m.CreatedAt) {
		return errors.New("body and valid timestamps are required")
	}
	switch m.Direction {
	case OutreachMessageInbound:
		if m.Status != OutreachMessageReceived || m.Intent != "" || m.Capability != nil || m.RunID != "" || m.ActionCallID != "" || m.ApprovalID != "" {
			return errors.New("inbound messages must be received provider evidence without outbound execution state")
		}
		if m.Receipt == nil || m.Receipt.Validate() != nil {
			return errors.New("inbound messages require a valid provider receipt")
		}
	case OutreachMessageOutbound:
		switch m.Intent {
		case OutreachIntentClarify, OutreachIntentRequestFeedback, OutreachIntentAnswer, OutreachIntentFollowUp:
		default:
			return errors.New("outbound message intent is invalid")
		}
		if !strings.Contains(m.Body, thread.Identity.Disclosure) {
			return errors.New("outbound message must include the configured identity disclosure")
		}
		if m.Capability == nil || m.Capability.Validate(thread.TargetURI, m.Body) != nil {
			return errors.New("outbound message requires an exact valid capability mapping")
		}
		switch m.Status {
		case OutreachMessageDraft:
			if m.RunID != "" || m.ActionCallID != "" || m.ApprovalID != "" || m.Receipt != nil || m.Outcome != "" {
				return errors.New("draft message cannot carry execution state")
			}
		case OutreachMessagePendingApproval:
			if m.RunID == "" || m.ActionCallID == "" || m.ApprovalID == "" || m.Receipt != nil {
				return errors.New("pending message requires Run, action, and approval identity")
			}
		case OutreachMessageReady:
			if m.RunID == "" || m.ActionCallID == "" || m.ApprovalID != "" || m.Receipt != nil {
				return errors.New("ready message requires Run and action identity without an approval checkpoint")
			}
		case OutreachMessageDelivered:
			if m.RunID == "" || m.ActionCallID == "" || m.Receipt == nil || m.Receipt.Validate() != nil {
				return errors.New("delivered message requires Run, action, and valid receipt")
			}
		case OutreachMessageDeclined, OutreachMessageFailed, OutreachMessageCanceled:
			if strings.TrimSpace(m.Outcome) == "" || m.Receipt != nil {
				return errors.New("terminal undelivered message requires an outcome and no receipt")
			}
		default:
			return errors.New("outbound message status is invalid")
		}
	default:
		return errors.New("message direction is invalid")
	}
	return nil
}

func (c *OutreachCapability) Validate(targetURI, body string) error {
	if c == nil || !validOpaqueIdentifier(c.SkillID, 128) || strings.TrimSpace(c.SkillVersion) == "" || len(c.SkillVersion) > 128 ||
		!validOpaqueIdentifier(c.Action, 128) || !validOpaqueIdentifier(c.TargetArgument, 128) || !validOpaqueIdentifier(c.BodyArgument, 128) ||
		c.TargetArgument == c.BodyArgument || c.Arguments == nil {
		return errors.New("outreach capability Skill identity and distinct target/body mappings are required")
	}
	if c.Arguments[c.TargetArgument] != targetURI || c.Arguments[c.BodyArgument] != body {
		return errors.New("outreach capability arguments do not preserve the reviewed target and body")
	}
	return validateCredentialFreeContext(c.Arguments)
}

func (r *OutreachReceipt) Validate() error {
	if r == nil || !validOpaqueIdentifier(r.Provider, 128) || strings.TrimSpace(r.ExternalID) == "" || len(r.ExternalID) > 512 || r.DeliveredAt.IsZero() {
		return errors.New("outreach receipt provider, external id, and delivery time are required")
	}
	if r.ExternalURI != "" {
		if err := validateOutreachReceiptURI(r.ExternalURI); err != nil {
			return err
		}
	}
	if r.Digest != "" {
		if err := validateSHA256Digest(r.Digest); err != nil {
			return err
		}
	}
	return nil
}

func validateOutreachReceiptURI(raw string) error {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || (target.Port() != "" && target.Port() != "443") {
		return errors.New("outreach receipt URI must be a credential-free absolute HTTPS URL")
	}
	return nil
}

func validateOutreachTarget(raw string) error {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || target.Fragment != "" || (target.Port() != "" && target.Port() != "443") {
		return errors.New("outreach target must be a credential-free absolute HTTPS URL")
	}
	return nil
}

type OutreachThreadFilter struct {
	Scope               Scope
	InitiativeID        string
	SourceObservationID string
	Statuses            []OutreachThreadStatus
	Limit               int
	Offset              int
}

type OutreachStore interface {
	CreateOutreachThreadWithEvent(context.Context, *OutreachThread, *ActivityEvent) (*ActivityEvent, error)
	GetOutreachThread(context.Context, Scope, string) (*OutreachThread, error)
	GetOutreachThreadByIdempotency(context.Context, Scope, string) (*OutreachThread, error)
	ListOutreachThreads(context.Context, OutreachThreadFilter) ([]*OutreachThread, error)
	UpdateOutreachThreadWithEvent(context.Context, *OutreachThread, int64, *ActivityEvent) (*ActivityEvent, error)
}

type CreateOutreachThreadRequest struct {
	Thread         *OutreachThread
	IdempotencyKey string
	Actor          ActivityActor
	Visibility     ActivityVisibility
}

type LinkOutreachActionRequest struct {
	ExpectedRevision int64
	MessageID        string
	RunID            string
	ActionCallID     string
	ApprovalID       string
	Actor            ActivityActor
	Visibility       ActivityVisibility
}

type RecordOutreachDeliveryRequest struct {
	ExpectedRevision int64
	MessageID        string
	ActionCallID     string
	Receipt          OutreachReceipt
	Actor            ActivityActor
	Visibility       ActivityVisibility
}

type ResolveOutreachMessageRequest struct {
	ExpectedRevision int64
	MessageID        string
	Status           OutreachMessageStatus
	Outcome          string
	Actor            ActivityActor
	Visibility       ActivityVisibility
}

type OutreachService struct {
	store       OutreachStore
	initiatives InitiativeStore
	sources     SourceMonitorStore
	actions     OutreachActionReader
	now         func() time.Time
	newID       func() string
}

type OutreachActionReader interface {
	GetActionCall(context.Context, Scope, string) (*ActionCall, error)
	GetApproval(context.Context, Scope, string) (*ApprovalCheckpoint, error)
}

func NewOutreachService(store OutreachStore, initiatives InitiativeStore, sources SourceMonitorStore, actions OutreachActionReader) *OutreachService {
	return &OutreachService{store: store, initiatives: initiatives, sources: sources, actions: actions, now: time.Now, newID: uuid.NewString}
}

func (s *OutreachService) Create(ctx context.Context, req CreateOutreachThreadRequest) (*OutreachThread, *ActivityEvent, error) {
	if s == nil || s.store == nil || s.initiatives == nil || s.sources == nil {
		return nil, nil, errors.New("outreach service is not configured")
	}
	thread := cloneOutreachThread(req.Thread)
	if thread == nil {
		return nil, nil, errors.New("outreach thread is required")
	}
	now := s.now().UTC()
	if thread.ID == "" {
		thread.ID = s.newID()
	}
	thread.Status, thread.Revision, thread.CreatedAt, thread.UpdatedAt = OutreachThreadOpen, 1, now, now
	for index := range thread.Messages {
		message := &thread.Messages[index]
		if message.ID == "" {
			message.ID = s.newID()
		}
		message.CreatedAt, message.UpdatedAt = now, now
	}
	if len(thread.Messages) == 0 || thread.Messages[0].Direction != OutreachMessageOutbound || thread.Messages[0].Status != OutreachMessageDraft {
		return nil, nil, errors.New("new outreach thread requires an initial outbound draft")
	}
	fingerprint, err := outreachCreationFingerprint(thread)
	if err != nil {
		return nil, nil, err
	}
	thread.CreationFingerprint = fingerprint
	if req.IdempotencyKey != "" {
		sum := sha256.Sum256([]byte(req.IdempotencyKey))
		thread.IdempotencyKeyHash = hex.EncodeToString(sum[:])
		existing, findErr := s.store.GetOutreachThreadByIdempotency(ctx, thread.Scope, thread.IdempotencyKeyHash)
		if findErr != nil && !errors.Is(findErr, ErrOutreachThreadNotFound) {
			return nil, nil, findErr
		}
		if existing != nil {
			if existing.CreationFingerprint != fingerprint {
				return nil, nil, ErrOutreachThreadIdempotency
			}
			return existing, nil, nil
		}
	}
	if err := thread.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidOutreachThread, err)
	}
	if err := s.validateEvidence(ctx, thread); err != nil {
		return nil, nil, err
	}
	event := outreachEvent(thread, &thread.Messages[0], "outreach.thread_created", "Outreach thread drafted", req.Actor, req.Visibility, now)
	persisted, err := s.store.CreateOutreachThreadWithEvent(ctx, thread, event)
	if err != nil {
		if thread.IdempotencyKeyHash != "" {
			winner, lookupErr := s.store.GetOutreachThreadByIdempotency(ctx, thread.Scope, thread.IdempotencyKeyHash)
			if lookupErr == nil && winner.CreationFingerprint == fingerprint {
				return winner, nil, nil
			}
		}
		return nil, nil, err
	}
	return cloneOutreachThread(thread), persisted, nil
}

func (s *OutreachService) Get(ctx context.Context, scope Scope, id string) (*OutreachThread, error) {
	return s.store.GetOutreachThread(ctx, scope, id)
}

func (s *OutreachService) List(ctx context.Context, filter OutreachThreadFilter) ([]*OutreachThread, error) {
	return s.store.ListOutreachThreads(ctx, filter)
}

func (s *OutreachService) LinkAction(ctx context.Context, scope Scope, id string, req LinkOutreachActionRequest) (*OutreachThread, *ActivityEvent, error) {
	if strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.ActionCallID) == "" {
		return nil, nil, errors.New("outreach Run and action call ids are required")
	}
	if s == nil || s.actions == nil {
		return nil, nil, errors.New("outreach action verifier is not configured")
	}
	thread, err := s.store.GetOutreachThread(ctx, scope, id)
	if err != nil {
		return nil, nil, err
	}
	message := findOutreachMessage(thread, req.MessageID)
	if message == nil || message.Capability == nil {
		return nil, nil, errors.New("outreach message capability is unavailable")
	}
	call, err := s.actions.GetActionCall(ctx, scope, req.ActionCallID)
	if err != nil || call == nil {
		return nil, nil, fmt.Errorf("resolve outreach action: %w", err)
	}
	capability := message.Capability
	if call.RunID != req.RunID || call.DeploymentID != thread.AssignedAgentID || call.SkillID != capability.SkillID || call.SkillVersion != capability.SkillVersion ||
		call.Action != capability.Action || call.SideEffect != skill.SideEffectExternal || strings.TrimSpace(call.IdempotencyKey) == "" ||
		!sameCredentialFreeValue(call.Arguments, capability.Arguments) || !outreachContainsString(call.EvidenceRefs, thread.SourceObservationID) || call.ApprovalID != strings.TrimSpace(req.ApprovalID) {
		return nil, nil, errors.New("outreach action identity, arguments, evidence, policy disposition, or idempotency does not match the reviewed message")
	}
	if call.Status != ActionCallStatusReady && call.Status != ActionCallStatusWaitingApproval {
		return nil, nil, errors.New("outreach action is not ready or awaiting approval")
	}
	if call.ApprovalID != "" {
		approval, approvalErr := s.actions.GetApproval(ctx, scope, call.ApprovalID)
		if approvalErr != nil || approval == nil || approval.ActionCallID != call.ID || approval.RunID != call.RunID || !outreachContainsString(approval.EvidenceRefs, thread.SourceObservationID) {
			return nil, nil, errors.New("outreach approval does not preserve the source evidence linkage")
		}
	}
	return s.mutate(ctx, scope, id, req.ExpectedRevision, req.MessageID, req.Actor, req.Visibility, func(thread *OutreachThread, message *OutreachMessage, now time.Time) (string, string, error) {
		if message.Direction != OutreachMessageOutbound || message.Status != OutreachMessageDraft {
			return "", "", errors.New("only an outbound draft can be linked to an action")
		}
		message.RunID, message.ActionCallID, message.ApprovalID = strings.TrimSpace(req.RunID), strings.TrimSpace(req.ActionCallID), strings.TrimSpace(req.ApprovalID)
		message.Status = OutreachMessageReady
		typeName, summary := "outreach.action_linked", "Outreach message ready for governed delivery"
		if message.ApprovalID != "" {
			message.Status, typeName, summary = OutreachMessagePendingApproval, "outreach.approval_requested", "Outreach message awaiting approval"
		}
		message.UpdatedAt = now
		return typeName, summary, nil
	})
}

func outreachContainsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func sameCredentialFreeValue(left, right map[string]interface{}) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func (s *OutreachService) RecordDelivery(ctx context.Context, scope Scope, id string, req RecordOutreachDeliveryRequest) (*OutreachThread, *ActivityEvent, error) {
	if err := req.Receipt.Validate(); err != nil {
		return nil, nil, err
	}
	current, err := s.store.GetOutreachThread(ctx, scope, id)
	if err != nil {
		return nil, nil, err
	}
	message := findOutreachMessage(current, req.MessageID)
	if message == nil {
		return nil, nil, errors.New("outreach message not found")
	}
	if message != nil && message.Status == OutreachMessageDelivered && message.ActionCallID == req.ActionCallID && sameOutreachReceipt(message.Receipt, &req.Receipt) {
		return current, nil, nil
	}
	if s.actions == nil {
		return nil, nil, errors.New("outreach action verifier is not configured")
	}
	call, err := s.actions.GetActionCall(ctx, scope, req.ActionCallID)
	if err != nil || call == nil || call.Status != ActionCallStatusSucceeded || call.RunID != message.RunID {
		return nil, nil, errors.New("outreach receipt requires the matching succeeded governed action")
	}
	if message.ApprovalID != "" {
		approval, approvalErr := s.actions.GetApproval(ctx, scope, message.ApprovalID)
		if approvalErr != nil || approval == nil || approval.Status != ApprovalStatusApproved || approval.ActionCallID != call.ID {
			return nil, nil, errors.New("outreach receipt requires the matching approved checkpoint")
		}
	}
	return s.mutate(ctx, scope, id, req.ExpectedRevision, req.MessageID, req.Actor, req.Visibility, func(_ *OutreachThread, message *OutreachMessage, now time.Time) (string, string, error) {
		if message.Direction != OutreachMessageOutbound || (message.Status != OutreachMessageReady && message.Status != OutreachMessagePendingApproval) || message.ActionCallID != req.ActionCallID {
			return "", "", errors.New("outreach delivery does not match a governed pending message")
		}
		message.Status, message.Receipt, message.UpdatedAt = OutreachMessageDelivered, cloneOutreachReceipt(&req.Receipt), now
		return "outreach.message_delivered", "Outreach message delivered", nil
	})
}

func (s *OutreachService) ResolveMessage(ctx context.Context, scope Scope, id string, req ResolveOutreachMessageRequest) (*OutreachThread, *ActivityEvent, error) {
	return s.mutate(ctx, scope, id, req.ExpectedRevision, req.MessageID, req.Actor, req.Visibility, func(_ *OutreachThread, message *OutreachMessage, now time.Time) (string, string, error) {
		switch req.Status {
		case OutreachMessageDeclined, OutreachMessageFailed, OutreachMessageCanceled:
		default:
			return "", "", errors.New("outreach resolution status must be declined, failed, or canceled")
		}
		if message.Status == OutreachMessageDelivered || message.Status == OutreachMessageReceived || message.Status == OutreachMessageDeclined || message.Status == OutreachMessageFailed || message.Status == OutreachMessageCanceled {
			return "", "", errors.New("outreach message is already terminal")
		}
		if strings.TrimSpace(req.Outcome) == "" {
			return "", "", errors.New("outreach resolution requires an outcome")
		}
		message.Status, message.Outcome, message.UpdatedAt = req.Status, strings.TrimSpace(req.Outcome), now
		return "outreach.message_" + string(req.Status), "Outreach message " + strings.ReplaceAll(string(req.Status), "_", " "), nil
	})
}

func (s *OutreachService) mutate(ctx context.Context, scope Scope, id string, expected int64, messageID string, actor ActivityActor, visibility ActivityVisibility, apply func(*OutreachThread, *OutreachMessage, time.Time) (string, string, error)) (*OutreachThread, *ActivityEvent, error) {
	if s == nil || s.store == nil {
		return nil, nil, errors.New("outreach service is not configured")
	}
	thread, err := s.store.GetOutreachThread(ctx, scope, id)
	if err != nil {
		return nil, nil, err
	}
	if thread.Revision != expected {
		return nil, nil, ErrOutreachThreadConflict
	}
	message := findOutreachMessage(thread, messageID)
	if message == nil {
		return nil, nil, errors.New("outreach message not found")
	}
	now := s.now().UTC()
	eventType, summary, err := apply(thread, message, now)
	if err != nil {
		return nil, nil, err
	}
	thread.Revision++
	thread.UpdatedAt = now
	if err := thread.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidOutreachThread, err)
	}
	event := outreachEvent(thread, message, eventType, summary, actor, visibility, now)
	persisted, err := s.store.UpdateOutreachThreadWithEvent(ctx, thread, expected, event)
	if err != nil {
		return nil, nil, err
	}
	return cloneOutreachThread(thread), persisted, nil
}

func (s *OutreachService) validateEvidence(ctx context.Context, thread *OutreachThread) error {
	initiative, err := s.initiatives.GetInitiative(ctx, thread.Scope, thread.InitiativeID)
	if err != nil || initiative == nil {
		if err == nil {
			err = ErrInitiativeNotFound
		}
		return fmt.Errorf("outreach Initiative: %w", err)
	}
	if initiative.Owner != thread.Owner {
		return errors.New("outreach owner must match Initiative owner")
	}
	observation, err := s.sources.GetSourceObservation(ctx, thread.Scope, thread.SourceObservationID)
	if err != nil || observation == nil {
		if err == nil {
			err = ErrSourceObservationNotFound
		}
		return fmt.Errorf("outreach source observation: %w", err)
	}
	if observation.InitiativeID != thread.InitiativeID || observation.MonitorID != thread.MonitorID || observation.SourceURI != thread.TargetURI || observation.StableSourceID != thread.StableSourceID {
		return errors.New("outreach target provenance does not match the source observation")
	}
	for _, monitor := range initiative.SourceMonitors {
		if monitor.ID == thread.MonitorID {
			if monitor.SourcePolicyRef != thread.SourcePolicyRef || monitor.AssignedAgentID != thread.AssignedAgentID {
				return errors.New("outreach source policy or assigned Agent does not match the Initiative monitor")
			}
			return nil
		}
	}
	return errors.New("outreach monitor does not belong to Initiative")
}

func outreachEvent(thread *OutreachThread, message *OutreachMessage, eventType, summary string, actor ActivityActor, visibility ActivityVisibility, now time.Time) *ActivityEvent {
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	payload := map[string]interface{}{"threadId": thread.ID, "messageId": message.ID, "messageStatus": message.Status, "sourceObservationId": thread.SourceObservationID}
	if message.ActionCallID != "" {
		payload["actionCallId"] = message.ActionCallID
	}
	if message.ApprovalID != "" {
		payload["approvalId"] = message.ApprovalID
	}
	if message.Receipt != nil {
		payload["provider"] = message.Receipt.Provider
		payload["externalId"] = message.Receipt.ExternalID
	}
	return &ActivityEvent{ID: uuid.NewString(), Scope: thread.Scope, EventType: eventType, Severity: ActivitySeverityInfo,
		InitiativeID: thread.InitiativeID, AgentID: thread.AssignedAgentID, TeamID: func() string {
			if thread.Owner.Type == OwnerTypeTeam {
				return thread.Owner.ID
			}
			return ""
		}(), Actor: actor, Summary: summary, Payload: payload, Visibility: visibility, CreatedAt: now}
}

func outreachCreationFingerprint(thread *OutreachThread) (string, error) {
	copy := cloneOutreachThread(thread)
	copy.ID, copy.IdempotencyKeyHash, copy.CreationFingerprint = "", "", ""
	copy.Revision, copy.CreatedAt, copy.UpdatedAt = 0, time.Time{}, time.Time{}
	for index := range copy.Messages {
		copy.Messages[index].ID = ""
		copy.Messages[index].CreatedAt, copy.Messages[index].UpdatedAt = time.Time{}, time.Time{}
	}
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func findOutreachMessage(thread *OutreachThread, id string) *OutreachMessage {
	if thread == nil {
		return nil
	}
	for index := range thread.Messages {
		if thread.Messages[index].ID == id {
			return &thread.Messages[index]
		}
	}
	return nil
}

func sameOutreachReceipt(left, right *OutreachReceipt) bool {
	return left != nil && right != nil && left.Provider == right.Provider && left.ExternalID == right.ExternalID && left.ExternalURI == right.ExternalURI && left.Digest == right.Digest && left.DeliveredAt.Equal(right.DeliveredAt)
}

func cloneOutreachReceipt(value *OutreachReceipt) *OutreachReceipt {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneOutreachThread(value *OutreachThread) *OutreachThread {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var cloned OutreachThread
	_ = json.Unmarshal(encoded, &cloned)
	return &cloned
}
