package skillbinding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
	inferschema "github.com/invopop/jsonschema"
)

const (
	SkillManagementSkillVersion = "1.7.0"
	SkillActionDiscoverBinding  = "discover"
	SkillActionUpsertBinding    = "upsert_binding"
	SkillActionDisableBinding   = "disable_binding"
	SkillManagementEndpoint     = "kernel://skills"
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

type SkillBindingUpsertArguments struct {
	BindingID                   string                                   `json:"bindingId"`
	ExpectedRevision            int64                                    `json:"expectedRevision"`
	SkillID                     string                                   `json:"skillId"`
	SkillVersion                string                                   `json:"skillVersion"`
	SourceIdentity              string                                   `json:"sourceIdentity,omitempty"`
	AllowedActions              []string                                 `json:"allowedActions"`
	EnablePrompt                bool                                     `json:"enablePrompt"`
	EnabledConversationAdapters []string                                 `json:"enabledConversationAdapters"`
	MaximumRisk                 skill.RiskLevel                          `json:"maximumRisk"`
	ArgumentRestrictions        map[string]map[string]skill.ArgumentRule `json:"argumentRestrictions,omitempty"`
	Config                      map[string]interface{}                   `json:"config,omitempty"`
	TargetAgentID               string                                   `json:"targetAgentId,omitempty"`
}

type skillBindingDisableArguments struct {
	BindingID        string `json:"bindingId"`
	ExpectedRevision int64  `json:"expectedRevision"`
	TargetAgentID    string `json:"targetAgentId,omitempty"`
}

type skillDiscoveryArguments struct {
	Query           string          `json:"query"`
	RequiredActions []string        `json:"requiredActions,omitempty"`
	MaximumRisk     skill.RiskLevel `json:"maximumRisk,omitempty"`
	Cursor          string          `json:"cursor,omitempty"`
	Limit           int             `json:"limit,omitempty"`
	TargetAgentID   string          `json:"targetAgentId,omitempty"`
}

// SkillBindingActionValidator resolves both deployment authority and the
// exact registered Skill before a proposal can become an approval request.
type SkillBindingActionValidator struct {
	catalog *skill.Catalog
	roster  models.TeamRosterReader
	agents  *kernelagent.Registry
}

// NewSkillBindingActionValidator checks Skill management proposals. roster
// reads Team (and, when it can, Agent) deployments to derive the run's
// umbrella, which every binding change targets.
func NewSkillBindingActionValidator(catalog *skill.Catalog, roster models.TeamRosterReader) (*SkillBindingActionValidator, error) {
	if catalog == nil {
		return nil, errors.New("skill catalog is required")
	}
	return &SkillBindingActionValidator{catalog: catalog, roster: roster}, nil
}

// UseAgentRegistry lets a builder act for another Agent (targetAgentId).
// Without it every targetAgentId is refused.
func (v *SkillBindingActionValidator) UseAgentRegistry(agents *kernelagent.Registry) {
	if v != nil {
		v.agents = agents
	}
}

func (v *SkillBindingActionValidator) ValidateActionProposal(ctx context.Context, input agentrun.ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isSkillBindingAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.catalog == nil || input.Run == nil || input.Bound.Binding == nil {
		return nil, errors.New("skill binding action validator is not configured")
	}
	member, err := skillActionMember(input.Run)
	if err != nil {
		return nil, err
	}
	// The management binding is always the acting member's own.
	if input.Bound.Binding.DeploymentID != member {
		return nil, errors.New("skill management action is not bound to the current Run's Agent deployment")
	}
	scope := skill.ScopeReference{Kind: input.Run.Scope.Kind, ID: input.Run.Scope.ID}
	preview := map[string]interface{}{
		"resourceType": "skill_binding", "operation": input.Bound.Action.Name, "deploymentId": member,
	}
	switch input.Bound.Action.Name {
	case agentrun.SkillActionRequestSetup, agentrun.SkillActionRequestCredential, agentrun.SkillActionRequestConnection:
		// Invalid interaction arguments are explained by the model from the
		// action result; they never block the proposal or end the Run.
		return nil, nil
	case SkillActionListSetupRequests, SkillActionFindApps:
		return nil, nil
	case SkillActionDiscoverBinding:
		_, err := skillActionTarget(ctx, v.catalog, v.agents, input.Run, member, input.Arguments)
		return nil, err
	case SkillActionUpsertBinding, SkillActionDisableBinding:
	default:
		return nil, errors.New("unsupported skill binding action")
	}
	owner, err := models.RunUmbrella(ctx, v.roster, input.Run)
	if err != nil {
		return nil, err
	}
	target, err := skillActionTarget(ctx, v.catalog, v.agents, input.Run, member, input.Arguments)
	if err != nil {
		return nil, err
	}
	if target != nil {
		owner = target.owner()
		preview["targetAgent"] = target.preview()
	}
	deploymentID := owner.ID
	preview["deploymentId"], preview["owner"] = deploymentID, owner
	if input.Bound.Action.Name == SkillActionUpsertBinding {
		args, definition, current, resolveErr := resolveSkillBindingUpsert(ctx, v.catalog, scope, deploymentID, input.Arguments)
		if resolveErr != nil {
			return nil, resolveErr
		}
		preview["bindingId"] = args.BindingID
		preview["expectedRevision"] = args.ExpectedRevision
		preview["skill"] = map[string]interface{}{
			"id": definition.ID, "version": definition.Version, "sourceIdentity": skill.DefinitionSourceIdentity(definition), "name": definition.Name,
		}
		preview["current"] = SecretSafeBindingPreview(current)
		preview["changes"] = SecretSafeBindingPreview(SkillBindingCandidate(scope, deploymentID, args, current))
		return preview, nil
	}
	args, current, resolveErr := resolveSkillBindingDisable(ctx, v.catalog, scope, deploymentID, input.Arguments)
	if resolveErr != nil {
		return nil, resolveErr
	}
	preview["bindingId"] = args.BindingID
	preview["expectedRevision"] = args.ExpectedRevision
	preview["current"] = SecretSafeBindingPreview(current)
	preview["changes"] = map[string]interface{}{"disabled": true}
	return preview, nil
}

// SkillBindingActionDispatcher materializes approved proposals through the
// existing CAS-protected, audited Skill binding lifecycle.
type SkillBindingActionDispatcher struct {
	store     models.KernelStore
	catalog   *skill.Catalog
	discovery skill.DiscoveryProvider
	fallback  agentrun.ActionDispatcher
	apps      *AppCatalogRef
	agents    *kernelagent.Registry
}

// UseAgentRegistry lets a builder act for another Agent (targetAgentId).
// Without it every targetAgentId is refused.
func (d *SkillBindingActionDispatcher) UseAgentRegistry(agents *kernelagent.Registry) {
	if d != nil {
		d.agents = agents
	}
}

// roster reads the Team and Agent deployments RunUmbrella checks.
func (d *SkillBindingActionDispatcher) roster() models.TeamRosterReader {
	return models.TeamRosterReaderOf(d.store)
}

func NewSkillBindingActionDispatcher(store models.KernelStore, catalog *skill.Catalog, fallback agentrun.ActionDispatcher, discovery ...skill.DiscoveryProvider) (*SkillBindingActionDispatcher, error) {
	if store == nil || catalog == nil {
		return nil, errors.New("kernel store and skill catalog are required")
	}
	if len(discovery) > 1 {
		return nil, errors.New("only one Skill discovery provider can be configured")
	}
	var provider skill.DiscoveryProvider
	if len(discovery) == 1 {
		provider = discovery[0]
		if provider == nil {
			return nil, errors.New("Skill discovery provider is required when configured")
		}
	}
	return &SkillBindingActionDispatcher{store: store, catalog: catalog, discovery: provider, fallback: fallback}, nil
}

func (d *SkillBindingActionDispatcher) DispatchAction(ctx context.Context, input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
	if !isSkillBindingAction(input.Bound) {
		if d == nil || d.fallback == nil {
			return nil, errors.New("action dispatcher does not support this action")
		}
		return d.fallback.DispatchAction(ctx, input)
	}
	if d == nil || d.store == nil || d.catalog == nil || input.Call == nil {
		return nil, errors.New("skill binding action dispatcher is not configured")
	}
	run, err := d.store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil || run == nil {
		if err == nil {
			err = models.ErrRunNotFound
		}
		return nil, err
	}
	member, err := skillActionMember(run)
	if err != nil {
		return nil, err
	}
	if input.Call.DeploymentID != member || input.Bound.Binding == nil || input.Bound.Binding.DeploymentID != member {
		return nil, errors.New("skill management action cannot target another Agent deployment")
	}
	// Every target is the run's umbrella: the Agent of a 1:1 chat, the Team
	// of a Team chat, task or hand-off. A builder in its own chat may name
	// another Agent instead (targetAgentId).
	umbrella, err := models.RunUmbrella(ctx, d.roster(), run)
	if err != nil {
		return nil, err
	}
	owner := umbrella
	var target *skillTargetAgent
	if skillActionTargetable(input.Bound.Action.Name) {
		target, err = skillActionTarget(ctx, d.catalog, d.agents, run, member, input.Arguments)
		if err != nil {
			if IsInteractionRequestAction(input.Bound.Action.Name) && ctx.Err() == nil {
				return interactionRequestRefusalResult(refuseInteraction(err.Error())), nil
			}
			return nil, err
		}
		if target != nil {
			owner = target.owner()
		}
	}
	deploymentID := owner.ID
	actor := skill.BindingActor{Type: "agent", ID: member}
	if input.Call.ApprovalID != "" {
		approval, approvalErr := approvedSkillActionCheckpoint(ctx, d.store, input.Call)
		if approvalErr != nil {
			return nil, approvalErr
		}
		actor = skill.BindingActor{Type: approval.DecisionBy.Type, ID: approval.DecisionBy.ID}
	}
	scope := skill.ScopeReference{Kind: input.Call.Scope.Kind, ID: input.Call.Scope.ID}
	switch input.Bound.Action.Name {
	case agentrun.SkillActionRequestSetup, agentrun.SkillActionRequestCredential, agentrun.SkillActionRequestConnection:
		var result map[string]interface{}
		switch input.Bound.Action.Name {
		case agentrun.SkillActionRequestSetup:
			result, err = d.requestSkillSetup(ctx, input, run, member, umbrella, owner)
		case agentrun.SkillActionRequestConnection:
			result, err = d.requestConnection(ctx, input, run, member, umbrella, owner)
		default:
			result, err = d.requestCredential(ctx, input, run, member, umbrella)
		}
		if err != nil && ctx.Err() == nil {
			return interactionRequestRefusalResult(err), nil
		}
		return result, err
	case SkillActionListSetupRequests:
		return d.listSkillSetupRequests(ctx, run, member, umbrella)
	case SkillActionFindApps:
		return d.findApps(ctx, input, run, umbrella)
	case SkillActionDiscoverBinding:
		if d.discovery == nil {
			return nil, errors.New("authorized Skill discovery is unavailable")
		}
		var args skillDiscoveryArguments
		if err := decodeSkillBindingArguments(input.Arguments, &args); err != nil {
			return nil, err
		}
		request, err := skill.NormalizeDiscoveryRequest(skill.DiscoveryRequest{
			Scope: scope, DeploymentID: deploymentID, Query: args.Query, RequiredActions: args.RequiredActions,
			MaximumRisk: args.MaximumRisk, Cursor: args.Cursor, Limit: args.Limit,
		})
		if err != nil {
			return nil, err
		}
		page, err := d.discovery.DiscoverSkills(ctx, request)
		if err != nil {
			return nil, err
		}
		normalized, err := skill.NormalizeDiscoveryPage(request, page)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return nil, fmt.Errorf("encode Skill discovery result: %w", err)
		}
		var result map[string]interface{}
		if err := json.Unmarshal(encoded, &result); err != nil {
			return nil, fmt.Errorf("decode Skill discovery result: %w", err)
		}
		return result, nil
	case SkillActionUpsertBinding:
		args, _, _, resolveErr := resolveSkillBindingUpsert(ctx, d.catalog, scope, deploymentID, input.Arguments)
		if errors.Is(resolveErr, skill.ErrBindingRevisionConflict) {
			return d.replayedUpsert(ctx, scope, deploymentID, args)
		}
		if resolveErr != nil {
			return nil, resolveErr
		}
		current, getErr := d.catalog.GetBinding(ctx, scope, deploymentID, args.BindingID)
		if getErr != nil {
			return nil, getErr
		}
		binding, upsertErr := d.catalog.UpsertBinding(ctx, skill.UpsertBindingRequest{
			Binding: SkillBindingCandidate(scope, deploymentID, args, current), ExpectedRevision: args.ExpectedRevision,
			Actor: actor, Reason: "Approved Skill access change from conversation",
		})
		if errors.Is(upsertErr, skill.ErrBindingRevisionConflict) {
			return d.replayedUpsert(ctx, scope, deploymentID, args)
		}
		return skillBindingActionResult(SkillActionUpsertBinding, binding, false), upsertErr
	case SkillActionDisableBinding:
		args, _, resolveErr := resolveSkillBindingDisable(ctx, d.catalog, scope, deploymentID, input.Arguments)
		if errors.Is(resolveErr, skill.ErrBindingAlreadyDisabled) || errors.Is(resolveErr, skill.ErrBindingRevisionConflict) {
			return d.replayedDisable(ctx, scope, deploymentID, args)
		}
		if resolveErr != nil {
			return nil, resolveErr
		}
		binding, disableErr := d.catalog.DisableBinding(ctx, skill.DisableBindingRequest{
			Scope: scope, DeploymentID: deploymentID, BindingID: args.BindingID, ExpectedRevision: args.ExpectedRevision,
			Actor: actor, Reason: "Approved Skill disable from conversation",
		})
		if errors.Is(disableErr, skill.ErrBindingAlreadyDisabled) || errors.Is(disableErr, skill.ErrBindingRevisionConflict) {
			return d.replayedDisable(ctx, scope, deploymentID, args)
		}
		return skillBindingActionResult(SkillActionDisableBinding, binding, false), disableErr
	default:
		return nil, errors.New("unsupported skill binding action")
	}
}

