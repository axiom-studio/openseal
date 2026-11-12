package models

import (
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	AgentManagementSkillID        = "openseal.agents"
	AgentManagementSkillVersion   = "1.6.0"
	agentManagementSkillVersionV8 = "1.5.0" // still bound until the 1.6.0 upgrade is approved
	agentManagementSkillVersionV7 = "1.4.0" // still bound until the reconcile upgrades bindings
	agentManagementSkillVersionV6 = "1.3.2"
	agentManagementSkillVersionV5 = "1.3.1"
	agentManagementSkillVersionV4 = "1.3.0"
	agentManagementSkillVersionV3 = "1.2.2"
	agentManagementSkillVersionV1 = "1.2.0"
	agentManagementSkillVersionV2 = "1.2.1"
	AgentActionAmendBehavior      = "amend_behavior"
	AgentActionListChannels       = "list_channels"
	AgentActionConfigureChannel   = "configure_channel"
	AgentActionListAgents         = "list_agents"
	AgentActionCreateAgent        = "create_agent"
	AgentActionUpdateAgent        = "update_agent"
	AgentActionTryAgent           = "try_agent"
	AgentProfileResourceType      = "agent_profile"
	AgentManagementEndpoint       = "kernel://agents"
	AgentBehaviorResourceType     = "agent_definition"
)

// AgentManagementSkill exposes bounded, self-only behavior amendments to an
// Agent conversation. The target deployment, active definition, and current
// revision are kernel-owned facts and are never model-selected inputs.
func AgentManagementSkill() *skill.Definition {
	return &skill.Definition{
		ID: AgentManagementSkillID, Version: AgentManagementSkillVersion,
		Name: "Agents", Description: "Change the current Agent's behavior, and build new Agents in chat with review.",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: AgentManagementEndpoint},
		Actions: map[string]skill.Action{
			AgentActionListChannels: {
				Name: AgentActionListChannels, Description: "List the current Agent's workflow channels, including their reply behavior, approval purpose, and materialized endpoint health.",
				Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectNone,
				Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
				InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false},
				OutputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"resourceType": map[string]interface{}{"type": "string", "const": "agent_channels"},
						"channels":     map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}},
					},
					"required": []interface{}{"resourceType", "channels"},
				},
			},
			AgentActionAmendBehavior: {
				Name:        AgentActionAmendBehavior,
				Description: "Propose changing the current Agent's display name, purpose, system prompt, personality, operating principles, or approval-timeout policy. Supply at least one changed field and preserve unrelated current behavior. String and list values replace the corresponding field in full. Automatic approval is opt-in and requires an explicit timeout duration and approve decision. The kernel resolves the target and revision, then enforces amendment policy and the immutable activation lifecycle.",
				Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Review: skill.ActionReviewAlways,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
				InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"expectedDeploymentRevision": map[string]interface{}{
							"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true,
						},
						"displayName":         map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 200},
						"purpose":             map[string]interface{}{"type": "string", "minLength": 1},
						"systemPrompt":        map[string]interface{}{"type": "string", "minLength": 1},
						"personality":         map[string]interface{}{"type": "string"},
						"operatingPrinciples": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1}, "uniqueItems": true},
						"approvalTimeout": map[string]interface{}{
							"type": "object", "additionalProperties": false,
							"properties": map[string]interface{}{
								"afterSeconds": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 2592000},
								"decision":     map[string]interface{}{"type": "string", "enum": []interface{}{"expire", "approve"}},
							},
							"required": []interface{}{"afterSeconds", "decision"},
						},
						"removeApprovalTimeout": map[string]interface{}{"type": "boolean", "const": true},
						"rationale":             map[string]interface{}{"type": "string", "minLength": 1},
					},
					"required": []interface{}{"expectedDeploymentRevision", "rationale"},
					"anyOf": []interface{}{
						map[string]interface{}{"required": []interface{}{"displayName"}},
						map[string]interface{}{"required": []interface{}{"purpose"}},
						map[string]interface{}{"required": []interface{}{"systemPrompt"}},
						map[string]interface{}{"required": []interface{}{"personality"}},
						map[string]interface{}{"required": []interface{}{"operatingPrinciples"}},
						map[string]interface{}{"required": []interface{}{"approvalTimeout"}},
						map[string]interface{}{"required": []interface{}{"removeApprovalTimeout"}},
					},
				},
				OutputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"resourceType": map[string]interface{}{"type": "string", "const": AgentBehaviorResourceType},
						"operation":    map[string]interface{}{"type": "string", "const": AgentActionAmendBehavior},
						"replayed":     map[string]interface{}{"type": "boolean"},
						"amendment":    map[string]interface{}{"type": "object"},
						"deployment":   map[string]interface{}{"type": "object"},
						"activation":   map[string]interface{}{"type": "object"},
					},
					"required": []interface{}{"resourceType", "operation", "replayed", "amendment", "deployment", "activation"},
				},
			},
			AgentActionConfigureChannel: {
				Name:        AgentActionConfigureChannel,
				Description: "Propose changing how an existing active or paused authorized channel enters and leaves the current Agent's workflow. Retired channels are historical and cannot be configured or reactivated. This operation governs message selection, reply mode, and approval-delivery purpose together.",
				Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Review: skill.ActionReviewAlways,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
				InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"expectedDeploymentRevision": map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true},
						"expectedEndpointRevision":   map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true},
						"endpointId":                 map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256},
						"messageSelection":           map[string]interface{}{"type": "string", "enum": []interface{}{"all_messages", "mentions", "direct_or_mentions"}},
						"replyMode":                  map[string]interface{}{"type": "string", "enum": []interface{}{"provider_default", "thread", "channel"}},
						"ignoreBots":                 map[string]interface{}{"type": "boolean"},
						"purposes":                   map[string]interface{}{"type": "array", "minItems": 1, "uniqueItems": true, "items": map[string]interface{}{"type": "string", "enum": []interface{}{"conversation", "approvals"}}},
						"status":                     map[string]interface{}{"type": "string", "enum": []interface{}{"active", "paused"}},
						"rationale":                  map[string]interface{}{"type": "string", "minLength": 1},
					},
					"required": []interface{}{"expectedDeploymentRevision", "expectedEndpointRevision", "endpointId", "rationale"},
				},
				OutputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"resourceType": map[string]interface{}{"type": "string", "const": AgentBehaviorResourceType},
						"operation":    map[string]interface{}{"type": "string", "const": AgentActionConfigureChannel},
						"replayed":     map[string]interface{}{"type": "boolean"},
						"amendment":    map[string]interface{}{"type": "object"},
						"deployment":   map[string]interface{}{"type": "object"},
						"activation":   map[string]interface{}{"type": "object"},
						"endpoint":     map[string]interface{}{"type": "object"},
					},
					"required": []interface{}{"resourceType", "operation", "replayed", "amendment", "deployment", "activation", "endpoint"},
				},
			},
			AgentActionListAgents:  agentListAgentsAction(),
			AgentActionCreateAgent: agentCreateAgentAction(),
			AgentActionUpdateAgent: agentUpdateAgentAction(),
			AgentActionTryAgent:    agentTryAgentAction(),
		},
	}
}

func TrimmedUniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func IsAgentManagementAction(bound *skill.BoundAction) bool {
	return bound != nil && bound.Definition != nil && bound.Definition.ID == AgentManagementSkillID &&
		IsSupportedAgentManagementSkillVersion(bound.Definition.Version) &&
		(bound.Action.Name == AgentActionAmendBehavior || bound.Action.Name == AgentActionListChannels || bound.Action.Name == AgentActionConfigureChannel)
}

// CanonicalActionReview applies the current kernel contract's review floor to
// an Agent management action bound at any supported version. Published
// versions are immutable, so bindings pinned before review: always was
// declared (1.2.0 through 1.3.1) still inherit it from AgentManagementSkill().
func CanonicalActionReview(definition *skill.Definition, action skill.Action) skill.Action {
	if !IsAgentManagementAction(&skill.BoundAction{Definition: definition, Action: action}) {
		return action
	}
	if canonical, ok := AgentManagementSkill().Actions[action.Name]; ok && canonical.AlwaysReview() {
		action.Review = canonical.Review
	}
	return action
}

// IsSupportedAgentManagementSkillVersion reports the agent management contracts
// the Agent action dispatcher still executes. Hosts use it instead of keeping
// their own copy of the version list.
func IsSupportedAgentManagementSkillVersion(version string) bool {
	return version == AgentManagementSkillVersion || version == agentManagementSkillVersionV8 || version == agentManagementSkillVersionV7 || version == agentManagementSkillVersionV6 || version == agentManagementSkillVersionV5 || version == agentManagementSkillVersionV4 || version == agentManagementSkillVersionV3 || version == agentManagementSkillVersionV2 || version == agentManagementSkillVersionV1
}
