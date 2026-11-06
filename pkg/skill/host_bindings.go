package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// RunActor is the run a deployment's Skills are activated or resolved for.
// A host maps it to the principal the run acts for (for example the user who
// sent the chat message, or who asked for a routine) and to the umbrella whose
// connections the run uses.
type RunActor struct {
	RunID string `json:"runId"`
	// AgentID is the acting member: the run's assigned Agent deployment. It
	// is the audience of a credential, never the owner of one.
	AgentID string `json:"agentId"`
	// Umbrella holds the run's connections. Host bindings are provided only
	// for the umbrella's own deployment: the Agent in a 1:1 chat, the Team in
	// a Team chat, task or hand-off. A Team member's own host bindings never
	// reach a Team run, and a Team's never reach a 1:1 chat.
	Umbrella Umbrella `json:"umbrella"`
	// ActingUser is the human the run acts for (the run's ActingUser), or
	// empty when no person asked for it. Hosts select per-person sign-ins by
	// it. It is optional: Valid does not require it.
	ActingUser string `json:"actingUser,omitempty"`
}

// Valid reports whether actor names a run, its acting Agent and an umbrella.
// An Agent umbrella is always the acting Agent itself.
func (a RunActor) Valid() bool {
	return a.RunID != "" && a.AgentID != "" && a.Umbrella.Valid() && (a.Umbrella.Kind != UmbrellaAgent || a.Umbrella.ID == a.AgentID)
}

// HostBindingSet is the bindings a host supplies for one run. They are not
// stored: they are validated like stored bindings at every activation and
// resolution, and appear next to the deployment's stored bindings.
type HostBindingSet struct {
	Bindings []*Binding
	// Fingerprint changes whenever any binding in the set changes or the
	// set itself changes; it keys cached activations together with each
	// binding's ID and revision.
	Fingerprint string
	// Services names, per binding ID, the services a binding stands for (a
	// connected app's catalog slug and display name). Setup of a Skill for
	// one of these services is refused in favour of the binding's tools.
	Services map[string][]string
	// PersonalApps names, by app key, the per-person apps of the umbrella
	// that the acting user has not connected yet (app key -> display name).
	PersonalApps map[string]string
}

// HostBindingProvider supplies host-owned bindings for one run of one
// umbrella deployment, for example the MCP apps and accounts connected to an
// Agent or a Team. It is called at every activation and action resolution
// that carries a RunActor, so it must be cheap. deploymentID is always the
// actor's Umbrella.ID.
type HostBindingProvider func(ctx context.Context, scope ScopeReference, deploymentID string, actor RunActor) (*HostBindingSet, error)

type runActorKey struct{}

// WithRunActor marks ctx as acting for actor, so activation and resolution
// through ctx include the host's bindings for that run. Only the kernel's
// trusted run paths and hosts redeeming an exact run's authority set it.
func WithRunActor(ctx context.Context, actor RunActor) context.Context {
	actor.RunID, actor.AgentID, actor.ActingUser = strings.TrimSpace(actor.RunID), strings.TrimSpace(actor.AgentID), strings.TrimSpace(actor.ActingUser)
	return context.WithValue(ctx, runActorKey{}, actor)
}

// RunActorFromContext returns the actor set by WithRunActor. It requires the
// run, the acting Agent and the umbrella.
func RunActorFromContext(ctx context.Context) (RunActor, bool) {
	actor, ok := ctx.Value(runActorKey{}).(RunActor)
	return actor, ok && actor.Valid()
}

type withoutConnectionsKey struct{}

// WithoutConnections marks ctx so activation leaves out every binding that
// holds a connection (see BindingHoldsConnection) and attaches no host
// bindings. A Team run activates its member's own Skills this way.
func WithoutConnections(ctx context.Context) context.Context {
	return context.WithValue(ctx, withoutConnectionsKey{}, true)
}

// ConnectionsExcluded reports whether ctx was marked by WithoutConnections.
func ConnectionsExcluded(ctx context.Context) bool {
	excluded, _ := ctx.Value(withoutConnectionsKey{}).(bool)
	return excluded
}

// SetHostBindingProvider installs (or with nil removes) the host binding
// provider. Hosts set it once at startup.
func (c *Catalog) SetHostBindingProvider(provider HostBindingProvider) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.hostBindings = provider
	c.mu.Unlock()
}

