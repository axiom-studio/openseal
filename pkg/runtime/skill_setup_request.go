package runtime

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrSkillSetupConflict = errors.New("skill setup request revision conflict")
var ErrInvalidSkillSetup = errors.New("invalid skill setup request")

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

func invalidSkillSetupField(field, problem string) error {
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
	DeploymentID          string   `json:"deploymentId"`
	ConversationID        string   `json:"conversationId"`
	TriggerMessageID      string   `json:"triggerMessageId"`
	RunID                 string   `json:"runId"`
	ActionCallID          string   `json:"actionCallId"`
	Kind                  string   `json:"kind"`
	SkillID               string   `json:"skillId"`
	SkillVersion          string   `json:"skillVersion"`
	SourceIdentity        string   `json:"sourceIdentity,omitempty"`
	SkillName             string   `json:"skillName"`
	BindingID             string   `json:"bindingId,omitempty"`
	BindingRevision       int64    `json:"bindingRevision"`
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
}

func (r *SkillSetupRequest) Validate() error {
	if r == nil {
		return invalidSkillSetupField("request", "is required")
	}
	if r.Scope.Validate() != nil {
		return invalidSkillSetupField("scope", "must identify an authorized workspace")
	}
	if r.Revision < 1 || r.BindingRevision < 0 {
		return invalidSkillSetupField("revision", "must be a valid positive request revision and non-negative binding revision")
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return invalidSkillSetupField("timestamps", "must describe a valid request lifecycle")
	}
	for _, field := range []struct{ name, value string }{{"id", r.ID}, {"deploymentId", r.DeploymentID}, {"conversationId", r.ConversationID}, {"triggerMessageId", r.TriggerMessageID}, {"runId", r.RunID}, {"actionCallId", r.ActionCallID}, {"skillId", r.SkillID}, {"skillVersion", r.SkillVersion}} {
		value := field.value
		if !validOpaqueIdentifier(value, 256) {
			return invalidSkillSetupField(field.name, "must be a non-empty bounded identifier")
		}
	}
	if strings.TrimSpace(r.SkillName) == "" || len(r.SkillName) > 256 {
		return invalidSkillSetupField("skillName", "must contain at most 256 bytes of display text")
	}
	if strings.TrimSpace(r.Reason) == "" || utf8.RuneCountInString(r.Reason) > 1000 {
		return invalidSkillSetupField("reason", "must contain between 1 and 1000 characters")
	}
	switch r.Kind {
	case "install", "configure", "reauthorize":
	default:
		return invalidSkillSetupField("kind", "must be install, configure, or reauthorize")
	}
	if r.Kind == "reauthorize" && (r.BindingID == "" || r.BindingRevision < 1) {
		return invalidSkillSetupField("binding", "must identify an existing revision for reauthorization")
	}
	switch r.Phase {
	case "", SkillSetupPhaseConfiguration:
		if r.BindingVersion != "" && r.BindingVersion != r.SkillVersion {
			return invalidSkillSetupField("bindingVersion", "must match the configuration target")
		}
	case SkillSetupPhaseBindingUpgrade:
		if r.BindingID == "" || r.BindingRevision < 1 || !validOpaqueIdentifier(r.BindingVersion, 256) || r.BindingVersion == r.SkillVersion || r.Status == "resolved" {
			return invalidSkillSetupField("phase", "requires an existing account and a different target version before configuration")
		}
	default:
		return invalidSkillSetupField("phase", "must be configuration or binding_upgrade")
	}
	switch r.Status {
	case "pending":
		if r.ResolvedBy != "" || r.ResolvedBindingID != "" || r.ResolvedBindingRevision != 0 {
			return invalidSkillSetupField("status", "cannot contain completion evidence while pending")
		}
	case "resolved":
		if r.ResolvedBy == "" || r.ResolvedBindingID == "" || r.ResolvedBindingRevision < 1 {
			return invalidSkillSetupField("status", "requires a saved binding and actor before resolution")
		}
	case "dismissed":
		if r.ResolvedBy == "" {
			return invalidSkillSetupField("status", "requires an actor before dismissal")
		}
	default:
		return invalidSkillSetupField("status", "must be pending, resolved, or dismissed")
	}
	return nil
}

type SkillSetupRequestStore interface {
	GetSkillSetupRequest(context.Context, Scope, string) (*SkillSetupRequest, error)
	ListSkillSetupRequests(context.Context, Scope, string, string) ([]*SkillSetupRequest, error)
	SaveSkillSetupRequest(context.Context, *SkillSetupRequest, int64) error
}
