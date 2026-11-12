package models

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/skill"
)

var ErrSkillSetupConflict = errors.New("skill setup request revision conflict")
var ErrInvalidSkillSetup = errors.New("invalid skill setup request")

// SkillSetupKindConnectAccount asks a person to sign in to an account that a
// bound action needs before it can dispatch. ConnectAccountWakeType is the
// wake condition of a run parked on one; its ID is
// "connect-account:<actionCallID>".
const (
	SkillSetupKindConnectAccount = "connect_account"
	ConnectAccountWakeType       = "connect_account"
)

// Setup request modes: whose sign-in answers the request.
const (
	SkillSetupModePerPerson = "per_person"
	SkillSetupModeShared    = "shared"
)

// ConnectAccountSlot is one binding credential slot a connect_account request
// asks to be filled, with the requirement the sign-in must satisfy.
type ConnectAccountSlot struct {
	Name        string                      `json:"name"`
	Requirement skill.CredentialRequirement `json:"requirement"`
}

// SkillSetupValidationError identifies a public contract field without copying
// user arguments, credentials, or internal identifiers into an error.
type SkillSetupValidationError struct {
	Field   string
	Problem string
}

func (e *SkillSetupValidationError) Error() string {
	return "invalid Skill setup: " + e.Field + " " + e.Problem
}
func (e *SkillSetupValidationError) Unwrap() error { return ErrInvalidSkillSetup }

func NewSkillSetupValidationError(field, problem string) error {
	return &SkillSetupValidationError{Field: field, Problem: problem}
}

type SkillSetupPhase string

const (
	SkillSetupPhaseConfiguration  SkillSetupPhase = "configuration"
	SkillSetupPhaseBindingUpgrade SkillSetupPhase = "binding_upgrade"
)

// SkillSetupRequest is a user interaction, not permission to install a Skill or
// disclose credentials. The requesting action supplies the exact verified target;
// the host applies its usual installation, configuration and credential policy.
// Secrets and authorization URLs never belong in this durable record.
type SkillSetupRequest struct {
	InstallationReference string   `json:"installationReference,omitempty"`
	SourceDigest          string   `json:"sourceDigest,omitempty"`
	RequiredActions       []string `json:"requiredActions,omitempty"`
	EnablePrompt          bool     `json:"enablePrompt,omitempty"`
	ID                    string   `json:"id"`
	Scope                 Scope    `json:"scope"`
	// Owner is the Agent or Team the connection is saved to: the Agent of a
	// 1:1 chat, the Team of a Team chat, or the Agent a builder acts for
	// (targetAgentId). DeploymentID is always Owner.ID.
	Owner        ObjectiveOwner `json:"owner"`
	DeploymentID string         `json:"deploymentId"`
	// AgentID is the acting member that asked. For an Agent owner it is the
	// Agent itself, unless a builder asked for another Agent: then it is the
	// builder, whose own chat holds the card (ChatOwner).
	AgentID          string `json:"agentId"`
	ConversationID   string `json:"conversationId"`
	TriggerMessageID string `json:"triggerMessageId"`
	RunID            string `json:"runId"`
	ActionCallID     string `json:"actionCallId"`
	Kind             string `json:"kind"`
	SkillID          string `json:"skillId"`
	SkillVersion     string `json:"skillVersion"`
	SourceIdentity   string `json:"sourceIdentity,omitempty"`
	SkillName        string `json:"skillName"`
	BindingID        string `json:"bindingId,omitempty"`
	BindingRevision  int64  `json:"bindingRevision"`
	// Phase separates an executable-version migration from the user saving
	// configuration. An absent phase is the legacy configuration phase.
	Phase                   SkillSetupPhase `json:"phase,omitempty"`
	BindingVersion          string          `json:"bindingVersion,omitempty"`
	Reason                  string          `json:"reason"`
	Status                  string          `json:"status"`
	Revision                int64           `json:"revision"`
	CreatedAt               time.Time       `json:"createdAt"`
	UpdatedAt               time.Time       `json:"updatedAt"`
	ResolvedBindingID       string          `json:"resolvedBindingId,omitempty"`
	ResolvedBindingRevision int64           `json:"resolvedBindingRevision,omitempty"`
	ResolvedBy              string          `json:"resolvedBy,omitempty"`
	// App is the connected app a connect_app request asks the user to
	// connect. It is set only for that kind, whose target Skill is always
	// openseal.mcp and whose resolution names the connection's binding.
	App *ConnectAppTarget `json:"app,omitempty"`
	// RequestedFor is the human who must act. Empty means the umbrella's
	// managers act.
	RequestedFor string `json:"requestedFor,omitempty"`
	// Mode is per_person or shared for connect_account; per_person, shared
	// or empty (the app is not enabled on the umbrella yet) for connect_app;
	// and empty for setup.
	Mode string `json:"mode,omitempty"`
	// Slots are the credential slots a connect_account request fills.
	Slots []ConnectAccountSlot `json:"slots,omitempty"`
}

