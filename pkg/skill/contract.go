package skill

import (
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type RiskLevel = capability.RiskLevel
type SideEffect = capability.SideEffect
type IdempotencyMode = capability.IdempotencyMode
type CredentialRequirement = capability.CredentialRequirement
type CredentialReference = capability.CredentialReference
type OAuth2Subject = capability.OAuth2Subject
type OAuth2Requirement = capability.OAuth2Requirement
type OAuth2GrantSummary = capability.OAuth2GrantSummary
type ConversationEndpointMode = capability.ConversationEndpointMode
type ConversationAdapterFeature = capability.ConversationAdapterFeature
type ConversationDeliveryOperation = capability.ConversationDeliveryOperation
type ConversationDeliveryOrdering = capability.ConversationDeliveryOrdering
type ConversationDeliveryCapabilities = capability.ConversationDeliveryCapabilities
type ConversationAdapterTransport = capability.ConversationAdapterTransport
type ConversationDestinationDiscovery = capability.ConversationDestinationDiscovery
type ConversationAdapter = capability.ConversationAdapter
type BoundConversationAdapter = capability.BoundConversationAdapter
type ActionRetryPolicy = capability.ActionRetryPolicy
type ExternalOperationPolicy = capability.ExternalOperationPolicy
type Duration = capability.Duration
type Action = capability.Action
type TransportReference = capability.TransportReference
type TransportArgument = capability.TransportArgument
type Definition = capability.Definition
type ArgumentRule = capability.ArgumentRule
type Binding = capability.Binding
type ScopeReference = capability.ScopeReference
type ModelAction = capability.ModelAction
type ModelPrompt = capability.ModelPrompt
type BoundAction = capability.BoundAction
type BindingReference = capability.BindingReference
type BindingActor = capability.BindingActor
type BindingLifecycleAction = capability.BindingLifecycleAction
type BindingLifecycleEntry = capability.BindingLifecycleEntry

const (
	RiskLevelRead        = capability.RiskLevelRead
	RiskLevelWrite       = capability.RiskLevelWrite
	RiskLevelExternal    = capability.RiskLevelExternal
	RiskLevelProduction  = capability.RiskLevelProduction
	RiskLevelDestructive = capability.RiskLevelDestructive

	SideEffectNone        = capability.SideEffectNone
	SideEffectRead        = capability.SideEffectRead
	SideEffectWrite       = capability.SideEffectWrite
	SideEffectExternal    = capability.SideEffectExternal
	SideEffectDestructive = capability.SideEffectDestructive

	IdempotencyNone      = capability.IdempotencyNone
	IdempotencySupported = capability.IdempotencySupported
	IdempotencyRequired  = capability.IdempotencyRequired

	ExternalOperationForbidden = capability.ExternalOperationForbidden
	ExternalOperationOptional  = capability.ExternalOperationOptional
	ExternalOperationRequired  = capability.ExternalOperationRequired

	OAuth2SubjectInstallation = capability.OAuth2SubjectInstallation
	OAuth2SubjectUser         = capability.OAuth2SubjectUser

	ConversationEndpointChannel = capability.ConversationEndpointChannel
	ConversationEndpointDirect  = capability.ConversationEndpointDirect

	ConversationAdapterProtocolV1 = capability.ConversationAdapterProtocolV1

	ConversationFeatureThreads     = capability.ConversationFeatureThreads
	ConversationFeatureMentions    = capability.ConversationFeatureMentions
	ConversationFeatureAttachments = capability.ConversationFeatureAttachments
	ConversationFeatureReactions   = capability.ConversationFeatureReactions
	ConversationFeatureEdits       = capability.ConversationFeatureEdits
	ConversationFeatureDeletes     = capability.ConversationFeatureDeletes
	ConversationFeatureTyping      = capability.ConversationFeatureTyping

	ConversationDeliveryMessageSend     = capability.ConversationDeliveryMessageSend
	ConversationDeliveryMessageUpdate   = capability.ConversationDeliveryMessageUpdate
	ConversationDeliveryMessageDelete   = capability.ConversationDeliveryMessageDelete
	ConversationDeliveryReactionAdd     = capability.ConversationDeliveryReactionAdd
	ConversationDeliveryReactionRemove  = capability.ConversationDeliveryReactionRemove
	ConversationDeliveryTypingIndicator = capability.ConversationDeliveryTypingIndicator

	ConversationDeliveryOrderEndpoint     = capability.ConversationDeliveryOrderEndpoint
	ConversationDeliveryOrderConversation = capability.ConversationDeliveryOrderConversation
	ConversationDeliveryOrderThread       = capability.ConversationDeliveryOrderThread

	ConversationEventMessageReceived   = capability.ConversationEventMessageReceived
	ConversationEventMessageUpdated    = capability.ConversationEventMessageUpdated
	ConversationEventMessageDeleted    = capability.ConversationEventMessageDeleted
	ConversationEventReactionAdded     = capability.ConversationEventReactionAdded
	ConversationEventReactionRemoved   = capability.ConversationEventReactionRemoved
	ConversationEventParticipantJoined = capability.ConversationEventParticipantJoined
	ConversationEventParticipantLeft   = capability.ConversationEventParticipantLeft

	BindingLifecycleCreated  = capability.BindingLifecycleCreated
	BindingLifecycleUpdated  = capability.BindingLifecycleUpdated
	BindingLifecycleEnabled  = capability.BindingLifecycleEnabled
	BindingLifecycleDisabled = capability.BindingLifecycleDisabled

	// SchemaExtensionKernelResolved marks an action argument that is required
	// by the executable contract but supplied by the trusted kernel rather than
	// requested from the model or user. Model action projections remove these
	// fields while the canonical Skill schema remains strict.
	SchemaExtensionKernelResolved = "x-openseal-kernel-resolved"
)

type Catalog struct {
	mu       sync.RWMutex
	skills   map[string]*Definition
	bindings map[string]*Binding
	schemas  map[string]*compiledActionSchemas
	store    CatalogStore
}

func NewCatalog() *Catalog {
	return &Catalog{
		skills: make(map[string]*Definition), bindings: make(map[string]*Binding),
		schemas: make(map[string]*compiledActionSchemas),
	}
}

// NewCatalogWithStore creates a catalog backed by durable control-plane
// storage. Definitions and bindings are loaded on demand rather than globally
// hydrated, preserving tenant isolation and bounded startup cost.
func NewCatalogWithStore(store CatalogStore) *Catalog {
	catalog := NewCatalog()
	catalog.store = store
	return catalog
}

func (c *Catalog) Register(ctx context.Context, definition *Definition) error {
	if c == nil {
		return errors.New("skill catalog is not configured")
	}
	if err := validateDefinition(definition); err != nil {
		return err
	}
	compiled, err := compileDefinitionSchemas(definition)
	if err != nil {
		return err
	}
	copy := cloneDefinition(definition)
	if copy.Source != nil {
		copy.Source.Identity = DefinitionSourceIdentity(copy)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sourceIdentity := DefinitionSourceIdentity(definition)
	key := definitionKey(definition.ID, definition.Version, sourceIdentity)
	if c.skills[key] != nil {
		return ErrDefinitionImmutable
	}
	if c.store != nil {
		if err := c.store.CreateSkillDefinition(ctx, copy); err != nil {
			return err
		}
	}
	c.skills[key] = copy
	for name, schemas := range compiled {
		c.schemas[actionKey(definition.ID, definition.Version, sourceIdentity, name)] = schemas
	}
	return nil
}

func (c *Catalog) GetDefinition(ctx context.Context, id, version string) (*Definition, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(version) == "" {
		return nil, errors.New("skill id and version are required")
	}
	definition, err := c.definitionFor(ctx, id, version, "")
	if err != nil {
		return nil, err
	}
	if definition == nil {
		return nil, nil
	}
	return cloneDefinition(definition), nil
}

// GetDefinitionVariant returns one exact source-qualified variant while
// keeping the declared Skill ID and version as the portable capability
// identity. Source identity is provenance and never becomes a model tool name.
func (c *Catalog) GetDefinitionVariant(ctx context.Context, id, version, sourceIdentity string) (*Definition, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(version) == "" || strings.TrimSpace(sourceIdentity) == "" {
		return nil, errors.New("skill id, version, and source identity are required")
	}
	definition, err := c.definitionFor(ctx, id, version, sourceIdentity)
	if err != nil || definition == nil {
		return definition, err
	}
	return cloneDefinition(definition), nil
}

// ValidateBindingCandidate validates an exact binding projection against its
// immutable Skill definition without persisting it. It is used by governed
// change-set operations that must validate every dependent reference before
// committing any mutation.
func (c *Catalog) ValidateBindingCandidate(ctx context.Context, binding *Binding) error {
	if c == nil {
		return errors.New("skill catalog is not configured")
	}
	if err := validateBindingShape(binding); err != nil {
		return err
	}
	if binding.Disabled {
		return errors.New("disabled skill binding cannot be used as an upgrade candidate")
	}
	definition, err := c.definitionFor(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
	if err != nil {
		return err
	}
	if definition == nil {
		return errors.New("skill definition is not registered")
	}
	normalized := cloneBinding(binding)
	normalized.SourceIdentity = DefinitionSourceIdentity(definition)
	return validateBindingAgainstDefinition(normalized, definition)
}

// ValidateDefinitionInput validates credential-free durable inputs against one
// exact action contract without resolving a mutable binding. Scheduled
// Objective and Project references use this during transactional upgrades
// so an incompatible target can never partially advance durable work.
func (c *Catalog) ValidateDefinitionInput(ctx context.Context, id, version, sourceIdentity, actionName string, input map[string]interface{}) error {
	if c == nil {
		return errors.New("skill catalog is not configured")
	}
	definition, err := c.definitionFor(ctx, id, version, sourceIdentity)
	if err != nil {
		return err
	}
	if definition == nil {
		return errors.New("skill definition is not registered")
	}
	if _, ok := definition.Actions[actionName]; !ok {
		return fmt.Errorf("skill action %s does not exist", actionName)
	}
	c.mu.RLock()
	schemas := c.schemas[actionKey(definition.ID, definition.Version, DefinitionSourceIdentity(definition), actionName)]
	c.mu.RUnlock()
	if schemas == nil || schemas.input == nil {
		return errors.New("input schema is not compiled")
	}
	if err := schemas.input.validate(input); err != nil {
		return fmt.Errorf("skill input is invalid: %w", err)
	}
	return nil
}

func (c *Catalog) Bind(ctx context.Context, binding *Binding) error {
	if c == nil {
		return errors.New("skill catalog is not configured")
	}
	if err := validateBindingShape(binding); err != nil {
		return err
	}
	// Disabling is a contraction operation. It must remain possible when an
	// older unqualified binding has become ambiguous after a second publisher
	// variant was installed; requiring definition resolution here would make
	// the unsafe legacy capability impossible to turn off.
	if binding.Disabled {
		return c.disableBinding(ctx, binding)
	}
	definition, err := c.definitionFor(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
	if err != nil {
		return err
	}
	if definition == nil {
		return errors.New("skill definition is not registered")
	}
	normalized := cloneBinding(binding)
	normalized.SourceIdentity = DefinitionSourceIdentity(definition)
	if err := validateBindingAgainstDefinition(normalized, definition); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := bindingKey(normalized.Scope, normalized.DeploymentID, normalized.ID)
	current := c.bindings[key]
	if c.store == nil {
		if (current == nil && normalized.Revision != 1) || (current != nil && normalized.Revision != current.Revision+1) {
			return ErrBindingRevisionConflict
		}
	} else {
		expectedRevision := normalized.Revision - 1
		if err := c.store.SaveSkillBinding(ctx, normalized, expectedRevision); err != nil {
			return err
		}
	}
	c.bindings[key] = normalized
	return nil
}

func (c *Catalog) disableBinding(ctx context.Context, binding *Binding) error {
	current, err := c.currentBindingWithoutDefinition(ctx, binding.Scope, binding.DeploymentID, binding.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return errors.New("cannot create a disabled binding without an existing binding")
	}
	if current.SkillID != binding.SkillID || current.SkillVersion != binding.SkillVersion || current.SourceIdentity != binding.SourceIdentity {
		return errors.New("disabled binding cannot change skill or source identity")
	}
	normalized := cloneBinding(binding)
	key := bindingKey(normalized.Scope, normalized.DeploymentID, normalized.ID)
	if c.store == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		latest := c.bindings[key]
		if latest == nil || normalized.Revision != latest.Revision+1 {
			return ErrBindingRevisionConflict
		}
		c.bindings[key] = normalized
		return nil
	}
	if err := c.store.SaveSkillBinding(ctx, normalized, normalized.Revision-1); err != nil {
		return err
	}
	c.mu.Lock()
	c.bindings[key] = normalized
	c.mu.Unlock()
	return nil
}

func (c *Catalog) currentBindingWithoutDefinition(ctx context.Context, scope ScopeReference, deploymentID, bindingID string) (*Binding, error) {
	if c.store == nil {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return cloneBinding(c.bindings[bindingKey(scope, deploymentID, bindingID)]), nil
	}
	bindings, err := c.store.ListSkillBindings(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	for _, candidate := range bindings {
		if candidate != nil && candidate.ID == bindingID {
			if err := validateBindingManagementShape(candidate); err != nil {
				return nil, fmt.Errorf("stored skill binding is invalid: %w", err)
			}
			return cloneBinding(candidate), nil
		}
	}
	return nil, nil
}

func (c *Catalog) ListModelActions(ctx context.Context, scope ScopeReference, deploymentID string) ([]ModelAction, error) {
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	bindings, err := c.bindingsFor(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	result := make([]ModelAction, 0)
	for _, binding := range bindings {
		if binding.Disabled {
			continue
		}
		definition, err := c.definitionFor(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
		if err != nil {
			return nil, err
		}
		if definition == nil {
			continue
		}
		for _, name := range binding.AllowedActions {
			action := definition.Actions[name]
			result = append(result, ModelAction{
				Name: definition.ID + "." + name, Description: action.Description,
				BindingID: binding.ID, BindingRevision: binding.Revision, DeploymentID: binding.DeploymentID,
				SkillID: definition.ID, Version: definition.Version, Action: name,
				InputSchema: modelVisibleInputSchema(action.InputSchema), SemanticArguments: cloneStringMap(action.SemanticArguments),
				Risk: action.Risk, SideEffect: action.SideEffect, ExternalOperationPolicy: action.ExternalOperationPolicy,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (c *Catalog) ListModelPrompts(ctx context.Context, scope ScopeReference, deploymentID string) ([]ModelPrompt, error) {
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	bindings, err := c.bindingsFor(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	result := make([]ModelPrompt, 0)
	for _, binding := range bindings {
		if binding.Disabled || !binding.EnablePrompt {
			continue
		}
		definition, err := c.definitionFor(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
		if err != nil {
			return nil, err
		}
		if definition == nil || definition.Prompt == nil || NeedsActionAdapter(definition) {
			continue
		}
		if !promptCredentialsSatisfied(definition.Prompt, binding) {
			continue
		}
		result = append(result, ModelPrompt{
			Name: definition.Name, Description: definition.Description, BindingID: binding.ID, BindingRevision: binding.Revision,
			SkillID: definition.ID, Version: definition.Version,
			AlwaysActive: definition.Prompt.AlwaysActive, UserInvocable: definition.Prompt.UserInvocable,
			ModelInvocable: !definition.Prompt.DisableModelInvocation,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (c *Catalog) ResolvePrompt(ctx context.Context, scope ScopeReference, deploymentID, skillID, version string) (*PromptModule, error) {
	return c.resolvePrompt(ctx, scope, deploymentID, skillID, version, nil)
}

func (c *Catalog) ResolveExactPrompt(ctx context.Context, scope ScopeReference, deploymentID, skillID, version string, selected BindingReference) (*PromptModule, error) {
	if strings.TrimSpace(selected.ID) == "" || selected.Revision < 1 {
		return nil, errors.New("an exact binding id and revision are required")
	}
	return c.resolvePrompt(ctx, scope, deploymentID, skillID, version, &selected)
}

func (c *Catalog) resolvePrompt(ctx context.Context, scope ScopeReference, deploymentID, skillID, version string, selected *BindingReference) (*PromptModule, error) {
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	bindings, err := c.bindingsFor(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	var resolved *PromptModule
	for _, binding := range bindings {
		if binding.Disabled || binding.SkillID != skillID || binding.SkillVersion != version || !binding.EnablePrompt {
			continue
		}
		if selected != nil && (binding.ID != selected.ID || binding.Revision != selected.Revision) {
			continue
		}
		definition, err := c.definitionFor(ctx, skillID, version, binding.SourceIdentity)
		if err != nil {
			return nil, err
		}
		if definition != nil && definition.Prompt != nil && !NeedsActionAdapter(definition) && promptCredentialsSatisfied(definition.Prompt, binding) {
			copy := cloneDefinition(definition)
			copy.Prompt.Credentials = nil
			if resolved != nil && selected == nil {
				return nil, ErrBindingAmbiguous
			}
			resolved = copy.Prompt
		}
	}
	if resolved != nil {
		return resolved, nil
	}
	if selected != nil {
		return nil, errors.New("selected skill binding is unavailable or stale")
	}
	return nil, errors.New("bound skill prompt not found")
}

// ResolveConversationAdapter resolves one exact Skill-owned provider adapter.
// The result contains only immutable adapter metadata and opaque credential
// references; a generic host resolves credential values out of band.
func (c *Catalog) ResolveConversationAdapter(ctx context.Context, scope ScopeReference, deploymentID, skillID, version, adapterID string, selected ...BindingReference) (*BoundConversationAdapter, error) {
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" || len(selected) > 1 || (len(selected) == 1 && (strings.TrimSpace(selected[0].ID) == "" || selected[0].Revision < 1)) {
		return nil, errors.New("conversation adapter and at most one exact binding reference are required")
	}
	bindings, err := c.bindingsFor(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	var resolved *BoundConversationAdapter
	for _, binding := range bindings {
		if binding.Disabled || binding.SkillID != skillID || binding.SkillVersion != version ||
			!containsString(binding.EnabledConversationAdapters, adapterID) {
			continue
		}
		if len(selected) == 1 && (binding.ID != selected[0].ID || binding.Revision != selected[0].Revision) {
			continue
		}
		definition, err := c.definitionFor(ctx, skillID, version, binding.SourceIdentity)
		if err != nil {
			return nil, err
		}
		if definition == nil {
			continue
		}
		_, ok := definition.ConversationAdapters[adapterID]
		if !ok || validateBindingAgainstDefinition(binding, definition) != nil {
			continue
		}
		copy := cloneDefinition(definition)
		candidate := &BoundConversationAdapter{
			Definition: copy, AdapterID: adapterID,
			Adapter: copy.ConversationAdapters[adapterID], Binding: cloneBinding(binding),
		}
		if resolved != nil && len(selected) == 0 {
			return nil, ErrBindingAmbiguous
		}
		resolved = candidate
	}
	if resolved != nil {
		return resolved, nil
	}
	if len(selected) == 1 {
		return nil, errors.New("selected conversation adapter binding is unavailable or stale")
	}
	return nil, errors.New("bound conversation adapter not found")
}

// NeedsActionAdapter reports whether an imported OpenClaw instruction module
// declares access to tools or credentials without defining any governed action
// that could consume them. Such a module remains inspectable and exportable,
// but must not be projected as an executable model capability.
func NeedsActionAdapter(definition *Definition) bool {
	if definition == nil || definition.Source == nil || definition.Source.Format != "openclaw.skill.v1" ||
		definition.Prompt == nil || len(definition.Actions) != 0 {
		return false
	}
	return len(definition.Prompt.AllowedTools) > 0 || len(definition.Prompt.Credentials) > 0 || len(definition.Requirements.Environment) > 0
}

func (c *Catalog) Resolve(ctx context.Context, scope ScopeReference, deploymentID, skillID, version, actionName string, selected ...BindingReference) (*BoundAction, error) {
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	if len(selected) > 1 || (len(selected) == 1 && (strings.TrimSpace(selected[0].ID) == "" || selected[0].Revision < 1)) {
		return nil, errors.New("an exact binding id and revision are required")
	}
	bindings, err := c.bindingsFor(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	var resolved *BoundAction
	for _, binding := range bindings {
		if binding.Disabled || binding.SkillID != skillID || binding.SkillVersion != version ||
			!containsString(binding.AllowedActions, actionName) {
			continue
		}
		if len(selected) == 1 && (binding.ID != selected[0].ID || binding.Revision != selected[0].Revision) {
			continue
		}
		definition, err := c.definitionFor(ctx, skillID, version, binding.SourceIdentity)
		if err != nil {
			return nil, err
		}
		if definition == nil {
			continue
		}
		copy := cloneDefinition(definition)
		candidate := &BoundAction{Definition: copy, Action: copy.Actions[actionName], Binding: cloneBinding(binding)}
		if resolved != nil && len(selected) == 0 {
			return nil, ErrBindingAmbiguous
		}
		resolved = candidate
	}
	if resolved != nil {
		return resolved, nil
	}
	if len(selected) == 1 {
		return nil, errors.New("selected skill binding is unavailable or stale")
	}
	return nil, errors.New("bound skill action not found")
}

func (c *Catalog) definitionFor(ctx context.Context, id, version, sourceIdentity string) (*Definition, error) {
	sourceIdentity = strings.TrimSpace(sourceIdentity)
	if c.store == nil {
		c.mu.RLock()
		variants := c.cachedDefinitionVariants(id, version, sourceIdentity)
		c.mu.RUnlock()
		return selectDefinitionVariant(variants, sourceIdentity)
	}
	definitions, err := c.store.ListSkillDefinitionVariants(ctx, id, version)
	if err != nil {
		return nil, err
	}
	validated := make([]*Definition, 0, len(definitions))
	for _, definition := range definitions {
		if definition == nil {
			continue
		}
		if err := validateDefinition(definition); err != nil {
			return nil, fmt.Errorf("stored skill definition %s@%s is invalid: %w", id, version, err)
		}
		compiled, err := compileDefinitionSchemas(definition)
		if err != nil {
			return nil, err
		}
		identity := DefinitionSourceIdentity(definition)
		key := definitionKey(id, version, identity)
		c.mu.Lock()
		if c.skills[key] == nil {
			c.skills[key] = cloneDefinition(definition)
			for name, schemas := range compiled {
				c.schemas[actionKey(id, version, identity, name)] = schemas
			}
		}
		validated = append(validated, cloneDefinition(c.skills[key]))
		c.mu.Unlock()
	}
	return selectDefinitionVariant(validated, sourceIdentity)
}

func (c *Catalog) cachedDefinitionVariants(id, version, sourceIdentity string) []*Definition {
	if sourceIdentity != "" {
		if definition := c.skills[definitionKey(id, version, sourceIdentity)]; definition != nil {
			return []*Definition{cloneDefinition(definition)}
		}
		return nil
	}
	result := make([]*Definition, 0, 1)
	for _, definition := range c.skills {
		if definition.ID == id && definition.Version == version {
			result = append(result, cloneDefinition(definition))
		}
	}
	return result
}

func selectDefinitionVariant(definitions []*Definition, sourceIdentity string) (*Definition, error) {
	if sourceIdentity != "" {
		for _, definition := range definitions {
			if DefinitionSourceIdentity(definition) == sourceIdentity {
				return cloneDefinition(definition), nil
			}
		}
		return nil, nil
	}
	if len(definitions) == 0 {
		return nil, nil
	}
	if len(definitions) > 1 {
		return nil, ErrDefinitionAmbiguous
	}
	return cloneDefinition(definitions[0]), nil
}

func (c *Catalog) bindingsFor(ctx context.Context, scope ScopeReference, deploymentID string) ([]*Binding, error) {
	if c.store == nil {
		c.mu.RLock()
		defer c.mu.RUnlock()
		result := make([]*Binding, 0)
		for _, binding := range c.bindings {
			if binding.Scope == scope && binding.DeploymentID == deploymentID {
				result = append(result, cloneBinding(binding))
			}
		}
		return result, nil
	}
	bindings, err := c.store.ListSkillBindings(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		if err := validateBindingShape(binding); err != nil {
			return nil, fmt.Errorf("stored skill binding is invalid: %w", err)
		}
		if binding.Disabled {
			c.mu.Lock()
			c.bindings[bindingKey(binding.Scope, binding.DeploymentID, binding.ID)] = cloneBinding(binding)
			c.mu.Unlock()
			continue
		}
		definition, err := c.definitionFor(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
		if err != nil {
			return nil, err
		}
		if definition == nil {
			return nil, fmt.Errorf("stored skill binding %s references a missing definition", binding.ID)
		}
		if err := validateBindingAgainstDefinition(binding, definition); err != nil {
			return nil, fmt.Errorf("stored skill binding %s is invalid: %w", binding.ID, err)
		}
		c.mu.Lock()
		c.bindings[bindingKey(binding.Scope, binding.DeploymentID, binding.ID)] = cloneBinding(binding)
		c.mu.Unlock()
	}
	return bindings, nil
}

func compileDefinitionSchemas(definition *Definition) (map[string]*compiledActionSchemas, error) {
	compiled := make(map[string]*compiledActionSchemas, len(definition.Actions))
	for name, action := range definition.Actions {
		schemas, err := compileActionSchemas(definition.ID, definition.Version, action)
		if err != nil {
			return nil, fmt.Errorf("compile %s.%s schemas: %w", definition.ID, name, err)
		}
		compiled[name] = schemas
	}
	return compiled, nil
}

func (c *Catalog) ValidateInput(_ context.Context, bound *BoundAction, input map[string]interface{}) error {
	if bound == nil || bound.Definition == nil || bound.Binding == nil {
		return errors.New("bound skill action is required")
	}
	c.mu.RLock()
	schemas := c.schemas[actionKey(bound.Definition.ID, bound.Definition.Version, DefinitionSourceIdentity(bound.Definition), bound.Action.Name)]
	c.mu.RUnlock()
	if schemas == nil || schemas.input == nil {
		return errors.New("input schema is not compiled")
	}
	if err := schemas.input.validate(input); err != nil {
		return fmt.Errorf("skill input is invalid: %w", err)
	}
	for field, rule := range bound.Binding.ArgumentRestrictions[bound.Action.Name] {
		value, ok := input[field]
		if rule.Const != nil && (!ok || !valuesEqual(value, rule.Const)) {
			return fmt.Errorf("skill input field %s must equal its binding constant", field)
		}
		if len(rule.Enum) > 0 && (!ok || !containsValue(rule.Enum, value)) {
			return fmt.Errorf("skill input field %s is outside its binding allowlist", field)
		}
	}
	return nil
}

func (c *Catalog) ValidateOutput(_ context.Context, bound *BoundAction, output map[string]interface{}) error {
	if bound == nil || bound.Definition == nil || bound.Binding == nil {
		return errors.New("bound skill action is required")
	}
	c.mu.RLock()
	schemas := c.schemas[actionKey(bound.Definition.ID, bound.Definition.Version, DefinitionSourceIdentity(bound.Definition), bound.Action.Name)]
	c.mu.RUnlock()
	if schemas == nil {
		return errors.New("output schema is not compiled")
	}
	if schemas.output == nil {
		return nil
	}
	if err := schemas.output.validate(output); err != nil {
		return fmt.Errorf("skill output is invalid: %w", err)
	}
	return nil
}

// MaterializeTransportArguments applies the canonical deterministic input
// projection for a bound action. Secret credentials remain a separate worker
// input and can never be introduced through this mapping.
func MaterializeTransportArguments(bound *BoundAction, input map[string]interface{}) (map[string]interface{}, error) {
	if bound == nil || bound.Definition == nil {
		return nil, errors.New("bound skill action is required")
	}
	result := make(map[string]interface{})
	transport := bound.Definition.Transport
	if bound.Action.Transport != nil {
		transport = *bound.Action.Transport
	}
	if len(transport.Arguments) == 0 {
		for name, value := range input {
			result[name] = cloneValue(value)
		}
		return result, nil
	}
	for name, mapping := range transport.Arguments {
		if mapping.SourceArgument != "" {
			value, ok := input[mapping.SourceArgument]
			if !ok {
				return nil, fmt.Errorf("transport argument %s requires action input %s", name, mapping.SourceArgument)
			}
			result[name] = value
			continue
		}
		result[name] = cloneValue(mapping.Literal)
	}
	return result, nil
}

func DefinitionSourceIdentity(definition *Definition) string {
	if definition == nil || definition.Source == nil {
		return ""
	}
	return strings.TrimSpace(definition.Source.Identity)
}

func definitionKey(id, version, sourceIdentity string) string {
	return id + "@" + version + "\x00" + sourceIdentity
}
func actionKey(id, version, sourceIdentity, action string) string {
	return definitionKey(id, version, sourceIdentity) + ":" + action
}
func bindingKey(scope ScopeReference, deploymentID, bindingID string) string {
	return scope.Kind + ":" + scope.ID + ":" + deploymentID + ":" + bindingID
}

func validateDefinition(definition *Definition) error {
	if definition == nil || strings.TrimSpace(definition.ID) == "" || strings.TrimSpace(definition.Version) == "" || strings.TrimSpace(definition.Name) == "" {
		return errors.New("skill id, version, and name are required")
	}
	if len(definition.Actions) == 0 && definition.Prompt == nil && len(definition.ConversationAdapters) == 0 {
		return errors.New("skill must declare at least one action, prompt module, or conversation adapter")
	}
	if definition.Category != strings.TrimSpace(definition.Category) || len(definition.Tags) > 32 {
		return errors.New("skill category or tags are invalid")
	}
	seenTags := make(map[string]bool, len(definition.Tags))
	for _, tag := range definition.Tags {
		if tag != strings.TrimSpace(tag) || tag == "" || len(tag) > 64 || seenTags[tag] {
			return errors.New("skill category or tags are invalid")
		}
		seenTags[tag] = true
	}
	if definition.Prompt != nil && strings.TrimSpace(definition.Prompt.Instructions) == "" {
		return errors.New("skill prompt instructions are required")
	}
	if definition.Source != nil {
		if err := validateSourceIdentity(definition.Source.Identity); err != nil {
			return fmt.Errorf("skill source identity is invalid: %w", err)
		}
	}
	if definition.BindingConfigSchema != nil {
		if _, err := compileSchema(definition.ID+"-"+definition.Version+"-binding-config.json", definition.BindingConfigSchema); err != nil {
			return fmt.Errorf("skill binding config schema is invalid: %w", err)
		}
	}
	if err := validateHostingRequirements(definition.Requirements); err != nil {
		return fmt.Errorf("skill hosting requirements are invalid: %w", err)
	}
	if definition.Prompt != nil {
		if err := validateCredentialRequirements(definition.Prompt.Credentials); err != nil {
			return fmt.Errorf("skill prompt has an invalid credential requirement: %w", err)
		}
	}
	for id, adapter := range definition.ConversationAdapters {
		if id == "" || id != strings.TrimSpace(id) || len(id) > 128 {
			return fmt.Errorf("skill conversation adapter id %q is invalid", id)
		}
		normalized, err := capability.NormalizeConversationAdapter(adapter)
		if err != nil {
			return fmt.Errorf("skill conversation adapter %s is invalid: %w", id, err)
		}
		if !reflect.DeepEqual(normalized, adapter) {
			return fmt.Errorf("skill conversation adapter %s must use canonical ordering and values", id)
		}
		for _, discovery := range adapter.DestinationDiscovery {
			action, ok := definition.Actions[discovery.Action]
			if !ok {
				return fmt.Errorf("skill conversation adapter %s destination discovery references missing action %s", id, discovery.Action)
			}
			if action.Risk != RiskLevelRead || (action.SideEffect != SideEffectRead && action.SideEffect != SideEffectNone) {
				return fmt.Errorf("skill conversation adapter %s destination discovery action %s must be read-only", id, discovery.Action)
			}
			adapterCredentials := make(map[string]CredentialRequirement, len(adapter.Credentials))
			for _, requirement := range adapter.Credentials {
				adapterCredentials[requirement.Name] = requirement
			}
			for _, requirement := range action.Credentials {
				declared, exists := adapterCredentials[requirement.Name]
				if !exists || declared.Kind != requirement.Kind || declared.Optional != requirement.Optional || !reflect.DeepEqual(declared.OAuth2, requirement.OAuth2) {
					return fmt.Errorf("skill conversation adapter %s destination discovery action %s credential %s is not declared identically by the adapter", id, discovery.Action, requirement.Name)
				}
			}
			if err := validateConversationDestinationDiscoverySchema(action, discovery); err != nil {
				return fmt.Errorf("skill conversation adapter %s destination discovery action %s is invalid: %w", id, discovery.Action, err)
			}
		}
	}
	for name, action := range definition.Actions {
		if name == "" || action.Name != name || strings.TrimSpace(action.Description) == "" || action.InputSchema == nil {
			return fmt.Errorf("skill action %s is incomplete", name)
		}
		if !validRisk(action.Risk) || !validSideEffect(action.SideEffect) || !validIdempotency(action.Idempotency) || !validExternalOperationPolicy(action.ExternalOperationPolicy) {
			return fmt.Errorf("skill action %s has invalid policy metadata", name)
		}
		if action.ExternalOperationPolicy == ExternalOperationRequired && action.SideEffect != SideEffectExternal {
			return fmt.Errorf("skill action %s requires external side effects to require an external-operation receipt", name)
		}
		if action.SideEffect != SideEffectNone && action.SideEffect != SideEffectRead && action.Idempotency == IdempotencyNone {
			return fmt.Errorf("skill action %s with side effects must support idempotency", name)
		}
		if err := validateCredentialRequirements(action.Credentials); err != nil {
			return fmt.Errorf("skill action %s has an invalid credential requirement: %w", name, err)
		}
		if action.DryRunAction != "" {
			if _, ok := definition.Actions[action.DryRunAction]; !ok {
				return fmt.Errorf("skill action %s references missing dry-run action", name)
			}
		}
		if action.CompensationAction != "" {
			if _, ok := definition.Actions[action.CompensationAction]; !ok {
				return fmt.Errorf("skill action %s references missing compensation action", name)
			}
		}
		transport := definition.Transport
		if action.Transport != nil {
			transport = *action.Transport
		}
		if strings.TrimSpace(transport.Kind) == "" {
			return fmt.Errorf("skill action %s requires an action or definition transport", name)
		}
		properties, _ := action.InputSchema["properties"].(map[string]interface{})
		seenSemanticArguments := make(map[string]bool, len(action.SemanticArguments))
		for role, argument := range action.SemanticArguments {
			if !validSemanticRole(role) || strings.TrimSpace(argument) == "" || seenSemanticArguments[argument] {
				return fmt.Errorf("skill action %s has invalid or duplicate semantic argument %q", name, role)
			}
			if _, ok := properties[argument]; !ok {
				return fmt.Errorf("skill action %s semantic argument %s references unknown input %s", name, role, argument)
			}
			seenSemanticArguments[argument] = true
		}
		for argument, mapping := range transport.Arguments {
			if strings.TrimSpace(argument) == "" || (mapping.SourceArgument == "" && mapping.Literal == nil) || (mapping.SourceArgument != "" && mapping.Literal != nil) {
				return fmt.Errorf("skill action %s has invalid transport argument mapping %q", name, argument)
			}
			if mapping.SourceArgument != "" {
				if _, ok := properties[mapping.SourceArgument]; !ok {
					return fmt.Errorf("skill action %s transport references unknown input %s", name, mapping.SourceArgument)
				}
			}
		}
	}
	return nil
}

var (
	hostingStorageNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	hostingCPUPattern         = regexp.MustCompile(`^[1-9][0-9]*(m)?$`)
	hostingMemoryPattern      = regexp.MustCompile(`^[1-9][0-9]*(Ki|Mi|Gi|Ti)$`)
)

func validateHostingRequirements(requirements Requirements) error {
	seenNames := make(map[string]bool, len(requirements.Storage))
	seenMounts := make(map[string]bool, len(requirements.Storage))
	for _, storage := range requirements.Storage {
		if !hostingStorageNamePattern.MatchString(storage.Name) || seenNames[storage.Name] {
			return fmt.Errorf("storage name %q is invalid or duplicated", storage.Name)
		}
		seenNames[storage.Name] = true
		if storage.MountPath == "" || storage.MountPath == "/" || !strings.HasPrefix(storage.MountPath, "/") ||
			path.Clean(storage.MountPath) != storage.MountPath || seenMounts[storage.MountPath] {
			return fmt.Errorf("storage mount path %q is invalid or duplicated", storage.MountPath)
		}
		seenMounts[storage.MountPath] = true
		if storage.WritableGroup != nil && *storage.WritableGroup <= 0 {
			return fmt.Errorf("storage %s writable group must be positive", storage.Name)
		}
		if storage.MinimumCapacity != "" && !hostingMemoryPattern.MatchString(storage.MinimumCapacity) {
			return fmt.Errorf("storage %s minimum capacity %q is invalid", storage.Name, storage.MinimumCapacity)
		}
		switch storage.Durability {
		case StorageDurabilityPersistent:
			if storage.MinimumCapacity == "" {
				return fmt.Errorf("persistent storage %s requires minimum capacity", storage.Name)
			}
			if storage.Retention != StorageRetentionRetain && storage.Retention != StorageRetentionDelete {
				return fmt.Errorf("persistent storage %s requires an explicit retention policy", storage.Name)
			}
		case StorageDurabilityEphemeral:
			if storage.Retention != "" && storage.Retention != StorageRetentionDelete {
				return fmt.Errorf("ephemeral storage %s cannot be retained", storage.Name)
			}
		default:
			return fmt.Errorf("storage %s durability %q is invalid", storage.Name, storage.Durability)
		}
	}
	if requirements.Compute == nil {
		return nil
	}
	compute := requirements.Compute
	if compute.Requests.CPU == "" && compute.Requests.Memory == "" && compute.Limits.CPU == "" && compute.Limits.Memory == "" {
		return errors.New("compute requirements are empty")
	}
	for label, value := range map[string]string{"requests.cpu": compute.Requests.CPU, "limits.cpu": compute.Limits.CPU} {
		if value != "" && !hostingCPUPattern.MatchString(value) {
			return fmt.Errorf("compute %s %q is invalid", label, value)
		}
	}
	for label, value := range map[string]string{"requests.memory": compute.Requests.Memory, "limits.memory": compute.Limits.Memory} {
		if value != "" && !hostingMemoryPattern.MatchString(value) {
			return fmt.Errorf("compute %s %q is invalid", label, value)
		}
	}
	return nil
}

func validateConversationDestinationDiscoverySchema(action Action, discovery ConversationDestinationDiscovery) error {
	for name, expectedType := range map[string]string{
		discovery.CursorArgument: "string",
		discovery.LimitArgument:  "integer",
		discovery.QueryArgument:  "string",
	} {
		if name == "" {
			continue
		}
		property, ok := topLevelSchemaProperty(action.InputSchema, name)
		if !ok || property["type"] != expectedType {
			return fmt.Errorf("input argument %s must be a declared %s property", name, expectedType)
		}
	}
	items, ok := schemaAtPath(action.OutputSchema, discovery.ItemsPath)
	if !ok || items["type"] != "array" {
		return errors.New("items path must resolve to an output array")
	}
	itemSchema, ok := items["items"].(map[string]interface{})
	if !ok {
		return errors.New("destination items must declare an object schema")
	}
	for path, label := range map[string]string{
		discovery.IDPath:          "id",
		discovery.DisplayNamePath: "display name",
		discovery.DescriptionPath: "description",
	} {
		if path == "" {
			continue
		}
		value, ok := schemaAtPath(itemSchema, path)
		if !ok || value["type"] != "string" {
			return fmt.Errorf("destination %s path must resolve to a string", label)
		}
	}
	if discovery.NextCursorPath != "" {
		value, ok := schemaAtPath(action.OutputSchema, discovery.NextCursorPath)
		if !ok || value["type"] != "string" {
			return errors.New("next cursor path must resolve to an output string")
		}
	}
	return nil
}

func topLevelSchemaProperty(schema map[string]interface{}, name string) (map[string]interface{}, bool) {
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return nil, false
	}
	property, ok := properties[name].(map[string]interface{})
	return property, ok
}

func schemaAtPath(schema map[string]interface{}, path string) (map[string]interface{}, bool) {
	current := schema
	for _, component := range strings.Split(path, ".") {
		property, ok := topLevelSchemaProperty(current, component)
		if !ok {
			return nil, false
		}
		current = property
	}
	return current, true
}

func validateBindingShape(binding *Binding) error {
	if err := validateBindingManagementShape(binding); err != nil {
		return err
	}
	if (len(binding.AllowedActions) == 0 && !binding.EnablePrompt && len(binding.EnabledConversationAdapters) == 0) || !validRisk(binding.MaximumRisk) {
		return fmt.Errorf("%w: binding must enable a prompt or explicitly allow actions or conversation adapters and set maximum risk", ErrBindingInvalid)
	}
	seenAdapters := make(map[string]bool, len(binding.EnabledConversationAdapters))
	for _, adapterID := range binding.EnabledConversationAdapters {
		if adapterID == "" || adapterID != strings.TrimSpace(adapterID) || len(adapterID) > 128 || seenAdapters[adapterID] {
			return fmt.Errorf("%w: enabled conversation adapter ids must be unique and non-empty", ErrBindingInvalid)
		}
		seenAdapters[adapterID] = true
	}
	return nil
}

// validateBindingManagementShape permits an authority-invalid legacy binding
// to remain inspectable and CAS-repairable without permitting it to resolve as
// executable model authority.
func validateBindingManagementShape(binding *Binding) error {
	if binding == nil || strings.TrimSpace(binding.ID) == "" || strings.TrimSpace(binding.SkillID) == "" ||
		strings.TrimSpace(binding.SkillVersion) == "" || binding.Revision < 1 {
		return fmt.Errorf("%w: binding id, skill id, version, and revision are required", ErrBindingInvalid)
	}
	if err := validateScopeAndDeployment(binding.Scope, binding.DeploymentID); err != nil {
		return fmt.Errorf("%w: %v", ErrBindingInvalid, err)
	}
	if err := validateSourceIdentity(binding.SourceIdentity); err != nil {
		return fmt.Errorf("%w: binding source identity is invalid: %v", ErrBindingInvalid, err)
	}
	if err := validateNonSecretConfiguration(binding.Config, ""); err != nil {
		return fmt.Errorf("%w: %v", ErrBindingInvalid, err)
	}
	if err := validateBindingLifecycle(binding); err != nil {
		return fmt.Errorf("%w: %v", ErrBindingInvalid, err)
	}
	return nil
}

// ValidateBindingShape validates the durable, definition-independent shape of
// a Skill binding. Persistence implementations and transactional compilers use
// it to prevent malformed authority from entering storage before an immutable
// Skill definition is resolved.
func ValidateBindingShape(binding *Binding) error {
	return validateBindingShape(binding)
}

func validateBindingLifecycle(binding *Binding) error {
	if len(binding.Lifecycle) == 0 {
		return nil
	}
	if binding.CreatedAt.IsZero() || binding.UpdatedAt.IsZero() || binding.UpdatedAt.Before(binding.CreatedAt) {
		return errors.New("managed binding timestamps are invalid")
	}
	previous := int64(0)
	for _, entry := range binding.Lifecycle {
		if entry.Revision <= previous || entry.Revision > binding.Revision || entry.At.IsZero() ||
			strings.TrimSpace(entry.Actor.Type) == "" || strings.TrimSpace(entry.Actor.ID) == "" || strings.TrimSpace(entry.Reason) == "" {
			return errors.New("binding lifecycle entry is invalid")
		}
		switch entry.Action {
		case BindingLifecycleCreated, BindingLifecycleUpdated, BindingLifecycleEnabled, BindingLifecycleDisabled:
		default:
			return errors.New("binding lifecycle action is invalid")
		}
		previous = entry.Revision
	}
	if previous != binding.Revision {
		return errors.New("binding lifecycle must end at the current revision")
	}
	return nil
}

func validateSourceIdentity(value string) error {
	value = strings.TrimSpace(value)
	if len(value) > 2048 {
		return errors.New("must be at most 2048 bytes")
	}
	for _, character := range value {
		if character == 0 || character < 0x20 || character == 0x7f {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

func validateBindingAgainstDefinition(binding *Binding, definition *Definition) error {
	if definition.BindingConfigSchema == nil {
		if len(binding.Config) != 0 {
			return errors.New("binding config is not declared by the skill")
		}
	} else {
		schema, err := compileSchema(definition.ID+"-"+definition.Version+"-binding-config.json", definition.BindingConfigSchema)
		if err != nil {
			return fmt.Errorf("compile binding config schema: %w", err)
		}
		config := binding.Config
		if config == nil {
			config = map[string]interface{}{}
		}
		if err := schema.validate(config); err != nil {
			return fmt.Errorf("binding config is invalid: %w", err)
		}
	}
	if binding.EnablePrompt && definition.Prompt == nil {
		return errors.New("binding enables a prompt that the skill does not define")
	}
	if binding.EnablePrompt && definition.Prompt != nil {
		for _, requirement := range definition.Prompt.Credentials {
			ref, ok := binding.Credentials[requirement.Name]
			if !ok {
				continue
			}
			if strings.TrimSpace(ref.ID) == "" || ref.Kind != requirement.Kind {
				return fmt.Errorf("binding prompt credential %s must use an opaque reference of kind %s", requirement.Name, requirement.Kind)
			}
		}
	}
	for _, adapterID := range binding.EnabledConversationAdapters {
		adapter, ok := definition.ConversationAdapters[adapterID]
		if !ok {
			return fmt.Errorf("binding conversation adapter %s does not exist", adapterID)
		}
		for _, requirement := range adapter.Credentials {
			ref, exists := binding.Credentials[requirement.Name]
			if requirement.Optional && !exists {
				continue
			}
			if !exists || strings.TrimSpace(ref.ID) == "" || ref.Kind != requirement.Kind {
				return fmt.Errorf("binding conversation adapter %s is missing credential %s of kind %s", adapterID, requirement.Name, requirement.Kind)
			}
		}
	}
	for _, name := range binding.AllowedActions {
		action, ok := definition.Actions[name]
		if !ok {
			return fmt.Errorf("binding action %s does not exist", name)
		}
		if riskRank(action.Risk) > riskRank(binding.MaximumRisk) {
			return fmt.Errorf("binding action %s exceeds maximum risk", name)
		}
		for _, requirement := range action.Credentials {
			ref, ok := binding.Credentials[requirement.Name]
			if requirement.Optional && !ok {
				continue
			}
			if !ok || ref.ID == "" || ref.Kind != requirement.Kind {
				return fmt.Errorf("binding action %s is missing credential %s of kind %s", name, requirement.Name, requirement.Kind)
			}
		}
		for field := range binding.ArgumentRestrictions[name] {
			properties, _ := action.InputSchema["properties"].(map[string]interface{})
			if _, ok := properties[field]; !ok {
				return fmt.Errorf("binding restriction references unknown field %s", field)
			}
		}
	}
	return nil
}

// ValidateBindingConfiguration validates host-owned, non-secret binding
// configuration against the same local-only JSON Schema contract used when a
// durable Skill binding is written. Callers may use this during authoring
// placement without constructing or persisting a binding.
func ValidateBindingConfiguration(schemaValue map[string]interface{}, config map[string]interface{}) error {
	if schemaValue == nil {
		if len(config) != 0 {
			return errors.New("binding config is not declared by the skill")
		}
		return nil
	}
	schema, err := compileSchema("binding-config.json", schemaValue)
	if err != nil {
		return fmt.Errorf("compile binding config schema: %w", err)
	}
	if config == nil {
		config = map[string]interface{}{}
	}
	if err := schema.validate(config); err != nil {
		return fmt.Errorf("binding config is invalid: %w", err)
	}
	return nil
}

func validateCredentialRequirements(requirements []CredentialRequirement) error {
	seen := make(map[string]bool, len(requirements))
	for _, requirement := range requirements {
		name := strings.TrimSpace(requirement.Name)
		if name == "" || strings.TrimSpace(requirement.Kind) == "" || seen[name] {
			return errors.New("credential name and kind must be unique and non-empty")
		}
		normalized, err := capability.NormalizeOAuth2Requirement(requirement.OAuth2)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(normalized, requirement.OAuth2) {
			return errors.New("OAuth 2 credential requirements must use canonical provider, resource, and scope values")
		}
		seen[name] = true
	}
	return nil
}

func promptCredentialsSatisfied(prompt *PromptModule, binding *Binding) bool {
	if prompt == nil || binding == nil {
		return false
	}
	for _, requirement := range prompt.Credentials {
		ref, ok := binding.Credentials[requirement.Name]
		if requirement.Optional && !ok {
			continue
		}
		if !ok || strings.TrimSpace(ref.ID) == "" || ref.Kind != requirement.Kind {
			return false
		}
	}
	return true
}

func validateScopeAndDeployment(scope ScopeReference, deploymentID string) error {
	if strings.TrimSpace(scope.Kind) == "" || strings.TrimSpace(scope.ID) == "" || strings.TrimSpace(deploymentID) == "" {
		return errors.New("scope and deployment id are required")
	}
	return nil
}

func validRisk(value RiskLevel) bool { return riskRank(value) >= 0 }
func riskRank(value RiskLevel) int {
	switch value {
	case RiskLevelRead:
		return 0
	case RiskLevelWrite:
		return 1
	case RiskLevelExternal:
		return 2
	case RiskLevelProduction:
		return 3
	case RiskLevelDestructive:
		return 4
	default:
		return -1
	}
}

func validSideEffect(value SideEffect) bool {
	return value == SideEffectNone || value == SideEffectRead || value == SideEffectWrite || value == SideEffectExternal || value == SideEffectDestructive
}

func validIdempotency(value IdempotencyMode) bool {
	return value == IdempotencyNone || value == IdempotencySupported || value == IdempotencyRequired
}

func validExternalOperationPolicy(value ExternalOperationPolicy) bool {
	return value == "" || value == ExternalOperationForbidden || value == ExternalOperationOptional || value == ExternalOperationRequired
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func validSemanticRole(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}
