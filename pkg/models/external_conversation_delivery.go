package models

import (
	"fmt"
	"mime"
	"strings"
	"time"
)

// Provider file IDs are durable references. Private URLs and bytes never enter
// normalized ingress, outbox progress, or model-visible context.
type ExternalConversationAttachment struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType,omitempty"`
	SizeBytes int64  `json:"sizeBytes"`
}

func ValidateExternalAttachments(files []ExternalConversationAttachment) error {
	if len(files) > 8 {
		return ErrInvalidExternalConversation
	}
	seen := map[string]bool{}
	for _, file := range files {
		if !IsValidExternalConversationReference(file.ID, 256) || seen[file.ID] || strings.TrimSpace(file.Name) == "" || len(file.Name) > 256 || strings.ContainsAny(file.Name, "\r\n\x00") || file.SizeBytes < 0 || len(file.MediaType) > 128 {
			return ErrInvalidExternalConversation
		}
		if file.MediaType != "" {
			if _, _, err := mime.ParseMediaType(file.MediaType); err != nil {
				return ErrInvalidExternalConversation
			}
		}
		seen[file.ID] = true
	}
	return nil
}

// ExternalConversationIngressGateway pins one shared provider webhook to an
// immutable Skill-owned verifier. It may live in a host/platform scope while
// the endpoints selected from its verified routing claims remain tenant
// scoped. Provider application secrets are supplied only by the Skill host.
type ExternalConversationIngressGateway struct {
	Scope Scope `json:"scope"`
	// Owner is the Agent or Team whose binding the gateway serves;
	// DeploymentID is always Owner.ID.
	Owner          ObjectiveOwner                       `json:"owner"`
	DeploymentID   string                               `json:"deploymentId"`
	Adapter        ExternalConversationAdapterReference `json:"adapter"`
	Provider       string                               `json:"provider"`
	InstallationID string                               `json:"installationId,omitempty"`
	ApplicationID  string                               `json:"applicationId,omitempty"`
}

func (g ExternalConversationIngressGateway) Validate() error {
	if g.Scope.Validate() != nil || g.Owner.Validate() != nil || g.DeploymentID != g.Owner.ID ||
		!IsValidAgentReference(strings.TrimSpace(g.DeploymentID), 256) || !IsValidOpaqueIdentifier(strings.TrimSpace(g.Provider), 128) {
		return ErrInvalidExternalConversation
	}
	if (g.InstallationID != "" && !IsValidOpaqueIdentifier(g.InstallationID, 1024)) ||
		(g.ApplicationID != "" && (!IsValidOpaqueIdentifier(g.ApplicationID, 1024) || g.InstallationID == "")) {
		return ErrInvalidExternalConversation
	}
	if g.Scope.Kind == "platform" && (g.InstallationID != "" || g.ApplicationID != "") {
		return fmt.Errorf("%w: platform gateway authority comes from tenant endpoint mappings", ErrInvalidExternalConversation)
	}
	return g.Adapter.Validate()
}

// ExternalConversationVerifiedRoute contains only routing facts asserted
// after a Skill has authenticated the provider request. It is never populated
// from an unverified HTTP body by the kernel or generic host.
type ExternalConversationVerifiedRoute struct {
	Provider       string `json:"provider"`
	SkillID        string `json:"skillId"`
	SkillVersion   string `json:"skillVersion"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
	AdapterID      string `json:"adapterId"`
	InstallationID string `json:"installationId"`
	ApplicationID  string `json:"applicationId,omitempty"`
	Address        string `json:"address"`
}

func (r ExternalConversationVerifiedRoute) Validate() error {
	for _, value := range []string{r.Provider, r.SkillID, r.SkillVersion, r.AdapterID, r.InstallationID, r.Address} {
		if !IsValidOpaqueIdentifier(strings.TrimSpace(value), 1024) {
			return ErrInvalidExternalConversation
		}
	}
	if r.ApplicationID != "" && !IsValidOpaqueIdentifier(strings.TrimSpace(r.ApplicationID), 1024) {
		return ErrInvalidExternalConversation
	}
	if len(r.SourceIdentity) > 1024 || strings.ContainsAny(r.SourceIdentity, "\r\n") {
		return ErrInvalidExternalConversation
	}
	return nil
}

// ExternalConversationReplyStore is the durable kernel view required to turn
// a finished conversation Run into one canonical channel reply and one
// provider delivery. It deliberately contains no provider-specific behavior.
type ExternalConversationReplyStore interface {
	ExternalConversationStore
	PortfolioStore
}

type ExternalConversationStore interface {
	ExternalConversationEndpointStore
	ExternalConversationGatewayStore
	ExternalConversationTransportStore
	ConversationStore
}

func ExternalConversationAdapterBelongsToEndpoint(endpoint, captured ExternalConversationAdapterReference) bool {
	return endpoint.BindingID == captured.BindingID && endpoint.AdapterID == captured.AdapterID
}

// ExternalMessageSource describes provider provenance, never authorization.
// Names are provider-supplied labels; stable IDs distinguish namesakes.
type ExternalMessageSource struct {
	Provider               string    `json:"provider"`
	WorkspaceID            string    `json:"workspaceId,omitempty"`
	ChannelID              string    `json:"channelId"`
	ChannelName            string    `json:"channelName,omitempty"`
	ChannelType            string    `json:"channelType,omitempty"`
	ThreadID               string    `json:"threadId,omitempty"`
	MessageID              string    `json:"messageId"`
	ParticipantID          string    `json:"participantId"`
	ParticipantDisplayName string    `json:"participantDisplayName,omitempty"`
	OccurredAt             time.Time `json:"occurredAt,omitempty"`
}

func CloneExternalMessageSource(in *ExternalMessageSource) *ExternalMessageSource {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