func (r *SkillSetupRequest) Validate() error {
	if r == nil {
		return NewSkillSetupValidationError("request", "is required")
	}
	if r.Scope.Validate() != nil {
		return NewSkillSetupValidationError("scope", "must identify an authorized workspace")
	}
	if err := validateInteractionUmbrella(r.Owner, r.DeploymentID, r.AgentID, true); err != nil {
		return NewSkillSetupValidationError(err.Error(), "must name the umbrella and acting Agent")
	}
	if !IsValidActingUser(r.RequestedFor) {
		return NewSkillSetupValidationError("requestedFor", "must be empty or a bounded identifier")
	}
	if r.Kind != SkillSetupKindConnectAccount && len(r.Slots) > 0 {
		return NewSkillSetupValidationError("slots", "are only valid for connect_account")
	}
	if r.Revision < 1 || r.BindingRevision < 0 {
		return NewSkillSetupValidationError("revision", "must be a valid positive request revision and non-negative binding revision")
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return NewSkillSetupValidationError("timestamps", "must describe a valid request lifecycle")
	}
	for _, field := range []struct{ name, value string }{{"id", r.ID}, {"deploymentId", r.DeploymentID}, {"conversationId", r.ConversationID}, {"triggerMessageId", r.TriggerMessageID}, {"runId", r.RunID}, {"actionCallId", r.ActionCallID}, {"skillId", r.SkillID}, {"skillVersion", r.SkillVersion}} {
		value := field.value
		if !IsValidOpaqueIdentifier(value, 256) {
			return NewSkillSetupValidationError(field.name, "must be a non-empty bounded identifier")
		}
	}
	if strings.TrimSpace(r.SkillName) == "" || len(r.SkillName) > 256 {
		return NewSkillSetupValidationError("skillName", "must contain at most 256 bytes of display text")
	}
	if strings.TrimSpace(r.Reason) == "" || utf8.RuneCountInString(r.Reason) > 1000 {
		return NewSkillSetupValidationError("reason", "must contain between 1 and 1000 characters")
	}
	switch r.Kind {
	case "install", "configure", "reauthorize":
		if r.App != nil {
			return NewSkillSetupValidationError("app", "is only valid for connect_app")
		}
		if r.Mode != "" {
			return NewSkillSetupValidationError("mode", "is only valid for connect_app and connect_account")
		}
	case SkillSetupKindConnectAccount:
		if r.App != nil {
			return NewSkillSetupValidationError("app", "is only valid for connect_app")
		}
		if err := validateConnectAccountSlots(r.Slots); err != nil {
			return err
		}
		if !IsValidOpaqueIdentifier(r.BindingID, 256) || r.Phase == SkillSetupPhaseBindingUpgrade || len(r.RequiredActions) > 0 || r.EnablePrompt || r.InstallationReference != "" {
			return NewSkillSetupValidationError("kind", "connect_account requests carry only the binding and slots to connect")
		}
		switch r.Mode {
		case SkillSetupModePerPerson:
			if r.RequestedFor == "" {
				return NewSkillSetupValidationError("requestedFor", "is required for a per_person request")
			}
		case SkillSetupModeShared:
		default:
			return NewSkillSetupValidationError("mode", "must be per_person or shared")
		}
	case SkillSetupKindConnectApp:
		if err := r.App.validate(); err != nil {
			return err
		}
		switch r.Mode {
		case "", SkillSetupModePerPerson, SkillSetupModeShared:
		default:
			return NewSkillSetupValidationError("mode", "must be per_person, shared or empty")
		}
		if r.BindingID != "" || r.BindingRevision != 0 || r.Phase == SkillSetupPhaseBindingUpgrade || len(r.RequiredActions) > 0 || r.EnablePrompt || r.InstallationReference != "" {
			return NewSkillSetupValidationError("kind", "connect_app requests carry only the app to connect")
		}
	default:
		return NewSkillSetupValidationError("kind", "must be install, configure, reauthorize, connect_app, or connect_account")
	}
	if r.Kind == "reauthorize" && (r.BindingID == "" || r.BindingRevision < 1) {
		return NewSkillSetupValidationError("binding", "must identify an existing revision for reauthorization")
	}
	switch r.Phase {
	case "", SkillSetupPhaseConfiguration:
		if r.BindingVersion != "" && r.BindingVersion != r.SkillVersion {
			return NewSkillSetupValidationError("bindingVersion", "must match the configuration target")
		}
	case SkillSetupPhaseBindingUpgrade:
		if r.BindingID == "" || r.BindingRevision < 1 || !IsValidOpaqueIdentifier(r.BindingVersion, 256) || r.BindingVersion == r.SkillVersion || r.Status == "resolved" {
			return NewSkillSetupValidationError("phase", "requires an existing account and a different target version before configuration")
		}
	default:
		return NewSkillSetupValidationError("phase", "must be configuration or binding_upgrade")
	}
	switch r.Status {
	case "pending":
		if r.ResolvedBy != "" || r.ResolvedBindingID != "" || r.ResolvedBindingRevision != 0 {
			return NewSkillSetupValidationError("status", "cannot contain completion evidence while pending")
		}
	case "resolved":
		if r.ResolvedBy == "" || r.ResolvedBindingID == "" || r.ResolvedBindingRevision < 1 {
			return NewSkillSetupValidationError("status", "requires a saved binding and actor before resolution")
		}
	case "dismissed":
		if r.ResolvedBy == "" {
			return NewSkillSetupValidationError("status", "requires an actor before dismissal")
		}
	default:
		return NewSkillSetupValidationError("status", "must be pending, resolved, or dismissed")
	}
	return nil
}

