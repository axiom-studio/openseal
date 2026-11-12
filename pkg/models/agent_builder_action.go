package models

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/validation"
)

// Bounds of a drafted Agent profile. They are the limits the change-set
// drafts enforced, now in the action schema and validateAgentProfileText.
const (
	MaxAgentDisplayNameRunes      = 80
	maxAgentPurposeRunes          = 2000
	maxAgentProfileTextRunes      = 16384
	maxAgentOperatingPrinciples   = 16
	maxAgentOperatingPrincipleLen = 2048
	maxAgentModelIDLength         = 128
	MaxAgentBuilderSkillGrants    = 16
	MaxAgentBuilderConnections    = 8
	MaxAgentTrialMessageRunes     = 4096
	maxAgentListQueryRunes        = 200
)

// AgentSkillGrant is one Skill binding a builder action gives the new or
// changed Agent. It mirrors upsert_binding without the binding id or revision:
// the kernel derives both.
type AgentSkillGrant struct {
	SkillID        string                 `json:"skillId"`
	SkillVersion   string                 `json:"skillVersion"`
	SourceIdentity string                 `json:"sourceIdentity,omitempty"`
	AllowedActions []string               `json:"allowedActions"`
	EnablePrompt   bool                   `json:"enablePrompt"`
	MaximumRisk    skill.RiskLevel        `json:"maximumRisk"`
	Config         map[string]interface{} `json:"config,omitempty"`
}

