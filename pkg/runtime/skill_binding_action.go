package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	SkillManagementSkillID      = "openseal.skills"
	SkillManagementSkillVersion = "1.1.0"
	SkillActionDiscoverBinding  = "discover"
	SkillActionUpsertBinding    = "upsert_binding"
	SkillActionDisableBinding   = "disable_binding"
	SkillManagementEndpoint     = "kernel://skills"
)

// SkillManagementSkill exposes the canonical Skill binding lifecycle to an
// Agent conversation. The target deployment is deliberately absent from the
// model-visible schema: the kernel derives it from the durable Run.
func SkillManagementSkill() *skill.Definition {
	credentialReference := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"kind": map[string]interface{}{"type": "string", "minLength": 1},
			"id":   map[string]interface{}{"type": "string", "minLength": 1},
		},
		"required": []interface{}{"kind", "id"},
	}
	upsertProperties := map[string]interface{}{
		"bindingId":        map[string]interface{}{"type": "string", "minLength": 1},
		"expectedRevision": map[string]interface{}{"type": "integer", "minimum": 0},
		"skillId":          map[string]interface{}{"type": "string", "minLength": 1},
		"skillVersion":     map[string]interface{}{"type": "string", "minLength": 1},
		"sourceIdentity":   map[string]interface{}{"type": "string"},
		"allowedActions":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1}, "uniqueItems": true},
		"enablePrompt":     map[string]interface{}{"type": "boolean"},
		"maximumRisk": map[string]interface{}{"type": "string", "enum": []interface{}{
			string(skill.RiskLevelRead), string(skill.RiskLevelWrite), string(skill.RiskLevelExternal), string(skill.RiskLevelProduction), string(skill.RiskLevelDestructive),
		}},
		"argumentRestrictions": map[string]interface{}{"type": "object"},
		"config":               map[string]interface{}{"type": "object"},
		// Access references are opaque identifiers resolved by the host. The
		// field intentionally cannot carry credential values.
		"accessReferences": map[string]interface{}{
			"type": "object", "additionalProperties": credentialReference,
		},
	}
	return &skill.Definition{
		ID: SkillManagementSkillID, Version: SkillManagementSkillVersion,
		Name: "Skills", Description: "Propose governed changes to the current Agent's Skill access.",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: SkillManagementEndpoint},
		Actions: map[string]skill.Action{
			SkillActionDiscoverBinding: skillDiscoveryAction(),
			SkillActionUpsertBinding: skillManagementAction(
				SkillActionUpsertBinding,
				"Propose enabling or updating an exact registered Skill for this Agent. Credential values are never accepted; credentials must be opaque references.",
				upsertProperties,
				[]interface{}{"bindingId", "expectedRevision", "skillId", "skillVersion", "allowedActions", "enablePrompt", "maximumRisk"},
			),
			SkillActionDisableBinding: skillManagementAction(
				SkillActionDisableBinding,
				"Propose disabling one current Skill binding for this Agent using its current revision.",
				map[string]interface{}{
					"bindingId":        map[string]interface{}{"type": "string", "minLength": 1},
					"expectedRevision": map[string]interface{}{"type": "integer", "minimum": 1},
				},
				[]interface{}{"bindingId", "expectedRevision"},
			),
		},
	}
}

func skillDiscoveryAction() skill.Action {
	riskValues := []interface{}{string(skill.RiskLevelRead), string(skill.RiskLevelWrite), string(skill.RiskLevelExternal), string(skill.RiskLevelProduction), string(skill.RiskLevelDestructive)}
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
		},
		"required": []interface{}{"name", "kind", "configured"},
	}
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
			"maximumRisk":   map[string]interface{}{"type": "string", "enum": riskValues},
			"readiness":     map[string]interface{}{"type": "string", "enum": []interface{}{string(skill.DiscoveryReadinessBindable), string(skill.DiscoveryReadinessNeedsInstallation), string(skill.DiscoveryReadinessUnavailable)}},
			"compatibility": map[string]interface{}{"type": "array", "items": compatibilitySchema},
		},
		"required": []interface{}{"id", "version", "name", "readiness"},
	}
	return skill.Action{
		Name:        SkillActionDiscoverBinding,
		Description: "Find exact authorized Skills that could satisfy a capability request for the current Agent. Results contain no credential references or values and do not install or activate anything.",
		Risk:        skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported,
		Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"query":           map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 512},
				"requiredActions": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128}, "uniqueItems": true, "maxItems": 32},
				"maximumRisk":     map[string]interface{}{"type": "string", "enum": riskValues},
				"cursor":          map[string]interface{}{"type": "string", "maxLength": 1024},
				"limit":           map[string]interface{}{"type": "integer", "minimum": 1, "maximum": skill.MaximumDiscoveryLimit},
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

