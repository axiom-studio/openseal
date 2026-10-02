package openseal

import (
	"context"
	"hash"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type (
	SkillRuntimeMaintenance                = runtime.SkillRuntimeMaintenance
	SkillRuntimeMaintenanceRequest         = runtime.SkillRuntimeMaintenanceRequest
	SkillRuntimeMaintenanceCompletion      = runtime.SkillRuntimeMaintenanceCompletion
	SkillRuntimeMaintenanceBindingRevision = runtime.SkillRuntimeMaintenanceBindingRevision
	SkillRuntimeMaintenanceStore           = runtime.SkillRuntimeMaintenanceStore
	SkillRuntimeMaintenanceError           = runtime.SkillRuntimeMaintenanceError
)

var (
	ErrSkillRuntimeMaintenance         = runtime.ErrSkillRuntimeMaintenance
	ErrSkillRuntimeMaintenanceNotFound = runtime.ErrSkillRuntimeMaintenanceNotFound
	ErrSkillRuntimeMaintenanceConflict = runtime.ErrSkillRuntimeMaintenanceConflict
)

func WithSkillRuntimeMaintenance(ctx context.Context, gate *runtime.SkillRuntimeMaintenance) context.Context {
	return runtime.WithSkillRuntimeMaintenance(ctx, gate)
}
func SkillRuntimeMaintenanceFromContext(ctx context.Context) (*runtime.SkillRuntimeMaintenance, bool) {
	return runtime.SkillRuntimeMaintenanceFromContext(ctx)
}
func ValidateSkillRuntimeMaintenanceContext(ctx context.Context, gate *runtime.SkillRuntimeMaintenance, now time.Time) error {
	return runtime.ValidateSkillRuntimeMaintenanceContext(ctx, gate, now)
}
func AddSkillRuntimeMaintenanceBindingDigest(h hash.Hash, b runtime.SkillRuntimeMaintenanceBindingRevision) error {
	return runtime.AddSkillRuntimeMaintenanceBindingDigest(h, b)
}

func (e *Engine) skillRuntimeMaintenanceStore() (runtime.SkillRuntimeMaintenanceStore, error) {
	if e == nil {
		return nil, runtime.ErrSkillReferenceUpgradeUnavailable
	}
	store, ok := e.store.(runtime.SkillRuntimeMaintenanceStore)
	if !ok {
		return nil, runtime.ErrSkillReferenceUpgradeUnavailable
	}
	return store, nil
}

func (e *Engine) HasSkillRuntimeMaintenanceUsage(ctx context.Context, scope runtime.Scope, skillID string) (bool, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return false, err
	}
	return s.HasSkillRuntimeMaintenanceUsage(ctx, scope, skillID)
}

func (e *Engine) WithSkillRuntimeMaintenanceMutation(ctx context.Context, gate *runtime.SkillRuntimeMaintenance, mutation func(context.Context) error) error {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return err
	}
	return s.WithSkillRuntimeMaintenanceMutation(ctx, gate, mutation)
}

func (e *Engine) AcquireSkillRuntimeMaintenance(ctx context.Context, r runtime.SkillRuntimeMaintenanceRequest) (*runtime.SkillRuntimeMaintenance, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return nil, err
	}
	return s.AcquireSkillRuntimeMaintenance(ctx, r)
}
func (e *Engine) GetSkillRuntimeMaintenance(ctx context.Context, scope runtime.Scope, skillID string) (*runtime.SkillRuntimeMaintenance, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return nil, err
	}
	return s.GetSkillRuntimeMaintenance(ctx, scope, skillID)
}
func (e *Engine) ListActiveSkillRuntimeMaintenances(ctx context.Context, scope runtime.Scope) ([]*runtime.SkillRuntimeMaintenance, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return nil, err
	}
	return s.ListActiveSkillRuntimeMaintenances(ctx, scope)
}
func (e *Engine) CompleteSkillRuntimeMaintenance(ctx context.Context, r runtime.SkillRuntimeMaintenanceCompletion) (*runtime.SkillRuntimeMaintenance, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return nil, err
	}
	return s.CompleteSkillRuntimeMaintenance(ctx, r)
}
func (e *Engine) BeginSkillRuntimeMaintenanceRollback(ctx context.Context, r runtime.SkillRuntimeMaintenanceCompletion) (*runtime.SkillRuntimeMaintenance, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return nil, err
	}
	return s.BeginSkillRuntimeMaintenanceRollback(ctx, r)
}
func (e *Engine) ListSkillRuntimeMaintenanceBindings(ctx context.Context, scope runtime.Scope, skillID, afterDeploymentID, afterBindingID string, limit int) ([]*skill.Binding, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return nil, err
	}
	return s.ListSkillRuntimeMaintenanceBindings(ctx, scope, skillID, afterDeploymentID, afterBindingID, limit)
}

// GetAgentDeploymentForSkillMaintenance is a metadata-only policy read. It
// never creates a control conversation or performs any chat preparation.
func (e *Engine) GetAgentDeploymentForSkillMaintenance(ctx context.Context, scope skill.ScopeReference, id string) (*kernelagent.AgentDeployment, error) {
	if e == nil || e.agents == nil {
		return nil, runtime.ErrSkillReferenceUpgradeUnavailable
	}
	return e.agents.GetDeployment(ctx, scope, id)
}
