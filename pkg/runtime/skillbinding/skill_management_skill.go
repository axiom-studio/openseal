package skillbinding

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
	inferschema "github.com/invopop/jsonschema"
)

// SkillActionTargetAgentArgument names the Agent a builder acts for in
// discover, upsert_binding, disable_binding, request_setup and
// request_connection. Without it the kernel derives the target from the
// durable Run (the chat's Agent or Team).
const SkillActionTargetAgentArgument = "targetAgentId"

func skillActionTargetAgentProperty() map[string]interface{} {
	return map[string]interface{}{
		"type": "string", "minLength": 1, "maxLength": 128,
		"description": "Agent deployment id to act for, when building or changing another Agent. Omit for this chat's Agent or Team.",
	}
}

// SkillManagementSkill exposes the canonical Skill binding lifecycle to an
// Agent conversation. The target deployment is absent from the model-visible
// schema, except for a builder's optional targetAgentId: the kernel derives
// it from the durable Run.
func SkillManagementSkill() *skill.Definition {
	upsertProperties := map[string]interface{}{
		"bindingId":        map[string]interface{}{"type": "string", "minLength": 1},
		"expectedRevision": map[string]interface{}{"type": "integer", "minimum": 0},
		"skillId":          map[string]interface{}{"type": "string", "minLength": 1},
		"skillVersion":     map[string]interface{}{"type": "string", "minLength": 1},
		"sourceIdentity":   map[string]interface{}{"type": "string"},
		"allowedActions": map[string]interface{}{
			"type":        "array",
			"description": "Exact action names from the selected Skill definition. Use an empty array for prompt-only access or account-only connection storage. Wildcards such as * are invalid and never grant authority.",
			"items":       map[string]interface{}{"type": "string", "minLength": 1, "pattern": `^[^*]+$`},
			"uniqueItems": true,
		},
		"enablePrompt": map[string]interface{}{"type": "boolean"},
		"enabledConversationAdapters": map[string]interface{}{
			"type": "array", "description": "Exact provider adapter ids from the selected Skill definition.",
			"items":       map[string]interface{}{"type": "string", "minLength": 1, "pattern": `^[^*]+$`},
			"uniqueItems": true,
		},
		"maximumRisk": map[string]interface{}{"type": "string", "enum": []interface{}{
			string(skill.RiskLevelRead), string(skill.RiskLevelWrite), string(skill.RiskLevelExternal), string(skill.RiskLevelProduction), string(skill.RiskLevelDestructive),
		}},
		"argumentRestrictions":         map[string]interface{}{"type": "object"},
		"config":                       map[string]interface{}{"type": "object"},
		SkillActionTargetAgentArgument: skillActionTargetAgentProperty(),
	}
	return &skill.Definition{
		ID: models.SkillManagementSkillID, Version: SkillManagementSkillVersion,
		Name: "Skills", Description: "Propose governed changes to the Skill access of this chat's Agent or Team.",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: SkillManagementEndpoint},
		Actions: map[string]skill.Action{
			SkillActionDiscoverBinding:            skillDiscoveryAction(),
			agentrun.SkillActionRequestSetup:      skillSetupAction(),
			agentrun.SkillActionRequestCredential: credentialRequestAction(),
			SkillActionListSetupRequests:          skillListSetupRequestsAction(),
			SkillActionFindApps:                   findAppsAction(),
			agentrun.SkillActionRequestConnection: requestConnectionAction(),
			SkillActionUpsertBinding: skillManagementAction(
				SkillActionUpsertBinding,
				"Propose enabling or updating configuration on an exact registered Skill for this chat's Agent or Team. An existing binding retains its exact Skill ID, source and version and its saved account; use the canonical Skill reference upgrade flow before changing executable versions. This action never adds, changes or removes an account: when a Skill needs one, or a person must authorize it, call request_setup instead. With targetAgentId, the change applies to that Agent.",
				upsertProperties,
				[]interface{}{"bindingId", "expectedRevision", "skillId", "skillVersion", "allowedActions", "enablePrompt", "maximumRisk"},
			),
			SkillActionDisableBinding: skillManagementAction(
				SkillActionDisableBinding,
				"Propose disabling one current Skill binding of this chat's Agent or Team using its current revision. With targetAgentId, the change applies to that Agent.",
				map[string]interface{}{
					"bindingId":                    map[string]interface{}{"type": "string", "minLength": 1},
					"expectedRevision":             map[string]interface{}{"type": "integer", "minimum": 1},
					SkillActionTargetAgentArgument: skillActionTargetAgentProperty(),
				},
				[]interface{}{"bindingId", "expectedRevision"},
			),
		},
	}
}