type skillBindingUpsertArguments struct {
	BindingID            string                                   `json:"bindingId"`
	ExpectedRevision     int64                                    `json:"expectedRevision"`
	SkillID              string                                   `json:"skillId"`
	SkillVersion         string                                   `json:"skillVersion"`
	SourceIdentity       string                                   `json:"sourceIdentity,omitempty"`
	AllowedActions       []string                                 `json:"allowedActions"`
	EnablePrompt         bool                                     `json:"enablePrompt"`
	MaximumRisk          skill.RiskLevel                          `json:"maximumRisk"`
	ArgumentRestrictions map[string]map[string]skill.ArgumentRule `json:"argumentRestrictions,omitempty"`
	AccessReferences     map[string]skill.CredentialReference     `json:"accessReferences,omitempty"`
	Config               map[string]interface{}                   `json:"config,omitempty"`
}

type skillBindingDisableArguments struct {
	BindingID        string `json:"bindingId"`
	ExpectedRevision int64  `json:"expectedRevision"`
}

type skillDiscoveryArguments struct {
	Query           string          `json:"query"`
	RequiredActions []string        `json:"requiredActions,omitempty"`
	MaximumRisk     skill.RiskLevel `json:"maximumRisk,omitempty"`
	Cursor          string          `json:"cursor,omitempty"`
	Limit           int             `json:"limit,omitempty"`
}

// SkillBindingActionValidator resolves both deployment authority and the
// exact registered Skill before a proposal can become an approval request.
type SkillBindingActionValidator struct{ catalog *skill.Catalog }

func NewSkillBindingActionValidator(catalog *skill.Catalog) (*SkillBindingActionValidator, error) {
	if catalog == nil {
		return nil, errors.New("skill catalog is required")
	}
	return &SkillBindingActionValidator{catalog: catalog}, nil
}

func (v *SkillBindingActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isSkillBindingAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.catalog == nil || input.Run == nil || input.Bound.Binding == nil {
		return nil, errors.New("skill binding action validator is not configured")
	}
	deploymentID, err := skillActionAgentDeployment(input.Run)
	if err != nil {
		return nil, err
	}
	if input.Bound.Binding.DeploymentID != deploymentID {
		return nil, errors.New("skill management action is not bound to the current Run's Agent deployment")
	}
	scope := skill.ScopeReference{Kind: input.Run.Scope.Kind, ID: input.Run.Scope.ID}
	preview := map[string]interface{}{
		"resourceType": "skill_binding", "operation": input.Bound.Action.Name, "deploymentId": deploymentID,
	}
	switch input.Bound.Action.Name {
	case SkillActionDiscoverBinding:
		return nil, nil
	case SkillActionUpsertBinding:
		args, definition, current, resolveErr := resolveSkillBindingUpsert(ctx, v.catalog, scope, deploymentID, input.Arguments)
		if resolveErr != nil {
			return nil, resolveErr
		}
		preview["bindingId"] = args.BindingID
		preview["expectedRevision"] = args.ExpectedRevision
		preview["skill"] = map[string]interface{}{
			"id": definition.ID, "version": definition.Version, "sourceIdentity": skill.DefinitionSourceIdentity(definition), "name": definition.Name,
		}
		preview["current"] = secretSafeBindingPreview(current)
		preview["changes"] = secretSafeBindingPreview(skillBindingCandidate(scope, deploymentID, args))
		return preview, nil
	case SkillActionDisableBinding:
		args, current, resolveErr := resolveSkillBindingDisable(ctx, v.catalog, scope, deploymentID, input.Arguments)
		if resolveErr != nil {
			return nil, resolveErr
		}
		preview["bindingId"] = args.BindingID
		preview["expectedRevision"] = args.ExpectedRevision
		preview["current"] = secretSafeBindingPreview(current)
		preview["changes"] = map[string]interface{}{"disabled": true}
		return preview, nil
	default:
		return nil, errors.New("unsupported skill binding action")
	}
}

