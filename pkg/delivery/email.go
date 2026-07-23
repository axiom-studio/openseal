// Package delivery defines portable, governed outbound-delivery capabilities.
// Provider credentials, artifact bytes, and network transport remain host
// responsibilities and never enter model-visible Skill inputs or receipts.
package delivery

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	SkillID             = "openseal.delivery"
	SkillVersion        = "1.2.1"
	SkillImage          = "axiomstudio/skill-openseal-delivery:1.2.1"
	SendEmail           = "send_email"
	EmailCredentialName = "email-delivery"
	EmailCredentialKind = "email-delivery"
	MaximumRecipients   = 20
	MaximumArtifactRefs = 20

	// ActionCallIDTransportKey and AttachmentsTransportKey are reserved for
	// trusted host transport. They are absent from the action schema and must
	// never be accepted from model-generated arguments.
	ActionCallIDTransportKey = "_opensealDeliveryActionCallId"
	AttachmentsTransportKey  = "_opensealEmailAttachments"
)

// SkillDefinition describes email as an external, approval-governed action.
// The sender identity and provider configuration are deliberately absent from
// action inputs: they come from the opaque email-delivery credential binding.
func SkillDefinition() *skill.Definition {
	artifactReference := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"required": []interface{}{"id", "version"},
		"properties": map[string]interface{}{
			"id":      map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256},
			"version": map[string]interface{}{"type": "integer", "minimum": 1},
		},
	}
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Governed delivery",
		Description: "Deliver reviewed messages and durable artifacts through authorized channels.",
		Icon:        "mail", Category: "communication", Tags: []string{"email", "smtp", "delivery", "artifact"},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		Actions: map[string]skill.Action{SendEmail: {
			Name: SendEmail, Description: "Send a reviewed email through the bound delivery identity.",
			SemanticArguments: map[string]string{
				"target": "to",
				"body":   "body",
			},
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"required": []interface{}{"to", "subject", "body"},
				"properties": map[string]interface{}{
					"to":           map[string]interface{}{"type": "array", "minItems": 1, "maxItems": MaximumRecipients, "uniqueItems": true, "items": map[string]interface{}{"type": "string", "format": "email", "pattern": `^[^\s@]+@[^\s@]+\.[^\s@]+$`, "maxLength": 320}},
					"subject":      map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 300},
					"body":         map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 500000},
					"artifactRefs": map[string]interface{}{"type": "array", "maxItems": MaximumArtifactRefs, "uniqueItems": true, "items": artifactReference},
				},
			},
			OutputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"required": []interface{}{"receiptId", "status", "recipientCount", "deliveredAt", "artifactRefs"},
				"properties": map[string]interface{}{
					"receiptId":      map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 512},
					"status":         map[string]interface{}{"type": "string", "enum": []interface{}{"accepted"}},
					"recipientCount": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaximumRecipients},
					"deliveredAt":    map[string]interface{}{"type": "string", "format": "date-time"},
					"artifactRefs":   map[string]interface{}{"type": "array", "maxItems": MaximumArtifactRefs, "items": artifactReference},
				},
			},
			SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal,
			Permissions: []string{"communications:email:send"},
			Credentials: []skill.CredentialRequirement{{Name: EmailCredentialName, Kind: EmailCredentialKind}},
			Timeout:     capability.Duration(60 * time.Second), Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
			Idempotency: skill.IdempotencyRequired, EmittedEventTypes: []string{"delivery.email.accepted"},
		}},
		Prompt: &capability.PromptModule{
			Instructions:  "Use send_email only for authorized recipients and reviewed outbound communication. Preserve identity and affiliation policy, attach only durable artifact references, and never claim delivery before the governed action returns an accepted receipt.",
			UserInvocable: true, AllowedTools: []string{SendEmail},
		},
		Requirements: capability.Requirements{AlwaysAvailable: true},
		Installers: []capability.Installer{{
			ID: "oci", Kind: "oci", Label: "Governed delivery service image", Package: SkillImage,
		}},
	}
}