func approvedSkillActionCheckpoint(ctx context.Context, store models.ActionStore, call *models.ActionCall) (*models.ApprovalCheckpoint, error) {
	approval, err := store.GetApproval(ctx, call.Scope, call.ApprovalID)
	if err != nil {
		return nil, err
	}
	if approval.Status != models.ApprovalStatusApproved || approval.DecisionBy == nil {
		return nil, errors.New("Skill management action requires an approved checkpoint with a decision principal")
	}
	return approval, nil
}

func resolveSkillBindingUpsert(ctx context.Context, catalog *skill.Catalog, scope skill.ScopeReference, deploymentID string, arguments map[string]interface{}) (SkillBindingUpsertArguments, *skill.Definition, *skill.Binding, error) {
	var args SkillBindingUpsertArguments
	if err := decodeSkillBindingArguments(arguments, &args); err != nil {
		return args, nil, nil, err
	}
	args.BindingID = strings.TrimSpace(args.BindingID)
	args.SkillID = strings.TrimSpace(args.SkillID)
	args.SkillVersion = strings.TrimSpace(args.SkillVersion)
	args.SourceIdentity = strings.TrimSpace(args.SourceIdentity)
	if args.BindingID == "" || args.SkillID == "" || args.SkillVersion == "" || args.ExpectedRevision < 0 {
		return args, nil, nil, errors.New("bindingId, skillId, skillVersion, and a non-negative expectedRevision are required")
	}
	definition, err := ExactSkillDefinition(ctx, catalog, args.SkillID, args.SkillVersion, args.SourceIdentity)
	if err != nil {
		return args, nil, nil, err
	}
	if definition == nil {
		return args, nil, nil, errors.New("requested Skill definition is not registered")
	}
	current, err := catalog.GetBinding(ctx, scope, deploymentID, args.BindingID)
	if err != nil {
		return args, nil, nil, err
	}
	if current == nil && args.ExpectedRevision != 0 || current != nil && current.Revision != args.ExpectedRevision {
		return args, definition, current, skill.ErrBindingRevisionConflict
	}
	if current != nil && (current.SkillID != args.SkillID || current.SkillVersion != args.SkillVersion || current.SourceIdentity != args.SourceIdentity) {
		return args, definition, current, errors.New("an existing binding's Skill identity requires a canonical Skill reference upgrade before configuration")
	}
	var credentials map[string]skill.CredentialReference
	if current != nil {
		credentials = current.Credentials
	}
	if err := ValidateSkillBindingArguments(definition, args, credentials); err != nil {
		return args, definition, current, err
	}
	return args, definition, current, nil
}

