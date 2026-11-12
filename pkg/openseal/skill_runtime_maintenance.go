package openseal

import (
	"context"
	"hash"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type (
	SkillRuntimeMaintenance                = models.SkillRuntimeMaintenance
	SkillRuntimeMaintenanceRequest         = models.SkillRuntimeMaintenanceRequest
	SkillRuntimeMaintenanceCompletion      = models.SkillRuntimeMaintenanceCompletion
	SkillRuntimeMaintenanceBindingRevision = models.SkillRuntimeMaintenanceBindingRevision
	SkillRuntimeMaintenanceStore           = models.SkillRuntimeMaintenanceStore
	SkillRuntimeMaintenanceError           = models.SkillRuntimeMaintenanceError
)

var (
	ErrSkillRuntimeMaintenance         = models.ErrSkillRuntimeMaintenance
	ErrSkillRuntimeMaintenanceNotFound = models.ErrSkillRuntimeMaintenanceNotFound
	ErrSkillRuntimeMaintenanceConflict = models.ErrSkillRuntimeMaintenanceConflict
)

func WithSkillRuntimeMaintenance(ctx context.Context, gate *models.SkillRuntimeMaintenance) context.Context {
	return models.WithSkillRuntimeMaintenance(ctx, gate)
}
func SkillRuntimeMaintenanceFromContext(ctx context.Context) (*models.SkillRuntimeMaintenance, bool) {
	return models.SkillRuntimeMaintenanceFromContext(ctx)
}
func ValidateSkillRuntimeMaintenanceContext(ctx context.Context, gate *models.SkillRuntimeMaintenance, now time.Time) error {
	return models.ValidateSkillRuntimeMaintenanceContext(ctx, gate, now)
}
func AddSkillRuntimeMaintenanceBindingDigest(h hash.Hash, b models.SkillRuntimeMaintenanceBindingRevision) error {
	return models.AddSkillRuntimeMaintenanceBindingDigest(h, b)
}

func (e *Engine) skillRuntimeMaintenanceStore() (models.SkillRuntimeMaintenanceStore, error) {
	if e == nil {
		return nil, skillbinding.ErrSkillReferenceUpgradeUnavailable
	}
	store, ok := e.store.(models.SkillRuntimeMaintenanceStore)
	if !ok {
		return nil, skillbinding.ErrSkillReferenceUpgradeUnavailable
	}
	return store, nil
}

func (e *Engine) HasSkillRuntimeMaintenanceUsage(ctx context.Context, scope models.Scope, skillID string) (bool, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return false, err
	}
	return s.HasSkillRuntimeMaintenanceUsage(ctx, scope, skillID)
}

func (e *Engine) WithSkillRuntimeMaintenanceMutation(ctx context.Context, gate *models.SkillRuntimeMaintenance, mutation func(context.Context) error) error {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return err
	}
	return s.WithSkillRuntimeMaintenanceMutation(ctx, gate, mutation)
}

func (e *Engine) AcquireSkillRuntimeMaintenance(ctx context.Context, r models.SkillRuntimeMaintenanceRequest) (*models.SkillRuntimeMaintenance, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return nil, err
	}
	return s.AcquireSkillRuntimeMaintenance(ctx, r)
}
func (e *Engine) GetSkillRuntimeMaintenance(ctx context.Context, scope models.Scope, skillID string) (*models.SkillRuntimeMaintenance, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return nil, err
	}
	return s.GetSkillRuntimeMaintenance(ctx, scope, skillID)
}
func (e *Engine) ListActiveSkillRuntimeMaintenances(ctx context.Context, scope models.Scope) ([]*models.SkillRuntimeMaintenance, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return nil, err
	}
	return s.ListActiveSkillRuntimeMaintenances(ctx, scope)
}
func (e *Engine) CompleteSkillRuntimeMaintenance(ctx context.Context, r models.SkillRuntimeMaintenanceCompletion) (*models.SkillRuntimeMaintenance, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return nil, err
	}
	return s.CompleteSkillRuntimeMaintenance(ctx, r)
}
func (e *Engine) BeginSkillRuntimeMaintenanceRollback(ctx context.Context, r models.SkillRuntimeMaintenanceCompletion) (*models.SkillRuntimeMaintenance, error) {
	s, err := e.skillRuntimeMaintenanceStore()
	if err != nil {
		return nil, err
	}
	return s.BeginSkillRuntimeMaintenanceRollback(ctx, r)
}
func (e *Engine) ListSkillRuntimeMaintenanceBindings(ctx context.Context, scope models.Scope, skillID, afterDeploymentID, afterBindingID string, limit int) ([]*skill.Binding, error) {
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
		return nil, skillbinding.ErrSkillReferenceUpgradeUnavailable
	}
	return e.agents.GetDeployment(ctx, scope, id)
}
