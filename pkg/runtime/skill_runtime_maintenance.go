package runtime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"regexp"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

var (
	ErrSkillRuntimeMaintenance         = errors.New("Skill runtime maintenance is in progress")
	ErrSkillRuntimeMaintenanceNotFound = errors.New("Skill runtime maintenance is not found")
	ErrSkillRuntimeMaintenanceConflict = errors.New("Skill runtime maintenance ownership or intent changed")
)

var maintenanceArtifactDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// SkillRuntimeMaintenance is a durable execution gate for one tenant-owned
// logical Skill. Owner leases govern reconciliation, never dispatch: an expired
// owner leaves Active true until a verified cutover or rollback completes.
type SkillRuntimeMaintenance struct {
	Scope              Scope     `json:"scope"`
	SkillID            string    `json:"skillId"`
	SourceIdentity     string    `json:"sourceIdentity"`
	RuntimeIdentity    string    `json:"runtimeIdentity"`
	OperationID        string    `json:"operationId"`
	FromVersion        string    `json:"fromVersion"`
	ToVersion          string    `json:"toVersion"`
	TargetSourceDigest string    `json:"targetSourceDigest"`
	FromSourceDigest   string    `json:"fromSourceDigest"`
	FromArtifact       string    `json:"fromArtifact"`
	ToArtifact         string    `json:"toArtifact"`
	Owner              string    `json:"owner"`
	Active             bool      `json:"active"`
	Revision           int64     `json:"revision"`
	LeaseExpiresAt     time.Time `json:"leaseExpiresAt"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
	VerifiedVersion    string    `json:"verifiedVersion,omitempty"`
	DesiredVersion     string    `json:"desiredVersion"`
	Phase              string    `json:"phase"`
}

type SkillRuntimeMaintenanceRequest struct {
	Scope                                                        Scope
	SkillID, SourceIdentity, RuntimeIdentity, OperationID        string
	FromVersion, ToVersion, TargetSourceDigest, FromSourceDigest string
	FromArtifact, ToArtifact, Owner                              string
	LeaseDuration                                                time.Duration
	Now                                                          time.Time
	ExpectedBindings                                             []SkillRuntimeMaintenanceBindingRevision
	ExpectedBindingDigest                                        string
}

type SkillRuntimeMaintenanceBindingRevision struct {
	DeploymentID string `json:"deploymentId"`
	BindingID    string `json:"bindingId"`
	Revision     int64  `json:"revision"`
}

type SkillRuntimeMaintenanceCompletion struct {
	Scope                       Scope
	SkillID, OperationID, Owner string
	ExpectedRevision            int64
	VerifiedVersion             string
	Now                         time.Time
}

type SkillRuntimeMaintenanceStore interface {
	AcquireSkillRuntimeMaintenance(context.Context, SkillRuntimeMaintenanceRequest) (*SkillRuntimeMaintenance, error)
	GetSkillRuntimeMaintenance(context.Context, Scope, string) (*SkillRuntimeMaintenance, error)
	ListActiveSkillRuntimeMaintenances(context.Context, Scope) ([]*SkillRuntimeMaintenance, error)
	CompleteSkillRuntimeMaintenance(context.Context, SkillRuntimeMaintenanceCompletion) (*SkillRuntimeMaintenance, error)
	BeginSkillRuntimeMaintenanceRollback(context.Context, SkillRuntimeMaintenanceCompletion) (*SkillRuntimeMaintenance, error)
	ListSkillRuntimeMaintenanceBindings(context.Context, Scope, string, string, string, int) ([]*skill.Binding, error)
	HasSkillRuntimeMaintenanceUsage(context.Context, Scope, string) (bool, error)
	WithSkillRuntimeMaintenanceMutation(context.Context, *SkillRuntimeMaintenance, func(context.Context) error) error
}

type SkillRuntimeMaintenanceError struct{ Maintenance SkillRuntimeMaintenance }

func (e *SkillRuntimeMaintenanceError) Error() string { return ErrSkillRuntimeMaintenance.Error() }
func (e *SkillRuntimeMaintenanceError) Unwrap() error { return ErrSkillRuntimeMaintenance }

type skillRuntimeMaintenanceProofKey struct{}

// WithSkillRuntimeMaintenance carries controller ownership for canonical
// binding migrations. It never exempts an ActionCall from the execution gate.
func WithSkillRuntimeMaintenance(ctx context.Context, gate *SkillRuntimeMaintenance) context.Context {
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, skillRuntimeMaintenanceProofKey{}, *gate)
}

func SkillRuntimeMaintenanceFromContext(ctx context.Context) (*SkillRuntimeMaintenance, bool) {
	gate, ok := ctx.Value(skillRuntimeMaintenanceProofKey{}).(SkillRuntimeMaintenance)
	if !ok {
		return nil, false
	}
	return cloneSkillRuntimeMaintenance(&gate), true
}

func ValidateSkillRuntimeMaintenanceContext(ctx context.Context, gate *SkillRuntimeMaintenance, now time.Time) error {
	if gate == nil || !gate.Active {
		return ErrSkillRuntimeMaintenanceConflict
	}
	return validateSkillRuntimeMaintenanceOwner(ctx, gate, now)
}

func validateSkillRuntimeMaintenanceOwner(ctx context.Context, gate *SkillRuntimeMaintenance, now time.Time) error {
	if gate == nil || !gate.Active {
		return nil
	}
	proof, ok := ctx.Value(skillRuntimeMaintenanceProofKey{}).(SkillRuntimeMaintenance)
	if !ok || !proof.Active || proof.Scope != gate.Scope || proof.SkillID != gate.SkillID || proof.SourceIdentity != gate.SourceIdentity ||
		proof.OperationID != gate.OperationID || proof.Owner != gate.Owner || proof.Revision != gate.Revision ||
		proof.RuntimeIdentity != gate.RuntimeIdentity || proof.FromVersion != gate.FromVersion || proof.ToVersion != gate.ToVersion || proof.FromSourceDigest != gate.FromSourceDigest || proof.TargetSourceDigest != gate.TargetSourceDigest ||
		proof.FromArtifact != gate.FromArtifact || proof.ToArtifact != gate.ToArtifact || proof.DesiredVersion != gate.DesiredVersion || proof.Phase != gate.Phase || !gate.LeaseExpiresAt.After(now) {
		return &SkillRuntimeMaintenanceError{Maintenance: *gate}
	}
	return nil
}

func validateMaintenanceIdentity(scope Scope, skillID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if skillID == "" || skillID != strings.TrimSpace(skillID) || len(skillID) > 256 || strings.ContainsAny(skillID, "\r\n\t") {
		return ErrSkillRuntimeMaintenanceConflict
	}
	return nil
}

func (r SkillRuntimeMaintenanceRequest) Validate() error {
	if err := validateMaintenanceIdentity(r.Scope, r.SkillID); err != nil {
		return err
	}
	for _, v := range []string{r.SourceIdentity, r.RuntimeIdentity, r.OperationID, r.FromVersion, r.ToVersion, r.Owner} {
		if v == "" || v != strings.TrimSpace(v) || len(v) > 2048 || strings.ContainsAny(v, "\r\n\t") {
			return ErrSkillRuntimeMaintenanceConflict
		}
	}
	if r.FromVersion == r.ToVersion || r.Now.IsZero() || r.LeaseDuration <= 0 || r.LeaseDuration > 15*time.Minute || !maintenanceArtifactDigest.MatchString(r.TargetSourceDigest) || !maintenanceArtifactDigest.MatchString(r.FromSourceDigest) {
		return ErrSkillRuntimeMaintenanceConflict
	}
	for _, image := range []string{r.FromArtifact, r.ToArtifact} {
		repository, digest, found := strings.Cut(image, "@")
		if !found || repository == "" || len(image) > 2048 || strings.ContainsAny(repository, " \r\n\t?#\\") || strings.Contains(repository, "://") || !maintenanceArtifactDigest.MatchString(digest) {
			return ErrSkillRuntimeMaintenanceConflict
		}
	}
	seen := make(map[string]bool, len(r.ExpectedBindings))
	if r.ExpectedBindingDigest != "" && (!maintenanceArtifactDigest.MatchString(r.ExpectedBindingDigest) || len(r.ExpectedBindings) > 0) {
		return ErrSkillRuntimeMaintenanceConflict
	}
	for _, b := range r.ExpectedBindings {
		if b.DeploymentID == "" || b.BindingID == "" || len(b.DeploymentID) > 256 || len(b.BindingID) > 256 || b.Revision <= 0 {
			return ErrSkillRuntimeMaintenanceConflict
		}
		key := b.DeploymentID + "\x00" + b.BindingID
		if seen[key] {
			return ErrSkillRuntimeMaintenanceConflict
		}
		seen[key] = true
	}
	return nil
}

func (r SkillRuntimeMaintenanceRequest) matches(g *SkillRuntimeMaintenance) bool {
	return g != nil && r.Scope == g.Scope && r.SkillID == g.SkillID && r.SourceIdentity == g.SourceIdentity &&
		r.RuntimeIdentity == g.RuntimeIdentity && r.OperationID == g.OperationID && r.FromVersion == g.FromVersion && r.ToVersion == g.ToVersion &&
		r.TargetSourceDigest == g.TargetSourceDigest && r.FromSourceDigest == g.FromSourceDigest && r.FromArtifact == g.FromArtifact && r.ToArtifact == g.ToArtifact
}

func nextSkillRuntimeMaintenance(current *SkillRuntimeMaintenance, r SkillRuntimeMaintenanceRequest) (*SkillRuntimeMaintenance, bool, error) {
	if current != nil && current.OperationID == r.OperationID {
		if !r.matches(current) {
			return nil, false, ErrSkillRuntimeMaintenanceConflict
		}
		if !current.Active {
			return cloneSkillRuntimeMaintenance(current), false, nil
		}
		if current.Owner != r.Owner && current.LeaseExpiresAt.After(r.Now) {
			return nil, false, ErrSkillRuntimeMaintenanceConflict
		}
		copy := *current
		copy.Owner, copy.LeaseExpiresAt, copy.UpdatedAt, copy.Revision = r.Owner, r.Now.Add(r.LeaseDuration), r.Now, current.Revision+1
		return &copy, false, nil
	}
	if current != nil && current.Active {
		return nil, false, ErrSkillRuntimeMaintenanceConflict
	}
	revision := int64(1)
	if current != nil {
		revision = current.Revision + 1
	}
	return &SkillRuntimeMaintenance{Scope: r.Scope, SkillID: r.SkillID, SourceIdentity: r.SourceIdentity, RuntimeIdentity: r.RuntimeIdentity,
		OperationID: r.OperationID, FromVersion: r.FromVersion, ToVersion: r.ToVersion, TargetSourceDigest: r.TargetSourceDigest, FromSourceDigest: r.FromSourceDigest, FromArtifact: r.FromArtifact, ToArtifact: r.ToArtifact, DesiredVersion: r.ToVersion, Phase: "upgrading",
		Owner: r.Owner, Active: true, Revision: revision, LeaseExpiresAt: r.Now.Add(r.LeaseDuration), CreatedAt: r.Now, UpdatedAt: r.Now}, true, nil
}

func validateMaintenanceCompletion(g *SkillRuntimeMaintenance, r SkillRuntimeMaintenanceCompletion) error {
	if g == nil || r.Now.IsZero() || r.OperationID != g.OperationID || r.Owner != g.Owner || r.ExpectedRevision != g.Revision ||
		!g.Active || !g.LeaseExpiresAt.After(r.Now) {
		return ErrSkillRuntimeMaintenanceConflict
	}
	return nil
}

func beginMaintenanceRollbackRecord(g *SkillRuntimeMaintenance, r SkillRuntimeMaintenanceCompletion) *SkillRuntimeMaintenance {
	copy := *g
	copy.DesiredVersion, copy.Phase, copy.UpdatedAt, copy.Revision = g.FromVersion, "rolling_back", r.Now, g.Revision+1
	return &copy
}

func completeMaintenanceRecord(g *SkillRuntimeMaintenance, r SkillRuntimeMaintenanceCompletion) *SkillRuntimeMaintenance {
	copy := *g
	copy.Active, copy.VerifiedVersion, copy.UpdatedAt, copy.Revision, copy.Phase = false, r.VerifiedVersion, r.Now, g.Revision+1, "complete"
	return &copy
}

func cloneSkillRuntimeMaintenance(g *SkillRuntimeMaintenance) *SkillRuntimeMaintenance {
	if g == nil {
		return nil
	}
	copy := *g
	return &copy
}

func skillRuntimeMaintenanceLockKey(scope Scope, skillID string) string {
	return fmt.Sprintf("skill-runtime-maintenance:%d:%s:%d:%s:%d:%s", len(scope.Kind), scope.Kind, len(scope.ID), scope.ID, len(skillID), skillID)
}

// AddSkillRuntimeMaintenanceBindingDigest streams one metadata tuple. Callers
// feed rows in deployment-id/binding-id keyset order, then prefix the hex SHA256
// with "sha256:". Acquisition computes exactly the same ordered digest.
func AddSkillRuntimeMaintenanceBindingDigest(h hash.Hash, b SkillRuntimeMaintenanceBindingRevision) error {
	if h == nil || b.DeploymentID == "" || b.BindingID == "" || b.Revision <= 0 {
		return ErrSkillRuntimeMaintenanceConflict
	}
	return json.NewEncoder(h).Encode([]interface{}{b.DeploymentID, b.BindingID, b.Revision})
}

func skillRuntimeMaintenanceBindingDigest(records []SkillRuntimeMaintenanceBindingRevision) string {
	h := sha256.New()
	for _, b := range records {
		_ = AddSkillRuntimeMaintenanceBindingDigest(h, b)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

func logicalSkillRuntimeUsage(ctx context.Context, q skillRuntimeUsageQuerier, scope Scope, skillID, actions, runs string, postgres bool) (bool, error) {
	parameter := func(n int) string {
		if postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	identity := "a.scope_kind=" + parameter(1) + " AND a.scope_id=" + parameter(2) + " AND a.skill_id=" + parameter(3)
	query := "SELECT EXISTS (SELECT 1 FROM " + actions + " a WHERE " + identity + " AND a.status IN (" + unfinishedSkillActionSQL + ") LIMIT 1) OR EXISTS (SELECT 1 FROM " + runs + " r WHERE r.scope_kind=" + parameter(1) + " AND r.scope_id=" + parameter(2) + " AND r.status IN (" + unfinishedSkillRunSQL + ") AND EXISTS (SELECT 1 FROM " + actions + " a WHERE " + identity + " AND a.run_id=r.id AND a.status='succeeded' LIMIT 1) LIMIT 1)"
	args := []interface{}{scope.Kind, scope.ID, skillID}
	if !postgres {
		args = append(args, scope.Kind, scope.ID, scope.Kind, scope.ID, skillID)
	}
	query += " OR EXISTS (SELECT 1 FROM " + skillRuntimeDependencyTable(runs) + " d WHERE d.scope_kind=" + parameter(1) + " AND d.scope_id=" + parameter(2) + " AND d.skill_id=" + parameter(3) + " LIMIT 1)"
	if !postgres {
		args = append(args, scope.Kind, scope.ID, skillID)
	}
	var busy bool
	err := q.QueryRowContext(ctx, query, args...).Scan(&busy)
	return busy, err
}

func maintenanceBindingsMatch(ctx context.Context, q skillRuntimeUsageQuerier, g *SkillRuntimeMaintenance, version, bindings string, postgres bool) (bool, error) {
	parameter := func(n int) string {
		if postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	enabled := "COALESCE(json_extract(payload,'$.disabled'),0)=0"
	if postgres {
		enabled = "COALESCE((payload->>'disabled')::boolean,false)=false"
	}
	query := "SELECT NOT EXISTS (SELECT 1 FROM " + bindings + " WHERE scope_kind=" + parameter(1) + " AND scope_id=" + parameter(2) + " AND skill_id=" + parameter(3) + " AND " + enabled + " AND (skill_version<>" + parameter(4) + " OR source_identity<>" + parameter(5) + ") LIMIT 1)"
	var matched bool
	err := q.QueryRowContext(ctx, query, g.Scope.Kind, g.Scope.ID, g.SkillID, version, g.SourceIdentity).Scan(&matched)
	return matched, err
}

func validateMaintenanceBindingPage(scope Scope, skillID, afterDeploymentID, afterBindingID string, limit int) error {
	if err := validateMaintenanceIdentity(scope, skillID); err != nil {
		return err
	}
	if limit <= 0 || limit > 100 || len(afterDeploymentID) > 256 || len(afterBindingID) > 256 || strings.ContainsAny(afterDeploymentID+afterBindingID, "\r\n\t") {
		return ErrSkillRuntimeMaintenanceConflict
	}
	return nil
}

var _ skillRuntimeUsageQuerier = (*sql.Conn)(nil)
