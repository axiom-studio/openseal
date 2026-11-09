package openseal

import "github.com/axiom-studio/openseal/pkg/runtime"

// Shared agents: a whitelisted snapshot of an Agent that others add as their
// own copy. See pkg/runtime/shared_agent_types.go.
type (
	SharedAgentProfile        = runtime.SharedAgentProfile
	SharedAgentSlot           = runtime.SharedAgentSlot
	SharedAgentSkill          = runtime.SharedAgentSkill
	SharedAgentExcluded       = runtime.SharedAgentExcluded
	SharedAgentRefusal        = runtime.SharedAgentRefusal
	SharedAgentExport         = runtime.SharedAgentExport
	SharedAgentSource         = runtime.SharedAgentSource
	SharedAgentSpec           = runtime.SharedAgentSpec
	SharedAgentInstallRequest = runtime.SharedAgentInstallRequest
	SharedAgentPlannedSkill   = runtime.SharedAgentPlannedSkill
	SharedAgentPlannedSlot    = runtime.SharedAgentPlannedSlot
	SharedAgentInstallPlan    = runtime.SharedAgentInstallPlan
	SharedAgentInstalled      = runtime.SharedAgentInstalled
	SharedAgentUpdateRequest  = runtime.SharedAgentUpdateRequest
	SharedAgentChange         = runtime.SharedAgentChange
	SharedAgentRiskChange     = runtime.SharedAgentRiskChange
	SharedAgentUpdatePreview  = runtime.SharedAgentUpdatePreview
	SharedAgentUpdated        = runtime.SharedAgentUpdated
)

const (
	SharedAgentProvenanceSource       = runtime.SharedAgentProvenanceSource
	SharedAgentRefusalVaultCredential = runtime.SharedAgentRefusalVaultCredential
	SharedAgentRefusalChannelAdapter  = runtime.SharedAgentRefusalChannelAdapter
	SharedAgentRefusalNotInCatalog    = runtime.SharedAgentRefusalNotInCatalog
	SharedAgentSkillUnavailable       = runtime.SharedAgentSkillUnavailable
	SharedAgentChangeProfile          = runtime.SharedAgentChangeProfile
	SharedAgentChangeSkillAdded       = runtime.SharedAgentChangeSkillAdded
	SharedAgentChangeSkillRemoved     = runtime.SharedAgentChangeSkillRemoved
	SharedAgentChangeSkillChanged     = runtime.SharedAgentChangeSkillChanged
)

var (
	ErrAgentNameTaken           = runtime.ErrAgentNameTaken
	ErrSharedAgentReviewChanged = runtime.ErrSharedAgentReviewChanged
	ErrSharedAgentNotShareable  = runtime.ErrSharedAgentNotShareable
)
