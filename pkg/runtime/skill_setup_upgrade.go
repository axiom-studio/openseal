package runtime

import (
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// RebaseSkillSetupRequestAfterBindingUpgrade accepts only durable canonical
// upgrade provenance. It does not apply an upgrade or complete configuration.
// The host authenticates the caller and verifies current target authority;
// the setup store still applies the returned request with revision CAS.
func RebaseSkillSetupRequestAfterBindingUpgrade(request *SkillSetupRequest, expected int64, binding *skill.Binding) (*SkillSetupRequest, error) {
	if request == nil || request.Status != "pending" || request.Revision != expected || request.Phase != SkillSetupPhaseBindingUpgrade {
		return nil, ErrSkillSetupConflict
	}
	if binding == nil || binding.Disabled || binding.ID != request.BindingID || binding.DeploymentID != request.DeploymentID ||
		binding.Scope.Kind != request.Scope.Kind || binding.Scope.ID != request.Scope.ID || binding.SkillID != request.SkillID ||
		binding.SourceIdentity != request.SourceIdentity || binding.SkillVersion != request.SkillVersion || binding.Revision <= request.BindingRevision {
		return nil, ErrSkillSetupConflict
	}
	version, revision, upgraded := request.BindingVersion, request.BindingRevision, false
	for _, entry := range binding.Lifecycle {
		if entry.Revision <= request.BindingRevision {
			continue
		}
		if entry.Revision <= revision || entry.Revision > binding.Revision {
			return nil, ErrSkillSetupConflict
		}
		revision = entry.Revision
		proof := entry.SkillUpgrade
		if proof == nil {
			continue
		}
		digest, err := hex.DecodeString(strings.TrimPrefix(proof.PlanDigest, "sha256:"))
		if !strings.HasPrefix(proof.PlanDigest, "sha256:") || err != nil || len(digest) != 32 || proof.ExpectedBindingRevision != entry.Revision-1 ||
			proof.From.ID != request.SkillID || proof.To.ID != request.SkillID ||
			proof.From.SourceIdentity != request.SourceIdentity || proof.To.SourceIdentity != request.SourceIdentity ||
			proof.From.Version != version || proof.To.Version == "" || proof.To.Version == version {
			return nil, ErrSkillSetupConflict
		}
		version, upgraded = proof.To.Version, true
	}
	if !upgraded || version != request.SkillVersion {
		return nil, errors.New("the account has no verified canonical upgrade to the requested Skill version")
	}
	rebased := cloneSkillSetupRequest(request)
	rebased.Phase = SkillSetupPhaseConfiguration
	rebased.BindingVersion = binding.SkillVersion
	rebased.BindingRevision = binding.Revision
	if rebased.Kind == "install" {
		rebased.Kind = "configure"
	}
	rebased.Revision++
	rebased.UpdatedAt = time.Now().UTC()
	return rebased, nil
}
