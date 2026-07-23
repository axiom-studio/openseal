// Package outreach defines portable governed follow-up capabilities. Provider
// credentials and source-policy decisions remain host concerns and never enter
// model-visible Skill inputs.
package outreach

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	SkillID      = "openseal.outreach"
	SkillVersion = "1.0.0"
	SkillImage   = "axiomstudio/skill-openseal-outreach:1.0.0"
	PostReply    = "post_reply"

	PolicyDecisionTransportKey = "_opensealOutreachPolicyDecision"
	ApprovalPolicyTransportKey = "_opensealOutreachApprovalPolicy"
	ActionCallIDTransportKey   = "_opensealOutreachActionCallId"
	RunIDTransportKey          = "_opensealOutreachRunId"
	DeploymentIDTransportKey   = "_opensealOutreachDeploymentId"
)

func SkillDefinition() *skill.Definition {
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Governed source outreach",
		Description: "Post a reviewed, identity-disclosed reply to an evidence-linked webhook-compatible source.",
		Transport:   skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		Actions: map[string]skill.Action{PostReply: {
			Name: PostReply, Description: "Post one reviewed follow-up to the exact evidence source URL.",
			SemanticArguments: map[string]string{"target": "targetUri", "body": "body"},
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"required": []interface{}{"targetUri", "body"},
				"properties": map[string]interface{}{
					"targetUri": map[string]interface{}{"type": "string", "format": "uri", "pattern": "^https://", "maxLength": 2000},
					"body":      map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 20000},
				},
			},
			OutputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false, "required": []interface{}{"outreachReceipt"},
				"properties": map[string]interface{}{"outreachReceipt": map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"required": []interface{}{"provider", "externalId", "externalUri", "deliveredAt", "digest"},
					"properties": map[string]interface{}{
						"provider":    map[string]interface{}{"type": "string", "const": "http-webhook"},
						"externalId":  map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 512},
						"externalUri": map[string]interface{}{"type": "string", "format": "uri", "pattern": "^https://", "maxLength": 2000},
						"deliveredAt": map[string]interface{}{"type": "string", "format": "date-time"},
						"digest":      map[string]interface{}{"type": "string", "pattern": "^sha256:[a-f0-9]{64}$"},
					},
				}},
			},
			SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal,
			Permissions: []string{"network:https:write", "communications:source:reply"},
			Timeout:     capability.Duration(30 * time.Second), Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
			Idempotency: skill.IdempotencyRequired, EmittedEventTypes: []string{"outreach.reply.accepted"},
		}},
		Prompt: &capability.PromptModule{
			Instructions:  "Use post_reply only inside a governed OutreachThread after source policy and approval are satisfied. Preserve the reviewed body, truthful identity disclosure, and exact evidence URL.",
			UserInvocable: true, AllowedTools: []string{PostReply},
		},
		Requirements: capability.Requirements{AlwaysAvailable: true},
		Installers: []capability.Installer{{
			ID: "oci", Kind: "oci", Label: "Governed outreach service image", Package: SkillImage,
		}},
	}
}