// hostBindings is one validated provider answer.
type hostBindings struct {
	bindings    []*Binding
	definitions map[string]*Definition
	ids         map[string]bool
	services    map[string][]string
	// personalApps is the validated HostBindingSet.PersonalApps.
	personalApps map[string]string
	// key identifies the set for the activation cache.
	key string
}

func (h *hostBindings) has(id string) bool { return h != nil && h.ids[id] }

// HostBindings returns the validated host bindings for actor's run of
// deploymentID, or an empty set when the host supplies none.
func (c *Catalog) HostBindings(ctx context.Context, scope ScopeReference, deploymentID string, actor RunActor) (*HostBindingSet, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	hosted, err := c.hostBindingsFor(WithRunActor(ctx, actor), scope, strings.TrimSpace(deploymentID))
	if err != nil || hosted == nil {
		return &HostBindingSet{}, err
	}
	result := &HostBindingSet{Fingerprint: hosted.key, Services: make(map[string][]string, len(hosted.services))}
	if len(hosted.personalApps) > 0 {
		result.PersonalApps = make(map[string]string, len(hosted.personalApps))
		for key, name := range hosted.personalApps {
			result.PersonalApps[key] = name
		}
	}
	for _, binding := range hosted.bindings {
		result.Bindings = append(result.Bindings, cloneBinding(binding))
	}
	for id, names := range hosted.services {
		result.Services[id] = append([]string(nil), names...)
	}
	return result, nil
}

// GetHostBinding returns the host binding bindingID of the run ctx acts for
// (see WithRunActor), or nil when the host does not supply it to that run.
func (c *Catalog) GetHostBinding(ctx context.Context, scope ScopeReference, deploymentID, bindingID string) (*Binding, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	hosted, err := c.hostBindingsFor(ctx, scope, strings.TrimSpace(deploymentID))
	if err != nil || hosted == nil {
		return nil, err
	}
	for _, binding := range hosted.bindings {
		if binding.ID == strings.TrimSpace(bindingID) {
			return cloneBinding(binding), nil
		}
	}
	return nil, nil
}