func resolveSkillBindingDisable(ctx context.Context, catalog *skill.Catalog, scope skill.ScopeReference, deploymentID string, arguments map[string]interface{}) (skillBindingDisableArguments, *skill.Binding, error) {
	var args skillBindingDisableArguments
	if err := decodeSkillBindingArguments(arguments, &args); err != nil {
		return args, nil, err
	}
	args.BindingID = strings.TrimSpace(args.BindingID)
	if args.BindingID == "" || args.ExpectedRevision < 1 {
		return args, nil, errors.New("bindingId and a positive expectedRevision are required")
	}
	current, err := catalog.GetBinding(ctx, scope, deploymentID, args.BindingID)
	if err != nil {
		return args, nil, err
	}
	if current == nil {
		return args, nil, skill.ErrBindingNotFound
	}
	if current.Disabled {
		return args, current, skill.ErrBindingAlreadyDisabled
	}
	if current.Revision != args.ExpectedRevision {
		return args, current, skill.ErrBindingRevisionConflict
	}
	return args, current, nil
}

// validateSkillBindingArguments checks a proposed upsert against the exact
// definition. credentials are the saved binding's; a model cannot supply any.
func ValidateSkillBindingArguments(definition *skill.Definition, args SkillBindingUpsertArguments, credentials map[string]skill.CredentialReference) error {
	// The canonical Catalog performs the same recursive validation at write
	// time. Rejecting here prevents secret-shaped config from reaching approval
	// previews or durable Action arguments.
	if err := validateSkillActionNonSecretConfig(args.Config, ""); err != nil {
		return err
	}
	if len(args.AllowedActions) == 0 && !args.EnablePrompt && len(args.EnabledConversationAdapters) == 0 && len(credentials) == 0 {
		return errors.New("binding must enable a prompt or explicitly allow actions or conversation adapters")
	}
	if args.EnablePrompt && definition.Prompt == nil {
		return errors.New("binding enables a prompt that the Skill does not define")
	}
	if args.EnablePrompt {
		for _, requirement := range definition.Prompt.Credentials {
			reference, exists := credentials[requirement.Name]
			if requirement.Optional && !exists {
				continue
			}
			if !exists || strings.TrimSpace(reference.Kind) != requirement.Kind || strings.TrimSpace(reference.ID) == "" {
				return fmt.Errorf("binding prompt needs a saved %s account (%s); call request_setup", requirement.Name, requirement.Kind)
			}
		}
	}
	seen := map[string]bool{}
	for _, name := range args.AllowedActions {
		name = strings.TrimSpace(name)
		action, ok := definition.Actions[name]
		if name == "" || !ok || seen[name] {
			return fmt.Errorf("binding action %q is not a unique action on the exact Skill definition", name)
		}
		seen[name] = true
		if action.Risk.RankUnknownHighest() > args.MaximumRisk.RankUnknownHighest() {
			return fmt.Errorf("binding action %s exceeds maximum risk", name)
		}
		for _, requirement := range action.Credentials {
			reference, exists := credentials[requirement.Name]
			if requirement.Optional && !exists {
				continue
			}
			if !exists || strings.TrimSpace(reference.Kind) != requirement.Kind || strings.TrimSpace(reference.ID) == "" {
				return fmt.Errorf("binding action %s needs a saved %s account (%s); call request_setup", name, requirement.Name, requirement.Kind)
			}
		}
	}
	seenAdapters := map[string]bool{}
	for _, adapterID := range args.EnabledConversationAdapters {
		adapterID = strings.TrimSpace(adapterID)
		adapter, ok := definition.ConversationAdapters[adapterID]
		if adapterID == "" || !ok || seenAdapters[adapterID] {
			return fmt.Errorf("binding conversation adapter %q is not unique on the exact Skill definition", adapterID)
		}
		seenAdapters[adapterID] = true
		for _, requirement := range adapter.Credentials {
			reference, exists := credentials[requirement.Name]
			if requirement.Optional && !exists {
				continue
			}
			if !exists || strings.TrimSpace(reference.Kind) != requirement.Kind || strings.TrimSpace(reference.ID) == "" {
				return fmt.Errorf("binding conversation adapter %s needs a saved %s account (%s); call request_setup", adapterID, requirement.Name, requirement.Kind)
			}
		}
	}
	return nil
}

