package skillbinding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	SkillManagementSkillVersion = "1.7.0"
	SkillActionDiscoverBinding  = "discover"
	SkillActionUpsertBinding    = "upsert_binding"
	SkillActionDisableBinding   = "disable_binding"
	SkillManagementEndpoint     = "kernel://skills"
)

// UpsertArguments are the arguments of upsert_binding.
type UpsertArguments struct {
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

// ActionValidator resolves both deployment authority and the
// exact registered Skill before a proposal can become an approval request.
type ActionValidator struct {
	catalog *skill.Catalog
	roster  models.TeamRosterReader
	agents  *kernelagent.Registry
}

// NewActionValidator checks Skill management proposals. roster
// reads Team (and, when it can, Agent) deployments to derive the run's
// umbrella, which every binding change targets.
func NewActionValidator(catalog *skill.Catalog, roster models.TeamRosterReader) (*ActionValidator, error) {
	if catalog == nil {
		return nil, errors.New("skill catalog is required")
	}
	return &ActionValidator{catalog: catalog, roster: roster}, nil
}

// UseAgentRegistry lets a builder act for another Agent (targetAgentId).
// Without it every targetAgentId is refused.
func (v *ActionValidator) UseAgentRegistry(agents *kernelagent.Registry) {
	if v != nil {
		v.agents = agents
	}
}

func (v *ActionValidator) ValidateActionProposal(ctx context.Context, input agentrun.ActionProposalValidationInput) (map[string]interface{}, error) {
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
		preview["changes"] = SecretSafeBindingPreview(Candidate(scope, deploymentID, args, current))
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

// ActionDispatcher materializes approved proposals through the
// existing CAS-protected, audited Skill binding lifecycle.
type ActionDispatcher struct {
	store     models.KernelStore
	catalog   *skill.Catalog
	discovery skill.DiscoveryProvider
	fallback  agentrun.ActionDispatcher
	apps      *AppCatalogRef
	agents    *kernelagent.Registry
}

// UseAgentRegistry lets a builder act for another Agent (targetAgentId).
// Without it every targetAgentId is refused.
func (d *ActionDispatcher) UseAgentRegistry(agents *kernelagent.Registry) {
	if d != nil {
		d.agents = agents
	}
}

// roster reads the Team and Agent deployments RunUmbrella checks.
func (d *ActionDispatcher) roster() models.TeamRosterReader {
	return models.TeamRosterReaderOf(d.store)
}

func NewActionDispatcher(store models.KernelStore, catalog *skill.Catalog, fallback agentrun.ActionDispatcher, discovery ...skill.DiscoveryProvider) (*ActionDispatcher, error) {
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
	return &ActionDispatcher{store: store, catalog: catalog, discovery: provider, fallback: fallback}, nil
}

func (d *ActionDispatcher) DispatchAction(ctx context.Context, input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
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
			Binding: Candidate(scope, deploymentID, args, current), ExpectedRevision: args.ExpectedRevision,
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

func resolveSkillBindingUpsert(ctx context.Context, catalog *skill.Catalog, scope skill.ScopeReference, deploymentID string, arguments map[string]interface{}) (UpsertArguments, *skill.Definition, *skill.Binding, error) {
	var args UpsertArguments
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

// ValidateSkillBindingArguments checks a proposed upsert against the exact
// definition. credentials are the saved binding's; a model cannot supply any.
func ValidateSkillBindingArguments(definition *skill.Definition, args UpsertArguments, credentials map[string]skill.CredentialReference) error {
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

// ExactSkillDefinition returns one Skill definition version; a sourced
// definition must be named with its sourceIdentity.
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

// Candidate is the binding a model-proposed upsert writes. A
// model never adds, changes or removes credentials: the candidate keeps the
// current binding's, and only a person's card or route attaches them.
func Candidate(scope skill.ScopeReference, deploymentID string, args UpsertArguments, current *skill.Binding) *skill.Binding {
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

// SecretSafeBindingPreview describes binding for a person or model without
// credential values.
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

func (d *ActionDispatcher) replayedUpsert(ctx context.Context, scope skill.ScopeReference, deploymentID string, args UpsertArguments) (map[string]interface{}, error) {
	current, err := d.catalog.GetBinding(ctx, scope, deploymentID, args.BindingID)
	if err != nil {
		return nil, err
	}
	candidate := Candidate(scope, deploymentID, args, current)
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

func (d *ActionDispatcher) replayedDisable(ctx context.Context, scope skill.ScopeReference, deploymentID string, args skillBindingDisableArguments) (map[string]interface{}, error) {
	current, err := d.catalog.GetBinding(ctx, scope, deploymentID, args.BindingID)
	if err != nil {
		return nil, err
	}
	if current == nil || !current.Disabled || current.Revision != args.ExpectedRevision+1 {
		return nil, skill.ErrBindingRevisionConflict
	}
	return skillBindingActionResult(SkillActionDisableBinding, current, true), nil
}

// SameManagedSkillBinding reports whether two bindings agree on every field
// a model-proposed upsert can set.
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