// AgentConnectionRequest names a Skill that needs a saved account. The new
// Agent connects it in its own chat after creation.
type AgentConnectionRequest struct {
	SkillID        string `json:"skillId"`
	SkillVersion   string `json:"skillVersion"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
}

type AgentProfileArguments struct {
	DisplayName         string   `json:"displayName"`
	Purpose             string   `json:"purpose"`
	SystemPrompt        string   `json:"systemPrompt"`
	Personality         string   `json:"personality,omitempty"`
	OperatingPrinciples []string `json:"operatingPrinciples,omitempty"`
	Model               string   `json:"model,omitempty"`
}

type AgentCreateArguments struct {
	AgentProfileArguments
	SpaceID     string                   `json:"spaceId"`
	Skills      []AgentSkillGrant        `json:"skills,omitempty"`
	Connections []AgentConnectionRequest `json:"connections,omitempty"`
	Rationale   string                   `json:"rationale"`
}

type AgentUpdateArguments struct {
	AgentID                    string            `json:"agentId"`
	ExpectedDeploymentRevision int64             `json:"expectedDeploymentRevision"`
	DisplayName                *string           `json:"displayName,omitempty"`
	Purpose                    *string           `json:"purpose,omitempty"`
	SystemPrompt               *string           `json:"systemPrompt,omitempty"`
	Personality                *string           `json:"personality,omitempty"`
	OperatingPrinciples        *[]string         `json:"operatingPrinciples,omitempty"`
	Model                      *string           `json:"model,omitempty"`
	AddSkills                  []AgentSkillGrant `json:"addSkills,omitempty"`
	RemoveSkillBindingIDs      []string          `json:"removeSkillBindingIds,omitempty"`
	Rationale                  string            `json:"rationale"`
}

type AgentTryArguments struct {
	AgentProfileArguments
	Message string `json:"message"`
}

type AgentListArguments struct {
	Query string `json:"query,omitempty"`
}

// agentProfileProperties is the profile fragment shared by create_agent,
// update_agent and try_agent.
func agentProfileProperties() map[string]interface{} {
	return map[string]interface{}{
		"displayName":  map[string]interface{}{"type": "string", "minLength": 1, "maxLength": MaxAgentDisplayNameRunes},
		"purpose":      map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maxAgentPurposeRunes},
		"systemPrompt": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maxAgentProfileTextRunes},
		"personality":  map[string]interface{}{"type": "string", "maxLength": maxAgentProfileTextRunes},
		"operatingPrinciples": map[string]interface{}{
			"type": "array", "maxItems": maxAgentOperatingPrinciples, "uniqueItems": true,
			"items": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maxAgentOperatingPrincipleLen},
		},
		"model": map[string]interface{}{"type": "string", "maxLength": maxAgentModelIDLength, "description": "Gateway model id; omit for the default."},
	}
}

func agentSkillGrantSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"required": []interface{}{"skillId", "skillVersion", "allowedActions", "enablePrompt", "maximumRisk"},
		"properties": map[string]interface{}{
			"skillId":        map[string]interface{}{"type": "string", "minLength": 1},
			"skillVersion":   map[string]interface{}{"type": "string", "minLength": 1},
			"sourceIdentity": map[string]interface{}{"type": "string"},
			"allowedActions": map[string]interface{}{
				"type": "array", "uniqueItems": true,
				"items": map[string]interface{}{"type": "string", "minLength": 1, "pattern": "^[^*]+$"},
			},
			"enablePrompt": map[string]interface{}{"type": "boolean"},
			"maximumRisk":  map[string]interface{}{"type": "string", "enum": []interface{}{"read", "write", "external", "production", "destructive"}},
			"config":       map[string]interface{}{"type": "object"},
		},
	}
}

func agentConnectionRequestSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"required": []interface{}{"skillId", "skillVersion"},
		"properties": map[string]interface{}{
			"skillId":        map[string]interface{}{"type": "string", "minLength": 1},
			"skillVersion":   map[string]interface{}{"type": "string", "minLength": 1},
			"sourceIdentity": map[string]interface{}{"type": "string"},
		},
	}
}

func agentProfileOutputSchema(operation string) map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"resourceType": map[string]interface{}{"type": "string", "const": AgentProfileResourceType},
			"operation":    map[string]interface{}{"type": "string", "const": operation},
			"deploymentId": map[string]interface{}{"type": "string"},
			"displayName":  map[string]interface{}{"type": "string"},
			"replayed":     map[string]interface{}{"type": "boolean"},
		},
		"required": []interface{}{"resourceType", "operation", "deploymentId", "displayName", "replayed"},
	}
}

func agentListAgentsAction() skill.Action {
	return skill.Action{
		Name: AgentActionListAgents, Description: "List this workspace's Agents with their ids.",
		Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectNone,
		Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"query": map[string]interface{}{"type": "string", "maxLength": maxAgentListQueryRunes},
			},
		},
		OutputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"resourceType": map[string]interface{}{"type": "string", "const": "agent_list"},
				"agents":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}},
			},
			"required": []interface{}{"resourceType", "agents"},
		},
	}
}

func agentCreateAgentAction() skill.Action {
	properties := agentProfileProperties()
	properties["spaceId"] = map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128, skill.SchemaExtensionKernelResolved: true}
	properties["skills"] = map[string]interface{}{"type": "array", "maxItems": MaxAgentBuilderSkillGrants, "items": agentSkillGrantSchema()}
	properties["connections"] = map[string]interface{}{"type": "array", "maxItems": MaxAgentBuilderConnections, "items": agentConnectionRequestSchema()}
	properties["rationale"] = map[string]interface{}{"type": "string", "minLength": 1}
	return skill.Action{
		Name:        AgentActionCreateAgent,
		Description: "Create a new Agent from a drafted profile, after the person reviews it. Propose it as soon as the role is clear; each later message from the person refines the draft, so propose a revised create_agent. Keep per-task details (accounts, destinations, schedules, inputs) out of the profile; they are set up in the new Agent's chat. Choose a short, distinctive name that no current Agent uses. Bind only Skills from openseal.skills.discover whose selected actions need no saved account; list Skills that need an account in connections.",
		Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Review: skill.ActionReviewAlways,
		Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "properties": properties,
			"required": []interface{}{"displayName", "purpose", "systemPrompt", "spaceId", "rationale"},
		},
		OutputSchema: agentProfileOutputSchema(AgentActionCreateAgent),
	}
}

func agentUpdateAgentAction() skill.Action {
	properties := agentProfileProperties()
	properties["agentId"] = map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128}
	properties["expectedDeploymentRevision"] = map[string]interface{}{"type": "integer", "minimum": 1, skill.SchemaExtensionKernelResolved: true}
	properties["addSkills"] = map[string]interface{}{"type": "array", "maxItems": MaxAgentBuilderSkillGrants, "items": agentSkillGrantSchema()}
	properties["removeSkillBindingIds"] = map[string]interface{}{
		"type": "array", "maxItems": MaxAgentBuilderSkillGrants, "uniqueItems": true,
		"items": map[string]interface{}{"type": "string", "minLength": 1},
	}
	properties["rationale"] = map[string]interface{}{"type": "string", "minLength": 1}
	anyOf := make([]interface{}, 0, 8)
	for _, field := range []string{"displayName", "purpose", "systemPrompt", "personality", "operatingPrinciples", "model", "addSkills", "removeSkillBindingIds"} {
		anyOf = append(anyOf, map[string]interface{}{"required": []interface{}{field}})
	}
	return skill.Action{
		Name:        AgentActionUpdateAgent,
		Description: "Change another Agent's profile, model or Skills, after the person reviews it. Use list_agents for the exact agentId. String and list values replace the field in full.",
		Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Review: skill.ActionReviewAlways,
		Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "properties": properties,
			"required": []interface{}{"agentId", "expectedDeploymentRevision", "rationale"},
			"anyOf":    anyOf,
		},
		OutputSchema: agentProfileOutputSchema(AgentActionUpdateAgent),
	}
}

func agentTryAgentAction() skill.Action {
	properties := agentProfileProperties()
	properties["message"] = map[string]interface{}{"type": "string", "minLength": 1, "maxLength": MaxAgentTrialMessageRunes}
	return skill.Action{
		Name:        AgentActionTryAgent,
		Description: "Send one test message to a drafted profile and read its reply. Nothing is created and the test Agent has no tools.",
		Risk:        skill.RiskLevelRead, SideEffect: skill.SideEffectNone,
		Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "properties": properties,
			"required": []interface{}{"displayName", "purpose", "systemPrompt", "message"},
		},
		OutputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"resourceType": map[string]interface{}{"type": "string", "const": "agent_trial"},
				"trialRunId":   map[string]interface{}{"type": "string"},
				"status":       map[string]interface{}{"type": "string"},
			},
			"required": []interface{}{"resourceType", "trialRunId", "status"},
		},
	}
}

// NormalizedAgentName is the key the name check compares: case-folded, with
// runs of whitespace collapsed to one space.
func NormalizedAgentName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// Normalized returns the profile with trimmed text and deduplicated
// principles, the form a definition stores.
func (p AgentProfileArguments) Normalized() AgentProfileArguments {
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	p.Purpose = strings.TrimSpace(p.Purpose)
	p.SystemPrompt = strings.TrimSpace(p.SystemPrompt)
	p.Personality = strings.TrimSpace(p.Personality)
	p.OperatingPrinciples = TrimmedUniqueStrings(p.OperatingPrinciples)
	p.Model = strings.TrimSpace(p.Model)
	return p
}

// ValidateAgentProfileText enforces the profile bounds and refuses secrets in
// any profile text. profile must already be normalized.
func ValidateAgentProfileText(profile AgentProfileArguments) error {
	return ValidateAgentProfileFields(profile, nil)
}

// ValidateAgentProfileFields checks the named profile fields, or every field
// when fields is nil.
func ValidateAgentProfileFields(profile AgentProfileArguments, fields map[string]bool) error {
	checked := func(name string) bool { return fields == nil || fields[name] }
	if checked("displayName") {
		if err := ValidateAgentDisplayName(profile.DisplayName); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name, value string
		maximum     int
		required    bool
	}{
		{"purpose", profile.Purpose, maxAgentPurposeRunes, true},
		{"systemPrompt", profile.SystemPrompt, maxAgentProfileTextRunes, true},
		{"personality", profile.Personality, maxAgentProfileTextRunes, false},
	} {
		if !checked(field.name) {
			continue
		}
		if field.required && field.value == "" {
			return fmt.Errorf("agent %s is required", field.name)
		}
		if utf8.RuneCountInString(field.value) > field.maximum {
			return fmt.Errorf("agent %s must be at most %d characters", field.name, field.maximum)
		}
	}
	if checked("operatingPrinciples") {
		if len(profile.OperatingPrinciples) > maxAgentOperatingPrinciples {
			return fmt.Errorf("an agent can have at most %d operating principles", maxAgentOperatingPrinciples)
		}
		for _, principle := range profile.OperatingPrinciples {
			if utf8.RuneCountInString(principle) > maxAgentOperatingPrincipleLen {
				return fmt.Errorf("each operating principle must be at most %d characters", maxAgentOperatingPrincipleLen)
			}
		}
	}
	if checked("model") && len(profile.Model) > maxAgentModelIDLength {
		return fmt.Errorf("agent model must be at most %d characters", maxAgentModelIDLength)
	}
	values := []string{}
	for name, value := range map[string]string{
		"displayName": profile.DisplayName, "purpose": profile.Purpose,
		"systemPrompt": profile.SystemPrompt, "personality": profile.Personality,
	} {
		if checked(name) {
			values = append(values, value)
		}
	}
	if checked("operatingPrinciples") {
		values = append(values, profile.OperatingPrinciples...)
	}
	for _, value := range values {
		if err := validation.ValidateNoSensitiveInput(value); err != nil {
			return err
		}
	}
	return nil
}

func ValidateAgentDisplayName(name string) error {
	if name == "" || utf8.RuneCountInString(name) > MaxAgentDisplayNameRunes || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("agent name must contain 1 to %d characters without control characters", MaxAgentDisplayNameRunes)
	}
	return nil
}

// AgentBuilderTeamOperation is the create_team operation of an openseal.teams
// card, the only Team card under the agent-builder rule (D11).
const AgentBuilderTeamOperation = "create_team"

// IsAgentBuilderApproval reports whether approval reviews an agent-builder
// action: any agent_profile card, or a team card for create_team. Team role and
// profile amendments keep their own approval paths.
func IsAgentBuilderApproval(approval *ApprovalCheckpoint) bool {
	if approval == nil {
		return false
	}
	resourceType, _ := approval.ProposedAction["resourceType"].(string)
	operation, _ := approval.ProposedAction["operation"].(string)
	return resourceType == AgentProfileResourceType || resourceType == "team" && operation == AgentBuilderTeamOperation
}