// validateConnectAccountSlots requires at least one slot, each naming a
// unique bounded slot and the credential requirement of that slot.
func validateConnectAccountSlots(slots []ConnectAccountSlot) error {
	if len(slots) == 0 || len(slots) > 32 {
		return NewSkillSetupValidationError("slots", "must name between 1 and 32 credential slots")
	}
	seen := make(map[string]bool, len(slots))
	for _, slot := range slots {
		if !IsValidOpaqueIdentifier(slot.Name, 256) || slot.Name != strings.TrimSpace(slot.Name) || seen[slot.Name] {
			return NewSkillSetupValidationError("slots", "must name unique bounded credential slots")
		}
		seen[slot.Name] = true
		if slot.Requirement.Name != slot.Name || strings.TrimSpace(slot.Requirement.Kind) == "" || len(slot.Requirement.Kind) > 256 {
			return NewSkillSetupValidationError("slots", "must carry the credential requirement of each slot")
		}
	}
	return nil
}

// validateInteractionUmbrella checks the owner stamp shared by setup and
// credential requests and returns the offending field.
// validateInteractionUmbrella checks a request's owner and acting member.
// targetable allows an Agent owner asked for by another Agent: a builder
// acting for it from the builder's own chat.
func validateInteractionUmbrella(owner ObjectiveOwner, deploymentID, agentID string, targetable bool) error {
	switch {
	case owner.Validate() != nil:
		return errors.New("owner")
	case deploymentID != owner.ID:
		return errors.New("deploymentId")
	case !IsValidOpaqueIdentifier(agentID, 256), owner.Type == OwnerTypeAgent && agentID != owner.ID && !targetable:
		return errors.New("agentId")
	}
	return nil
}

// InteractionChatOwner is the umbrella of the chat an in-chat request was
// made in, whose Run it resumes: owner, except for a request a builder made
// for another Agent, which is in the builder's own 1:1 chat.
func InteractionChatOwner(owner ObjectiveOwner, agentID string) ObjectiveOwner {
	if owner.Type == OwnerTypeAgent && agentID != "" && agentID != owner.ID {
		return ObjectiveOwner{Type: OwnerTypeAgent, ID: agentID}
	}
	return owner
}

// ChatOwner is the umbrella of the chat the request's card is in and whose
// Run it resumes. It is Owner, except for a request a builder made for
// another Agent (targetAgentId): that card is in the builder's own chat.
func (r *SkillSetupRequest) ChatOwner() ObjectiveOwner {
	if r == nil {
		return ObjectiveOwner{}
	}
	return InteractionChatOwner(r.Owner, r.AgentID)
}

// Targeted reports a request a builder made for another Agent.
func (r *SkillSetupRequest) Targeted() bool {
	return r != nil && r.ChatOwner() != r.Owner
}

type SkillSetupRequestStore interface {
	GetSkillSetupRequest(context.Context, Scope, string) (*SkillSetupRequest, error)
	ListSkillSetupRequests(context.Context, Scope, string, string) ([]*SkillSetupRequest, error)
	SaveSkillSetupRequest(context.Context, *SkillSetupRequest, int64) error
}

func CloneSkillSetupRequest(r *SkillSetupRequest) *SkillSetupRequest {
	if r == nil {
		return nil
	}
	c := *r
	c.RequiredActions = append([]string(nil), r.RequiredActions...)
	return &c
}