func skillDiscoveryAction() skill.Action {
	riskValues := []interface{}{string(skill.RiskLevelRead), string(skill.RiskLevelWrite), string(skill.RiskLevelExternal), string(skill.RiskLevelProduction), string(skill.RiskLevelDestructive)}
	oauth2Schema := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"provider": map[string]interface{}{"type": "string"},
			"subject":  map[string]interface{}{"type": "string", "enum": []interface{}{string(skill.OAuth2SubjectInstallation), string(skill.OAuth2SubjectUser)}},
			"scopes":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "uniqueItems": true},
			"resource": map[string]interface{}{"type": "string"},
		},
		"required": []interface{}{"provider", "subject"},
	}
	actionSchema := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"name": map[string]interface{}{"type": "string"}, "description": map[string]interface{}{"type": "string"},
			"risk": map[string]interface{}{"type": "string", "enum": riskValues},
		},
		"required": []interface{}{"name", "risk"},
	}
	credentialSchema := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"name": map[string]interface{}{"type": "string"}, "kind": map[string]interface{}{"type": "string"},
			"optional": map[string]interface{}{"type": "boolean"}, "configured": map[string]interface{}{"type": "boolean"},
			"oauth2": oauth2Schema,
		},
		"required": []interface{}{"name", "kind", "configured"},
	}
	conversationDeliverySchema := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"operations": map[string]interface{}{
				"type": "array", "uniqueItems": true, "items": map[string]interface{}{"type": "string", "enum": []interface{}{
					string(skill.ConversationDeliveryMessageSend), string(skill.ConversationDeliveryMessageUpdate),
					string(skill.ConversationDeliveryMessageDelete), string(skill.ConversationDeliveryReactionAdd),
					string(skill.ConversationDeliveryReactionRemove), string(skill.ConversationDeliveryTypingIndicator),
				}},
			},
			"ordering": map[string]interface{}{"type": "string", "enum": []interface{}{
				string(skill.ConversationDeliveryOrderEndpoint), string(skill.ConversationDeliveryOrderConversation), string(skill.ConversationDeliveryOrderThread),
			}},
			"idempotency":                   map[string]interface{}{"type": "string", "enum": []interface{}{string(skill.IdempotencySupported), string(skill.IdempotencyRequired)}},
			"supportsAcknowledgementLookup": map[string]interface{}{"type": "boolean"},
			"supportsRetryAfter":            map[string]interface{}{"type": "boolean"},
		},
		"required": []interface{}{"operations", "ordering", "idempotency"},
	}
	conversationAdapterSchema := derivedDiscoveryConversationAdapterSchema(credentialSchema, conversationDeliverySchema)
	compatibilitySchema := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"requirement": map[string]interface{}{"type": "string"}, "compatible": map[string]interface{}{"type": "boolean"},
			"evidence": map[string]interface{}{"type": "string"}, "reference": map[string]interface{}{"type": "string"},
		},
		"required": []interface{}{"requirement", "compatible", "evidence"},
	}
	candidateSchema := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"id": map[string]interface{}{"type": "string"}, "version": map[string]interface{}{"type": "string"},
			"sourceIdentity": map[string]interface{}{"type": "string"}, "name": map[string]interface{}{"type": "string"},
			"description": map[string]interface{}{"type": "string"}, "actions": map[string]interface{}{"type": "array", "items": actionSchema},
			"credentials": map[string]interface{}{"type": "array", "items": credentialSchema}, "promptAvailable": map[string]interface{}{"type": "boolean"},
			"conversationAdapters": map[string]interface{}{"type": "array", "items": conversationAdapterSchema},
			"bindingConfigSchema":  map[string]interface{}{"type": "object"},
			"maximumRisk":          map[string]interface{}{"type": "string", "enum": riskValues},
			"readiness":            map[string]interface{}{"type": "string", "enum": []interface{}{string(skill.DiscoveryReadinessBindable), string(skill.DiscoveryReadinessNeedsInstallation), string(skill.DiscoveryReadinessUnavailable)}},
			"compatibility":        map[string]interface{}{"type": "array", "items": compatibilitySchema},
		},
		"required": []interface{}{"id", "version", "name", "readiness"},
	}
	return skill.Action{
		Name:        SkillActionDiscoverBinding,
		Description: "Find exact authorized Skills that could satisfy a capability request for the current Agent. Query with the requested service or Skill name or concise task keywords. Omit requiredActions on the initial lookup. After discovery, requiredActions may contain only exact actions[].name values copied from a result; never natural-language task descriptions. If a requiredActions filter fails, retry once without it to inspect the real names before reporting an access blocker. Omit maximumRisk for a connection request unless the user explicitly requested a catalog risk filter: read-only discovery does not execute the Skill, and an integration can include both read and destructive actions. Results contain no credential references or values and do not install or activate anything. With targetAgentId, readiness is reported for that Agent.",
		Risk:        skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported,
		Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"query":                        map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 512},
				"requiredActions":              map[string]interface{}{"description": "Optional exact actions[].name values copied from a previous discovery result. Omit on the first lookup or when the exact names are unknown. Put natural-language tasks in query instead.", "type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128}, "uniqueItems": true, "maxItems": 32},
				"maximumRisk":                  map[string]interface{}{"description": "Optional catalog risk filter. With requiredActions, checks the highest risk among those exact operations; otherwise checks the whole Skill. Candidate actions and maximumRisk still describe the full Skill. Omit for ordinary connection discovery; execution risk is enforced separately.", "type": "string", "enum": riskValues},
				"cursor":                       map[string]interface{}{"type": "string", "maxLength": 1024},
				"limit":                        map[string]interface{}{"type": "integer", "minimum": 1, "maximum": skill.MaximumDiscoveryLimit},
				SkillActionTargetAgentArgument: skillActionTargetAgentProperty(),
			},
			"required": []interface{}{"query"},
		},
		OutputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"items":      map[string]interface{}{"type": "array", "items": candidateSchema},
				"nextCursor": map[string]interface{}{"type": "string"},
			},
			"required": []interface{}{"items"},
		},
	}
}