func validateSkillActionNonSecretConfig(value interface{}, path string) error {
	switch values := value.(type) {
	case map[string]interface{}:
		for key, child := range values {
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(strings.TrimSpace(key)))
			for _, suffix := range []string{"token", "apikey", "password", "secret", "credential", "credentialid", "accesstoken", "refreshtoken"} {
				if normalized == "token" || strings.HasSuffix(normalized, suffix) {
					return fmt.Errorf("binding config %s%s must use an opaque credential reference", path, key)
				}
			}
			if err := validateSkillActionNonSecretConfig(child, path+key+"."); err != nil {
				return err
			}
		}
	case []interface{}:
		for index, child := range values {
			if err := validateSkillActionNonSecretConfig(child, fmt.Sprintf("%s%d.", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func ExactSkillDefinition(ctx context.Context, catalog *skill.Catalog, id, version, sourceIdentity string) (*skill.Definition, error) {
	if sourceIdentity != "" {
		return catalog.GetDefinitionVariant(ctx, id, version, sourceIdentity)
	}
	definition, err := catalog.GetDefinition(ctx, id, version)
	if err != nil || definition == nil {
		return definition, err
	}
	if skill.DefinitionSourceIdentity(definition) != "" {
		return nil, errors.New("sourceIdentity is required for a sourced Skill definition")
	}
	return definition, nil
}

// skillBindingCandidate is the binding a model-proposed upsert writes. A
// model never adds, changes or removes credentials: the candidate keeps the
// current binding's, and only a person's card or route attaches them.
func SkillBindingCandidate(scope skill.ScopeReference, deploymentID string, args SkillBindingUpsertArguments, current *skill.Binding) *skill.Binding {
	var credentials map[string]skill.CredentialReference
	if current != nil {
		credentials = maps.Clone(current.Credentials)
	}
	allowed := append([]string(nil), args.AllowedActions...)
	sort.Strings(allowed)
	adapters := append([]string(nil), args.EnabledConversationAdapters...)
	sort.Strings(adapters)
	return &skill.Binding{
		ID: args.BindingID, Scope: scope, DeploymentID: deploymentID,
		SkillID: args.SkillID, SkillVersion: args.SkillVersion, SourceIdentity: args.SourceIdentity,
		AllowedActions: allowed, EnablePrompt: args.EnablePrompt, EnabledConversationAdapters: adapters, MaximumRisk: args.MaximumRisk,
		ArgumentRestrictions: args.ArgumentRestrictions, Credentials: credentials, Config: args.Config,
	}
}

// skillActionMember is the acting member: the Agent whose own management
// binding the action runs through.
func skillActionMember(run *models.AgentRun) (string, error) {
	if run == nil {
		return "", errors.New("Agent Run is required")
	}
	if deploymentID := strings.TrimSpace(run.AssignedAgentID); deploymentID != "" {
		return deploymentID, nil
	}
	return "", errors.New("skill management requires a Run assigned to an Agent deployment")
}

// skillTargetAgent is the Agent a builder acts for (targetAgentId).
type skillTargetAgent struct {
	ID          string
	DisplayName string
}

func (t *skillTargetAgent) owner() models.ObjectiveOwner {
	return models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: t.ID}
}

func (t *skillTargetAgent) preview() map[string]interface{} {
	return map[string]interface{}{"id": t.ID, "displayName": t.DisplayName}
}

// isTargetedSkillBindingAction reports whether bound is an openseal.skills
// upsert_binding or disable_binding whose arguments name another Agent with
// targetAgentId. It changes that Agent, so like create_agent and update_agent
// it is always reviewed and approved only by a person in the app, in every
// approval mode.
func IsTargetedSkillBindingAction(bound *skill.BoundAction, arguments map[string]interface{}) bool {
	if bound == nil || bound.Definition == nil || bound.Definition.ID != models.SkillManagementSkillID {
		return false
	}
	if bound.Action.Name != SkillActionUpsertBinding && bound.Action.Name != SkillActionDisableBinding {
		return false
	}
	_, present := arguments[SkillActionTargetAgentArgument]
	return present
}

// skillActionTargetable reports the actions that accept targetAgentId.
func skillActionTargetable(action string) bool {
	switch action {
	case SkillActionDiscoverBinding, SkillActionUpsertBinding, SkillActionDisableBinding, agentrun.SkillActionRequestSetup, agentrun.SkillActionRequestConnection:
		return true
	}
	return false
}

// skillActionTarget returns the Agent named by targetAgentId, or nil when
// the call names none. Only a builder (an Agent holding an openseal.agents
// binding that allows create_agent or update_agent) may name one, and only
// in a reply in its own chat. The target exists in scope, is not retired,
// is not the builder and is not fixed-policy, the same rule as update_agent.
func skillActionTarget(ctx context.Context, catalog *skill.Catalog, agents *kernelagent.Registry, run *models.AgentRun, member string, arguments map[string]interface{}) (*skillTargetAgent, error) {
	raw, present := arguments[SkillActionTargetAgentArgument]
	if !present {
		return nil, nil
	}
	value, _ := raw.(string)
	targetID := strings.TrimSpace(value)
	if targetID == "" {
		// A blank target names nobody: the model filled the optional
		// argument with nothing, so the call acts for this chat's Agent.
		return nil, nil
	}
	if len(targetID) > 128 {
		return nil, errors.New("targetAgentId must be an Agent deployment id; use list_agents for the exact id")
	}
	if targetID == member {
		return nil, errors.New("targetAgentId names the Agent you are; omit it for this chat's own Skills")
	}
	if catalog == nil || run == nil {
		return nil, errors.New("acting for another Agent is unavailable")
	}
	scope := skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	bindings, err := catalog.ListBindings(ctx, scope, member)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(bindings, agentBuilderBinding) {
		return nil, errors.New("only an Agent that builds Agents can act for another Agent; omit targetAgentId")
	}
	_, taskHint := run.Context[models.ConversationTaskContextKey]
	if run.Owner != (models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: member}) || run.Kind != models.RunKindConversation || taskHint || strings.HasPrefix(run.ConcurrencyKey, "task:") {
		return nil, errors.New("targetAgentId is only available in a reply in your own chat")
	}
	if agents == nil {
		return nil, errors.New("acting for another Agent is unavailable on this host")
	}
	deployment, err := agents.GetDeployment(ctx, capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, targetID)
	if err != nil {
		return nil, AgentBuilderTargetError(targetID, err)
	}
	if deployment.RolloutStatus == kernelagent.RolloutRetired {
		return nil, fmt.Errorf("agent %q is retired and cannot be changed", targetID)
	}
	definition, err := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if AgentDefinitionPolicyFixed(definition) {
		return nil, fmt.Errorf("agent %q cannot be changed in chat: %w", targetID, ErrAgentBehaviorFixed)
	}
	return &skillTargetAgent{ID: deployment.ID, DisplayName: definition.DisplayName}, nil
}

// withoutUnusableTargetAgent hides targetAgentId from the model-visible
// actions of an Agent that cannot use it. Only a builder may name another
// Agent (skillActionTarget), so every other Agent is offered the actions
// without the argument and without the sentence that describes it; a model
// that sees an optional "act for another Agent" field will fill it.
func WithoutUnusableTargetAgent(activation *skill.ActivationSnapshot, actions []capability.ModelAction) []capability.ModelAction {
	touched := false
	for _, action := range actions {
		if hasTargetAgentProperty(action) {
			touched = true
			break
		}
	}
	if !touched || activationHasAgentBuilder(activation) {
		return actions
	}
	out := make([]capability.ModelAction, len(actions))
	for i, action := range actions {
		out[i] = action
		if !hasTargetAgentProperty(action) {
			continue
		}
		out[i].InputSchema = maps.Clone(action.InputSchema)
		properties := make(map[string]interface{}, len(action.InputSchema["properties"].(map[string]interface{})))
		for name, property := range action.InputSchema["properties"].(map[string]interface{}) {
			if name != SkillActionTargetAgentArgument {
				properties[name] = property
			}
		}
		out[i].InputSchema["properties"] = properties
		out[i].Description = strings.Replace(action.Description, targetAgentConnectionSentence, "", 1)
	}
	return out
}

// activationHasAgentBuilder mirrors agentBuilderBinding on an activation: an
// openseal.agents binding offering create_agent or update_agent.
func activationHasAgentBuilder(activation *skill.ActivationSnapshot) bool {
	if activation == nil {
		return false
	}
	for _, activated := range activation.Skills {
		if activated.SkillID != models.AgentManagementSkillID || activated.SkillVersion != models.AgentManagementSkillVersion || activated.SourceIdentity != "" {
			continue
		}
		for _, action := range activated.Actions {
			if action.Action == models.AgentActionCreateAgent || action.Action == models.AgentActionUpdateAgent {
				return true
			}
		}
	}
	return false
}

// targetAgentConnectionSentence is the request_connection description
// sentence that only a builder can act on.
const targetAgentConnectionSentence = " With targetAgentId, the card still appears in this chat and the app is connected for that Agent."

func hasTargetAgentProperty(action capability.ModelAction) bool {
	if action.SkillID != models.SkillManagementSkillID || !skillActionTargetable(action.Action) {
		return false
	}
	properties, _ := action.InputSchema["properties"].(map[string]interface{})
	_, ok := properties[SkillActionTargetAgentArgument]
	return ok
}

// agentBuilderBinding reports an enabled openseal.agents binding that lets
// its Agent create or change other Agents.
func agentBuilderBinding(binding *skill.Binding) bool {
	return binding != nil && !binding.Disabled && binding.SkillID == models.AgentManagementSkillID && binding.SkillVersion == models.AgentManagementSkillVersion &&
		binding.SourceIdentity == "" && (slices.Contains(binding.AllowedActions, models.AgentActionCreateAgent) || slices.Contains(binding.AllowedActions, models.AgentActionUpdateAgent))
}

func decodeSkillBindingArguments(arguments map[string]interface{}, target interface{}) error {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return fmt.Errorf("encode skill binding action: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid skill binding action: %w", err)
	}
	return nil
}

func SecretSafeBindingPreview(binding *skill.Binding) interface{} {
	if binding == nil {
		return nil
	}
	credentials := make(map[string]interface{}, len(binding.Credentials))
	for name, reference := range binding.Credentials {
		credentials[name] = map[string]interface{}{"kind": reference.Kind, "id": reference.ID}
	}
	return map[string]interface{}{
		"id": binding.ID, "skillId": binding.SkillID, "skillVersion": binding.SkillVersion, "sourceIdentity": binding.SourceIdentity,
		"disabled": binding.Disabled, "allowedActions": append([]string(nil), binding.AllowedActions...), "enablePrompt": binding.EnablePrompt,
		"enabledConversationAdapters": append([]string(nil), binding.EnabledConversationAdapters...),
		"maximumRisk":                 binding.MaximumRisk, "argumentRestrictions": binding.ArgumentRestrictions, "config": binding.Config,
		"credentials": credentials, "revision": binding.Revision,
	}
}

func (d *SkillBindingActionDispatcher) replayedUpsert(ctx context.Context, scope skill.ScopeReference, deploymentID string, args SkillBindingUpsertArguments) (map[string]interface{}, error) {
	current, err := d.catalog.GetBinding(ctx, scope, deploymentID, args.BindingID)
	if err != nil {
		return nil, err
	}
	candidate := SkillBindingCandidate(scope, deploymentID, args, current)
	if current == nil || current.Disabled || !SameManagedSkillBinding(current, candidate) {
		return nil, skill.ErrBindingRevisionConflict
	}
	// A creation under a deleted binding's ID starts above its revision floor,
	// so a replayed creation is recognized by its single "created" entry.
	created := args.ExpectedRevision == 0 && len(current.Lifecycle) == 1 &&
		current.Lifecycle[0].Action == skill.BindingLifecycleCreated && current.Lifecycle[0].Revision == current.Revision
	if current.Revision != args.ExpectedRevision+1 && !created {
		return nil, skill.ErrBindingRevisionConflict
	}
	return skillBindingActionResult(SkillActionUpsertBinding, current, true), nil
}

func (d *SkillBindingActionDispatcher) replayedDisable(ctx context.Context, scope skill.ScopeReference, deploymentID string, args skillBindingDisableArguments) (map[string]interface{}, error) {
	current, err := d.catalog.GetBinding(ctx, scope, deploymentID, args.BindingID)
	if err != nil {
		return nil, err
	}
	if current == nil || !current.Disabled || current.Revision != args.ExpectedRevision+1 {
		return nil, skill.ErrBindingRevisionConflict
	}
	return skillBindingActionResult(SkillActionDisableBinding, current, true), nil
}

func SameManagedSkillBinding(left, right *skill.Binding) bool {
	return left.SkillID == right.SkillID && left.SkillVersion == right.SkillVersion && left.SourceIdentity == right.SourceIdentity &&
		reflect.DeepEqual(left.AllowedActions, right.AllowedActions) && left.EnablePrompt == right.EnablePrompt &&
		reflect.DeepEqual(left.EnabledConversationAdapters, right.EnabledConversationAdapters) && left.MaximumRisk == right.MaximumRisk &&
		reflect.DeepEqual(left.ArgumentRestrictions, right.ArgumentRestrictions) && reflect.DeepEqual(left.Credentials, right.Credentials) && reflect.DeepEqual(left.Config, right.Config)
}

func skillBindingActionResult(operation string, binding *skill.Binding, replayed bool) map[string]interface{} {
	return map[string]interface{}{"resourceType": "skill_binding", "operation": operation, "replayed": replayed, "binding": binding}
}

func isSkillBindingAction(bound *skill.BoundAction) bool {
	if bound == nil || bound.Definition == nil || bound.Definition.ID != models.SkillManagementSkillID || skill.DefinitionSourceIdentity(bound.Definition) != "" {
		return false
	}
	if bound.Definition.Version != SkillManagementSkillVersion {
		current := SkillManagementSkill()
		if _, ok := bound.Definition.Actions[bound.Action.Name]; !ok {
			return false
		}
		if _, ok := current.Actions[bound.Action.Name]; !ok {
			return false
		}
		if len(compareUpgradeContracts(bound.Definition, current, []string{bound.Action.Name}, nil)) != 0 {
			return false
		}
	}
	return bound.Action.Name == SkillActionListSetupRequests || bound.Action.Name == agentrun.SkillActionRequestSetup || bound.Action.Name == agentrun.SkillActionRequestCredential || bound.Action.Name == agentrun.SkillActionRequestConnection || bound.Action.Name == SkillActionFindApps || bound.Action.Name == SkillActionDiscoverBinding || bound.Action.Name == SkillActionUpsertBinding || bound.Action.Name == SkillActionDisableBinding
}
