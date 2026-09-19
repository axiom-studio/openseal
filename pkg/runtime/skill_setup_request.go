package runtime

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrSkillSetupConflict = errors.New("skill setup request revision conflict")
var ErrInvalidSkillSetup = errors.New("invalid skill setup request")

// SkillSetupRequest is a user interaction, not permission to install a Skill or
// disclose credentials. The requesting action supplies the exact verified target;
// the host applies its usual installation, configuration and credential policy.
// Secrets and authorization URLs never belong in this durable record.
type SkillSetupRequest struct {
	InstallationReference   string    `json:"installationReference,omitempty"`
	SourceDigest            string    `json:"sourceDigest,omitempty"`
	RequiredActions         []string  `json:"requiredActions,omitempty"`
	EnablePrompt            bool      `json:"enablePrompt,omitempty"`
	ID                      string    `json:"id"`
	Scope                   Scope     `json:"scope"`
	DeploymentID            string    `json:"deploymentId"`
	ConversationID          string    `json:"conversationId"`
	TriggerMessageID        string    `json:"triggerMessageId"`
	RunID                   string    `json:"runId"`
	ActionCallID            string    `json:"actionCallId"`
	Kind                    string    `json:"kind"`
	SkillID                 string    `json:"skillId"`
	SkillVersion            string    `json:"skillVersion"`
	SourceIdentity          string    `json:"sourceIdentity,omitempty"`
	SkillName               string    `json:"skillName"`
	BindingID               string    `json:"bindingId,omitempty"`
	BindingRevision         int64     `json:"bindingRevision"`
	Reason                  string    `json:"reason"`
	Status                  string    `json:"status"`
	Revision                int64     `json:"revision"`
	CreatedAt               time.Time `json:"createdAt"`
	UpdatedAt               time.Time `json:"updatedAt"`
	ResolvedBindingID       string    `json:"resolvedBindingId,omitempty"`
	ResolvedBindingRevision int64     `json:"resolvedBindingRevision,omitempty"`
	ResolvedBy              string    `json:"resolvedBy,omitempty"`
}

func (r *SkillSetupRequest) Validate() error {
	if r == nil || r.Scope.Validate() != nil || r.Revision < 1 || r.BindingRevision < 0 || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return ErrInvalidSkillSetup
	}
	for _, value := range []string{r.ID, r.DeploymentID, r.ConversationID, r.TriggerMessageID, r.RunID, r.ActionCallID, r.SkillID, r.SkillVersion} {
		if !validOpaqueIdentifier(value, 256) {
			return ErrInvalidSkillSetup
		}
	}
	if strings.TrimSpace(r.SkillName) == "" || len(r.SkillName) > 256 || strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 1000 {
		return ErrInvalidSkillSetup
	}
	switch r.Kind {
	case "install", "configure", "reauthorize":
	default:
		return ErrInvalidSkillSetup
	}
	if r.Kind == "reauthorize" && (r.BindingID == "" || r.BindingRevision < 1) {
		return ErrInvalidSkillSetup
	}
	switch r.Status {
	case "pending":
		if r.ResolvedBy != "" || r.ResolvedBindingID != "" || r.ResolvedBindingRevision != 0 {
			return ErrInvalidSkillSetup
		}
	case "resolved":
		if r.ResolvedBy == "" || r.ResolvedBindingID == "" || r.ResolvedBindingRevision < 1 {
			return ErrInvalidSkillSetup
		}
	case "dismissed":
		if r.ResolvedBy == "" {
			return ErrInvalidSkillSetup
		}
	default:
		return ErrInvalidSkillSetup
	}
	return nil
}

type SkillSetupRequestStore interface {
	GetSkillSetupRequest(context.Context, Scope, string) (*SkillSetupRequest, error)
	ListSkillSetupRequests(context.Context, Scope, string, string) ([]*SkillSetupRequest, error)
	SaveSkillSetupRequest(context.Context, *SkillSetupRequest, int64) error
}
