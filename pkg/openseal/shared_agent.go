package openseal

import (
	"context"
	"errors"

	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Shared agents: a whitelisted snapshot of an Agent that others add as their
// own copy. See pkg/runtime/agentbuilder/shared_agent_types.go.
type (
	SharedAgentProfile        = agentbuilder.SharedAgentProfile
	SharedAgentSlot           = agentbuilder.SharedAgentSlot
	SharedAgentSkill          = agentbuilder.SharedAgentSkill
	SharedAgentExportSkill    = agentbuilder.SharedAgentExportSkill
	SharedAgentExcluded       = agentbuilder.SharedAgentExcluded
	SharedAgentRefusal        = agentbuilder.SharedAgentRefusal
	SharedAgentExport         = agentbuilder.SharedAgentExport
	SharedAgentSource         = agentbuilder.SharedAgentSource
	SharedAgentSpec           = agentbuilder.SharedAgentSpec
	SharedAgentInstallRequest = agentbuilder.SharedAgentInstallRequest
	SharedAgentPlannedSkill   = agentbuilder.SharedAgentPlannedSkill
	SharedAgentPlannedSlot    = agentbuilder.SharedAgentPlannedSlot
	SharedAgentInstallPlan    = agentbuilder.SharedAgentInstallPlan
	SharedAgentInstalled      = agentbuilder.SharedAgentInstalled
	SharedAgentUpdateRequest  = agentbuilder.SharedAgentUpdateRequest
	SharedAgentChange         = agentbuilder.SharedAgentChange
	SharedAgentRiskChange     = agentbuilder.SharedAgentRiskChange
	SharedAgentUpdatePreview  = agentbuilder.SharedAgentUpdatePreview
	SharedAgentUpdated        = agentbuilder.SharedAgentUpdated

	SharedAgentSkillUnavailableError = agentbuilder.SharedAgentSkillUnavailableError
)

const (
	SharedAgentProvenanceSource       = agentbuilder.SharedAgentProvenanceSource
	SharedAgentRefusalVaultCredential = agentbuilder.SharedAgentRefusalVaultCredential
	SharedAgentRefusalChannelAdapter  = agentbuilder.SharedAgentRefusalChannelAdapter
	SharedAgentRefusalNotInCatalog    = agentbuilder.SharedAgentRefusalNotInCatalog
	SharedAgentSkillUnavailable       = agentbuilder.SharedAgentSkillUnavailable
	SharedAgentChangeProfile          = agentbuilder.SharedAgentChangeProfile
	SharedAgentChangeSkillAdded       = agentbuilder.SharedAgentChangeSkillAdded
	SharedAgentChangeSkillRemoved     = agentbuilder.SharedAgentChangeSkillRemoved
	SharedAgentChangeSkillChanged     = agentbuilder.SharedAgentChangeSkillChanged
	SharedAgentFieldName              = agentbuilder.SharedAgentFieldName
	SharedAgentFieldPurpose           = agentbuilder.SharedAgentFieldPurpose
	SharedAgentFieldInstructions      = agentbuilder.SharedAgentFieldInstructions
	SharedAgentFieldPersonality       = agentbuilder.SharedAgentFieldPersonality
	SharedAgentFieldPrinciples        = agentbuilder.SharedAgentFieldPrinciples
	SharedAgentFieldSkills            = agentbuilder.SharedAgentFieldSkills
)

var (
	ErrAgentNameTaken           = agentbuilder.ErrAgentNameTaken
	ErrSharedAgentReviewChanged = agentbuilder.ErrSharedAgentReviewChanged
	ErrSharedAgentNotShareable  = agentbuilder.ErrSharedAgentNotShareable
	ErrSharedAgentIsCopy        = agentbuilder.ErrSharedAgentIsCopy
	ErrSharedAgentNotCopy       = agentbuilder.ErrSharedAgentNotCopy

	ErrSharedAgentSkillUnavailable = agentbuilder.ErrSharedAgentSkillUnavailable
)

var errSharedAgentsUnavailable = errors.New("shared agents need the agent builder actions")

func (e *Engine) sharedAgentInstaller() (*agentbuilder.AgentInstaller, error) {
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
