package openseal

import (
	"context"
	"errors"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Shared agents: a whitelisted snapshot of an Agent that others add as their
// own copy. See pkg/runtime/agentbuilder/shared_agent_types.go.
type (
	SharedAgentProfile        = runtime.SharedAgentProfile
	SharedAgentSlot           = runtime.SharedAgentSlot
	SharedAgentSkill          = runtime.SharedAgentSkill
	SharedAgentExportSkill    = runtime.SharedAgentExportSkill
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

	SharedAgentSkillUnavailableError = runtime.SharedAgentSkillUnavailableError
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
	SharedAgentFieldName              = runtime.SharedAgentFieldName
	SharedAgentFieldPurpose           = runtime.SharedAgentFieldPurpose
	SharedAgentFieldInstructions      = runtime.SharedAgentFieldInstructions
	SharedAgentFieldPersonality       = runtime.SharedAgentFieldPersonality
	SharedAgentFieldPrinciples        = runtime.SharedAgentFieldPrinciples
	SharedAgentFieldSkills            = runtime.SharedAgentFieldSkills
)

var (
	ErrAgentNameTaken           = runtime.ErrAgentNameTaken
	ErrSharedAgentReviewChanged = runtime.ErrSharedAgentReviewChanged
	ErrSharedAgentNotShareable  = runtime.ErrSharedAgentNotShareable
	ErrSharedAgentIsCopy        = runtime.ErrSharedAgentIsCopy
	ErrSharedAgentNotCopy       = runtime.ErrSharedAgentNotCopy

	ErrSharedAgentSkillUnavailable = runtime.ErrSharedAgentSkillUnavailable
)

var errSharedAgentsUnavailable = errors.New("shared agents need the agent builder actions")

func (e *Engine) sharedAgentInstaller() (*runtime.AgentInstaller, error) {
	if e == nil || e.agentInstaller == nil {
		return nil, errSharedAgentsUnavailable
	}
	return e.agentInstaller, nil
}

// ExportSharedAgent reads the whitelist snapshot of an Agent in the creator's
// scope. It refuses the platform assistant and copies of shared agents
// (ErrSharedAgentNotShareable). The host authorizes the creator.
func (e *Engine) ExportSharedAgent(ctx context.Context, scope skill.ScopeReference, deploymentID string) (*SharedAgentExport, error) {
	installer, err := e.sharedAgentInstaller()
	if err != nil {
		return nil, err
	}
	return installer.ExportSharedAgent(ctx, Scope{Kind: scope.Kind, ID: scope.ID}, deploymentID)
}

// PlanSharedAgentInstall resolves a shared Agent in the adopter's scope and
// writes nothing. Without an IdempotencyKey it is the Add dialog's preview.
func (e *Engine) PlanSharedAgentInstall(ctx context.Context, request SharedAgentInstallRequest) (*SharedAgentInstallPlan, error) {
	installer, err := e.sharedAgentInstaller()
	if err != nil {
		return nil, err
	}
	return installer.PlanSharedAgentInstall(ctx, request)
}

// InstallSharedAgent creates the copy a PlanSharedAgentInstall plan
// describes, approved by the acting person's Add.
func (e *Engine) InstallSharedAgent(ctx context.Context, plan *SharedAgentInstallPlan) (*SharedAgentInstalled, error) {
	installer, err := e.sharedAgentInstaller()
	if err != nil {
		return nil, err
	}
	return installer.InstallSharedAgent(ctx, plan)
}

// RetireSharedAgentInstall retires a copy whose install the host could not
// finish.
func (e *Engine) RetireSharedAgentInstall(ctx context.Context, scope skill.ScopeReference, deploymentID, actor string) error {
	installer, err := e.sharedAgentInstaller()
	if err != nil {
		return err
	}
	return installer.RetireSharedAgentInstall(ctx, Scope{Kind: scope.Kind, ID: scope.ID}, deploymentID, actor)
}

// PreviewSharedAgentUpdate lists what moving a copy to a newer version
// changes.
func (e *Engine) PreviewSharedAgentUpdate(ctx context.Context, request SharedAgentUpdateRequest) (*SharedAgentUpdatePreview, error) {
	installer, err := e.sharedAgentInstaller()
	if err != nil {
		return nil, err
	}
	return installer.PreviewSharedAgentUpdate(ctx, request)
}

// ApplySharedAgentUpdate moves a copy to a newer version, approved by the
// acting person's Update.
func (e *Engine) ApplySharedAgentUpdate(ctx context.Context, request SharedAgentUpdateRequest) (*SharedAgentUpdated, error) {
	installer, err := e.sharedAgentInstaller()
	if err != nil {
		return nil, err
	}
	return installer.ApplySharedAgentUpdate(ctx, request)
}
