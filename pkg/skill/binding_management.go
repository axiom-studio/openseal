package skill

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// UpsertBindingRequest is the canonical deployment-scoped assignment
// contract. ExpectedRevision is zero only for creation. Revision, timestamps,
// and lifecycle supplied on Binding are ignored and derived by OpenSeal.
type UpsertBindingRequest struct {
	Binding          *Binding     `json:"binding"`
	ExpectedRevision int64        `json:"expectedRevision"`
	Actor            BindingActor `json:"actor"`
	Reason           string       `json:"reason"`
}

type DisableBindingRequest struct {
	Scope            ScopeReference `json:"scope"`
	DeploymentID     string         `json:"deploymentId"`
	BindingID        string         `json:"bindingId"`
	ExpectedRevision int64          `json:"expectedRevision"`
	Actor            BindingActor   `json:"actor"`
	Reason           string         `json:"reason"`
}

// ListBindings returns every binding for one exact deployment, including
// disabled bindings, so management surfaces never confuse retirement with
// deletion. Definitions are deliberately not required for audit visibility.
func (c *Catalog) ListBindings(ctx context.Context, scope ScopeReference, deploymentID string) ([]*Binding, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	bindings, err := c.listBindingsWithoutDefinitions(ctx, scope, strings.TrimSpace(deploymentID))
	if err != nil {
		return nil, err
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].ID < bindings[j].ID })
	return bindings, nil
}

func (c *Catalog) GetBinding(ctx context.Context, scope ScopeReference, deploymentID, bindingID string) (*Binding, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(bindingID) == "" {
		return nil, errors.New("binding id is required")
	}
	return c.currentBindingWithoutDefinition(ctx, scope, strings.TrimSpace(deploymentID), strings.TrimSpace(bindingID))
}

// UpsertBinding creates or replaces the enabled state of one binding using a
// compare-and-swap revision. Credential values remain out of band: only the
// explicit opaque CredentialReference map is retained.
func (c *Catalog) UpsertBinding(ctx context.Context, request UpsertBindingRequest) (*Binding, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	if request.Binding == nil {
		return nil, errors.New("binding is required")
	}
	if err := validateBindingManagementActor(request.Actor, request.Reason); err != nil {
		return nil, err
	}
	if request.ExpectedRevision < 0 {
		return nil, errors.New("expected revision cannot be negative")
	}
	candidate := cloneBinding(request.Binding)
	if candidate.Disabled {
		return nil, errors.New("disable a binding through DisableBinding")
	}
	current, err := c.GetBinding(ctx, candidate.Scope, candidate.DeploymentID, candidate.ID)
	if err != nil {
		return nil, err
	}
	if current == nil && request.ExpectedRevision != 0 || current != nil && current.Revision != request.ExpectedRevision {
		return nil, ErrBindingRevisionConflict
	}
	now := time.Now().UTC()
	action := BindingLifecycleCreated
	candidate.Revision = 1
	candidate.CreatedAt = now
	candidate.Lifecycle = nil
	if current != nil {
		action = BindingLifecycleUpdated
		if current.Disabled {
			action = BindingLifecycleEnabled
		}
		candidate.Revision = current.Revision + 1
		candidate.CreatedAt = current.CreatedAt
		if candidate.CreatedAt.IsZero() {
			candidate.CreatedAt = now
		}
		candidate.Lifecycle = append([]BindingLifecycleEntry(nil), current.Lifecycle...)
	}
	candidate.UpdatedAt = now
	candidate.Lifecycle = append(candidate.Lifecycle, bindingLifecycleEntry(candidate.Revision, action, request.Actor, request.Reason, now))
	if err := c.Bind(ctx, candidate); err != nil {
		return nil, err
	}
	return cloneBinding(candidate), nil
}

// DisableBinding retires a binding without deleting its credential references
// or audit history. Runtime discovery already excludes disabled bindings.
func (c *Catalog) DisableBinding(ctx context.Context, request DisableBindingRequest) (*Binding, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	if err := validateBindingManagementActor(request.Actor, request.Reason); err != nil {
		return nil, err
	}
	current, err := c.GetBinding(ctx, request.Scope, request.DeploymentID, request.BindingID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrBindingNotFound
	}
	if current.Revision != request.ExpectedRevision {
		return nil, ErrBindingRevisionConflict
	}
	if current.Disabled {
		return nil, ErrBindingAlreadyDisabled
	}
	now := time.Now().UTC()
	updated := cloneBinding(current)
	updated.Disabled = true
	updated.Revision++
	if updated.CreatedAt.IsZero() {
		updated.CreatedAt = now
	}
	updated.UpdatedAt = now
	updated.Lifecycle = append(updated.Lifecycle, bindingLifecycleEntry(updated.Revision, BindingLifecycleDisabled, request.Actor, request.Reason, now))
	if err := c.Bind(ctx, updated); err != nil {
		return nil, err
	}
	return cloneBinding(updated), nil
}

func (c *Catalog) listBindingsWithoutDefinitions(ctx context.Context, scope ScopeReference, deploymentID string) ([]*Binding, error) {
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
	values, err := c.store.ListSkillBindings(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	result := make([]*Binding, 0, len(values))
	for _, binding := range values {
		if err := validateBindingShape(binding); err != nil {
			return nil, err
		}
		result = append(result, cloneBinding(binding))
	}
	return result, nil
}

func validateBindingManagementActor(actor BindingActor, reason string) error {
	if strings.TrimSpace(actor.Type) == "" || strings.TrimSpace(actor.ID) == "" {
		return errors.New("binding actor type and id are required")
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("binding lifecycle reason is required")
	}
	return nil
}

func bindingLifecycleEntry(revision int64, action BindingLifecycleAction, actor BindingActor, reason string, at time.Time) BindingLifecycleEntry {
	return BindingLifecycleEntry{
		Revision: revision, Action: action,
		Actor:  BindingActor{Type: strings.TrimSpace(actor.Type), ID: strings.TrimSpace(actor.ID)},
		Reason: strings.TrimSpace(reason), At: at,
	}
}