// derivedDiscoveryConversationAdapterSchema derives field presence and
// requiredness from the serialized discovery contract. Only semantic rules
// that Go reflection cannot express are overlaid below.
func derivedDiscoveryConversationAdapterSchema(credentialSchema, deliverySchema map[string]interface{}) map[string]interface{} {
	endpointModeType := reflect.TypeOf(skill.ConversationEndpointMode(""))
	inferred := (&inferschema.Reflector{
		Anonymous: true, ExpandedStruct: true, DoNotReference: true,
		Mapper: func(value reflect.Type) *inferschema.Schema {
			if value == endpointModeType {
				return &inferschema.Schema{Type: "string", Enum: []interface{}{
					string(skill.ConversationEndpointChannel), string(skill.ConversationEndpointDirect),
				}}
			}
			return nil
		},
	}).Reflect(skill.DiscoveryConversationAdapter{})
	encoded, err := json.Marshal(inferred)
	if err != nil {
		panic(fmt.Sprintf("derive Skill discovery conversation adapter schema: %v", err))
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(encoded, &schema); err != nil {
		panic(fmt.Sprintf("decode Skill discovery conversation adapter schema: %v", err))
	}
	delete(schema, "$schema")
	delete(schema, "$id")
	delete(schema, "$defs")
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		panic("derived Skill discovery conversation adapter schema has no properties")
	}
	properties["protocolVersion"] = map[string]interface{}{"type": "string", "const": skill.ConversationAdapterProtocolV1}
	properties["delivery"] = deliverySchema
	properties["credentials"] = map[string]interface{}{"type": "array", "items": credentialSchema}
	for _, name := range []string{"endpointModes", "inboundEventTypes", "features", "discoverableDestinationModes"} {
		if property, exists := properties[name].(map[string]interface{}); exists {
			property["uniqueItems"] = true
		}
	}
	return schema
}

func skillManagementAction(name, description string, properties map[string]interface{}, required []interface{}) skill.Action {
	return skill.Action{
		Name: name, Description: description, Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
		Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": properties, "required": required},
		OutputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"resourceType": map[string]interface{}{"type": "string", "const": "skill_binding"},
				"operation":    map[string]interface{}{"type": "string", "enum": []interface{}{SkillActionUpsertBinding, SkillActionDisableBinding}},
				"replayed":     map[string]interface{}{"type": "boolean"},
				"binding":      map[string]interface{}{"type": "object"},
			},
			"required": []interface{}{"resourceType", "operation", "replayed", "binding"},
		},
	}
}
