package runtime

import (
	"context"
	"time"
)

func validateSkillUpgradeMaintenance(ctx context.Context, gate *SkillRuntimeMaintenance, application *SkillReferenceUpgradeMutation, now time.Time) error {
	if err := validateSkillRuntimeMaintenanceOwner(ctx, gate, now); err != nil {
		return err
	}
	if gate == nil || !gate.Active {
		return nil
	}
	from, to := application.Plan.From, application.Plan.To
	// Preflight may prove older persisted bindings compatible with the same
	// verified target. The active operation still fixes the exact publisher and
	// destination, including the separately fenced rollback direction.
	if application.Plan.Scope != gate.Scope || from.ID != gate.SkillID || to.ID != gate.SkillID ||
		from.SourceIdentity != gate.SourceIdentity || to.SourceIdentity != gate.SourceIdentity ||
		to.Version != gate.DesiredVersion || (gate.DesiredVersion != gate.ToVersion && gate.DesiredVersion != gate.FromVersion) {
		return ErrSkillRuntimeMaintenanceConflict
	}
	return nil
}