// hostBindingsFor asks the provider for the bindings of the run ctx acts
// for and validates them. It returns nil without a provider, without an
// actor, under WithoutConnections, or for a deployment other than the
// actor's umbrella.
func (c *Catalog) hostBindingsFor(ctx context.Context, scope ScopeReference, deploymentID string) (*hostBindings, error) {
	c.mu.RLock()
	provider := c.hostBindings
	c.mu.RUnlock()
	actor, ok := RunActorFromContext(ctx)
	if provider == nil || !ok || actor.Umbrella.ID != deploymentID || ConnectionsExcluded(ctx) {
		return nil, nil
	}
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	set, err := provider(ctx, scope, deploymentID, actor)
	if err != nil {
		return nil, fmt.Errorf("host Skill bindings: %w", err)
	}
	if set == nil || (len(set.Bindings) == 0 && len(set.PersonalApps) == 0) {
		return nil, nil
	}
	personalApps, err := validPersonalApps(set.PersonalApps)
	if err != nil {
		return nil, err
	}
	result := &hostBindings{
		bindings: make([]*Binding, 0, len(set.Bindings)), definitions: make(map[string]*Definition),
		ids: make(map[string]bool, len(set.Bindings)), services: make(map[string][]string),
		personalApps: personalApps,
	}
	hash := sha256.New()
	hash.Write([]byte(set.Fingerprint))
	appKeys := make([]string, 0, len(personalApps))
	for key := range personalApps {
		appKeys = append(appKeys, key)
	}
	sort.Strings(appKeys)
	for _, key := range appKeys {
		hash.Write([]byte{0})
		hash.Write([]byte(key + "\x00" + personalApps[key]))
	}
	for _, candidate := range set.Bindings {
		binding, err := c.validHostBinding(ctx, scope, deploymentID, candidate)
		if err != nil {
			return nil, err
		}
		if result.ids[binding.ID] {
			return nil, fmt.Errorf("%w: host binding %s is supplied twice", ErrBindingInvalid, binding.ID)
		}
		if err := validateDeploymentBindingSlugs(binding, result.bindings); err != nil {
			return nil, err
		}
		key := definitionKey(binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
		if result.definitions[key] == nil {
			definition, err := c.definitionFor(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
			if err != nil {
				return nil, err
			}
			result.definitions[key] = definition
		}
		result.ids[binding.ID] = true
		result.bindings = append(result.bindings, binding)
		for _, name := range set.Services[binding.ID] {
			if name = strings.TrimSpace(name); name != "" {
				result.services[binding.ID] = append(result.services[binding.ID], name)
			}
		}
		for _, part := range []string{binding.ID, strconv.FormatInt(binding.Revision, 10), key} {
			hash.Write([]byte{0})
			hash.Write([]byte(part))
		}
	}
	sort.Slice(result.bindings, func(i, j int) bool { return result.bindings[i].ID < result.bindings[j].ID })
	result.key = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}

// validPersonalApps returns a trimmed copy of a host's per-person app names.
// Every app needs a bounded key and a display name.
func validPersonalApps(apps map[string]string) (map[string]string, error) {
	if len(apps) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(apps))
	for key, name := range apps {
		key, name = strings.TrimSpace(key), strings.TrimSpace(name)
		if key == "" || len(key) > 256 || name == "" {
			return nil, fmt.Errorf("%w: host personal app needs a key of at most 256 bytes and a name", ErrBindingInvalid)
		}
		if _, ok := result[key]; ok {
			return nil, fmt.Errorf("%w: host personal app %s is supplied twice", ErrBindingInvalid, key)
		}
		result[key] = name
	}
	return result, nil
}

// validHostBinding returns a caller-owned copy of one provided binding after
// the same checks a stored binding passes. Validation against the definition
// is cached by the binding's exact content.
func (c *Catalog) validHostBinding(ctx context.Context, scope ScopeReference, deploymentID string, candidate *Binding) (*Binding, error) {
	if candidate == nil {
		return nil, fmt.Errorf("%w: host binding is empty", ErrBindingInvalid)
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return nil, fmt.Errorf("%w: host binding cannot be encoded", ErrBindingInvalid)
	}
	var binding Binding
	if err := json.Unmarshal(encoded, &binding); err != nil {
		return nil, fmt.Errorf("%w: host binding cannot be decoded", ErrBindingInvalid)
	}
	if binding.Scope != scope || binding.DeploymentID != deploymentID || binding.Disabled || len(binding.Lifecycle) != 0 {
		return nil, fmt.Errorf("%w: host binding %s must be an enabled binding of this deployment without stored lifecycle", ErrBindingInvalid, binding.ID)
	}
	digest := sha256.Sum256(encoded)
	validatedKey := "host\x00" + hex.EncodeToString(digest[:])
	if _, ok := c.validated.get(validatedKey); ok {
		return &binding, nil
	}
	if err := validateBindingShape(&binding); err != nil {
		return nil, fmt.Errorf("host skill binding is invalid: %w", err)
	}
	definition, err := c.definitionFor(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
	if err != nil {
		return nil, err
	}
	if definition == nil {
		return nil, fmt.Errorf("host skill binding %s references a missing definition", binding.ID)
	}
	if err := validateBindingAgainstDefinition(&binding, definition); err != nil {
		return nil, fmt.Errorf("host skill binding %s is invalid: %w", binding.ID, err)
	}
	c.validated.put(validatedKey, struct{}{})
	return &binding, nil
}

// withHostBindings adds host bindings to a deployment's stored bindings. A
// host binding may not reuse a stored binding's ID or model-name slug.
func withHostBindings(stored []*Binding, hosted *hostBindings) ([]*Binding, error) {
	if hosted == nil {
		return stored, nil
	}
	for _, binding := range stored {
		if binding != nil && hosted.ids[binding.ID] {
			return nil, fmt.Errorf("%w: host binding %s collides with a stored binding", ErrBindingAmbiguous, binding.ID)
		}
	}
	for _, binding := range hosted.bindings {
		if err := validateDeploymentBindingSlugs(binding, stored); err != nil {
			return nil, err
		}
	}
	merged := make([]*Binding, 0, len(stored)+len(hosted.bindings))
	merged = append(merged, stored...)
	return append(merged, hosted.bindings...), nil
}