// SkillBindingActionDispatcher materializes approved proposals through the
// existing CAS-protected, audited Skill binding lifecycle.
type SkillBindingActionDispatcher struct {
	store     KernelStore
	catalog   *skill.Catalog
	discovery skill.DiscoveryProvider
	fallback  ActionDispatcher
}

func NewSkillBindingActionDispatcher(store KernelStore, catalog *skill.Catalog, fallback ActionDispatcher, discovery ...skill.DiscoveryProvider) (*SkillBindingActionDispatcher, error) {
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

func (d *SkillBindingActionDispatcher) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
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
			err = ErrRunNotFound
		}
		return nil, err
	}
	deploymentID, err := skillActionAgentDeployment(run)
	if err != nil {
		return nil, err
	}
	if input.Call.DeploymentID != deploymentID || input.Bound.Binding == nil || input.Bound.Binding.DeploymentID != deploymentID {
		return nil, errors.New("skill management action cannot target another Agent deployment")
	}
	actor := skill.BindingActor{Type: "agent", ID: deploymentID}
	if input.Call.ApprovalID != "" {
		approval, approvalErr := approvedSkillActionCheckpoint(ctx, d.store, input.Call)
		if approvalErr != nil {
			return nil, approvalErr
		}
		actor = skill.BindingActor{Type: approval.DecisionBy.Type, ID: approval.DecisionBy.ID}
	}
	scope := skill.ScopeReference{Kind: input.Call.Scope.Kind, ID: input.Call.Scope.ID}
	switch input.Bound.Action.Name {
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
		binding, upsertErr := d.catalog.UpsertBinding(ctx, skill.UpsertBindingRequest{
			Binding: skillBindingCandidate(scope, deploymentID, args), ExpectedRevision: args.ExpectedRevision,
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

func approvedSkillActionCheckpoint(ctx context.Context, store ActionStore, call *ActionCall) (*ApprovalCheckpoint, error) {
	approval, err := store.GetApproval(ctx, call.Scope, call.ApprovalID)
	if err != nil {
		return nil, err
	}
	if approval.Status != ApprovalStatusApproved || approval.DecisionBy == nil {
		return nil, errors.New("Skill management action requires an approved checkpoint with a decision principal")
	}
	return approval, nil
}

func resolveSkillBindingUpsert(ctx context.Context, catalog *skill.Catalog, scope skill.ScopeReference, deploymentID string, arguments map[string]interface{}) (skillBindingUpsertArguments, *skill.Definition, *skill.Binding, error) {
	var args skillBindingUpsertArguments
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
	definition, err := exactSkillDefinition(ctx, catalog, args.SkillID, args.SkillVersion, args.SourceIdentity)
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
	if err := validateSkillBindingArguments(definition, args); err != nil {
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

func validateSkillBindingArguments(definition *skill.Definition, args skillBindingUpsertArguments) error {
	if len(args.AllowedActions) == 0 && !args.EnablePrompt {
		return errors.New("binding must enable a prompt or explicitly allow actions")
	}
	if args.EnablePrompt && definition.Prompt == nil {
		return errors.New("binding enables a prompt that the Skill does not define")
	}
	if args.EnablePrompt {
		for _, requirement := range definition.Prompt.Credentials {
			reference, exists := args.AccessReferences[requirement.Name]
			if requirement.Optional && !exists {
				continue
			}
			if !exists || strings.TrimSpace(reference.Kind) != requirement.Kind || strings.TrimSpace(reference.ID) == "" {
				return fmt.Errorf("binding prompt requires opaque credential %s of kind %s", requirement.Name, requirement.Kind)
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
		if skillRiskRank(action.Risk) > skillRiskRank(args.MaximumRisk) {
			return fmt.Errorf("binding action %s exceeds maximum risk", name)
		}
		for _, requirement := range action.Credentials {
			reference, exists := args.AccessReferences[requirement.Name]
			if requirement.Optional && !exists {
				continue
			}
			if !exists || strings.TrimSpace(reference.Kind) != requirement.Kind || strings.TrimSpace(reference.ID) == "" {
				return fmt.Errorf("binding action %s requires opaque credential %s of kind %s", name, requirement.Name, requirement.Kind)
			}
		}
	}
	for name, reference := range args.AccessReferences {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(reference.Kind) == "" || strings.TrimSpace(reference.ID) == "" {
			return errors.New("credential references require a name, kind, and opaque id")
		}
	}
	// The canonical Catalog performs the same recursive validation at write
	// time. Rejecting here prevents secret-shaped config from reaching approval
	// previews or durable Action arguments.
	if err := validateSkillActionNonSecretConfig(args.Config, ""); err != nil {
		return err
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

func exactSkillDefinition(ctx context.Context, catalog *skill.Catalog, id, version, sourceIdentity string) (*skill.Definition, error) {
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

func skillBindingCandidate(scope skill.ScopeReference, deploymentID string, args skillBindingUpsertArguments) *skill.Binding {
	allowed := append([]string(nil), args.AllowedActions...)
	sort.Strings(allowed)
	return &skill.Binding{
		ID: args.BindingID, Scope: scope, DeploymentID: deploymentID,
		SkillID: args.SkillID, SkillVersion: args.SkillVersion, SourceIdentity: args.SourceIdentity,
		AllowedActions: allowed, EnablePrompt: args.EnablePrompt, MaximumRisk: args.MaximumRisk,
		ArgumentRestrictions: args.ArgumentRestrictions, Credentials: args.AccessReferences, Config: args.Config,
	}
}

func skillActionAgentDeployment(run *AgentRun) (string, error) {
	if run == nil {
		return "", errors.New("Agent Run is required")
	}
	if deploymentID := strings.TrimSpace(run.AssignedAgentID); deploymentID != "" {
		return deploymentID, nil
	}
	if run.Owner.Type == OwnerTypeAgent && strings.TrimSpace(run.Owner.ID) != "" {
		return strings.TrimSpace(run.Owner.ID), nil
	}
	return "", errors.New("skill management requires a Run assigned to an Agent deployment")
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

func secretSafeBindingPreview(binding *skill.Binding) interface{} {
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
		"maximumRisk": binding.MaximumRisk, "argumentRestrictions": binding.ArgumentRestrictions, "config": binding.Config,
		"credentials": credentials, "revision": binding.Revision,
	}
}

func (d *SkillBindingActionDispatcher) replayedUpsert(ctx context.Context, scope skill.ScopeReference, deploymentID string, args skillBindingUpsertArguments) (map[string]interface{}, error) {
	current, err := d.catalog.GetBinding(ctx, scope, deploymentID, args.BindingID)
	if err != nil {
		return nil, err
	}
	candidate := skillBindingCandidate(scope, deploymentID, args)
	if current == nil || current.Revision != args.ExpectedRevision+1 || current.Disabled || !sameManagedSkillBinding(current, candidate) {
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

func sameManagedSkillBinding(left, right *skill.Binding) bool {
	return left.SkillID == right.SkillID && left.SkillVersion == right.SkillVersion && left.SourceIdentity == right.SourceIdentity &&
		reflect.DeepEqual(left.AllowedActions, right.AllowedActions) && left.EnablePrompt == right.EnablePrompt && left.MaximumRisk == right.MaximumRisk &&
		reflect.DeepEqual(left.ArgumentRestrictions, right.ArgumentRestrictions) && reflect.DeepEqual(left.Credentials, right.Credentials) && reflect.DeepEqual(left.Config, right.Config)
}

func skillBindingActionResult(operation string, binding *skill.Binding, replayed bool) map[string]interface{} {
	return map[string]interface{}{"resourceType": "skill_binding", "operation": operation, "replayed": replayed, "binding": binding}
}

func skillRiskRank(value skill.RiskLevel) int {
	switch value {
	case skill.RiskLevelRead:
		return 1
	case skill.RiskLevelWrite:
		return 2
	case skill.RiskLevelExternal:
		return 3
	case skill.RiskLevelProduction:
		return 4
	case skill.RiskLevelDestructive:
		return 5
	default:
		return 100
	}
}

func isSkillBindingAction(bound *skill.BoundAction) bool {
	if bound == nil || bound.Definition == nil || bound.Definition.ID != SkillManagementSkillID || bound.Definition.Version != SkillManagementSkillVersion {
		return false
	}
	return bound.Action.Name == SkillActionDiscoverBinding || bound.Action.Name == SkillActionUpsertBinding || bound.Action.Name == SkillActionDisableBinding
}
