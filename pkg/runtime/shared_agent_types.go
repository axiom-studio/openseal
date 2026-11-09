package runtime

import (
	"errors"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Shared agents: a person publishes a whitelisted snapshot of an Agent and
// others add their own copy of it. The snapshot crosses tenants only through
// the host; the kernel exports it in the creator's scope and installs or
// updates a copy in the adopter's scope.

// SharedAgentProvenanceSource is the definition provenance source of a copy.
// Its Reference is "<shareId>@<version>". It is informational only: the host
// holds the lineage.
const SharedAgentProvenanceSource = "shared-agent"

// Reasons an Agent's skill keeps it from being shared.
const (
	SharedAgentRefusalVaultCredential = "vault_credential"
	SharedAgentRefusalChannelAdapter  = "channel_adapter"
	SharedAgentRefusalNotInCatalog    = "not_in_catalog"
)

// Reasons a shared skill can't be installed in the adopter's scope.
const (
	SharedAgentSkillUnavailable = "skill_unavailable"
)

var (
	// ErrAgentNameTaken is what a create or install returns when another
	// Agent or Team in the scope uses the name.
	ErrAgentNameTaken = errors.New("agent name is already used by another Agent or Team")
	// ErrSharedAgentReviewChanged is returned when the recomputed ceiling
	// differs from the one the person reviewed.
	ErrSharedAgentReviewChanged = errors.New("the reviewed agent changed; review it again")
	// ErrSharedAgentNotShareable is returned for an Agent that can't be
	// exported, such as the platform assistant.
	ErrSharedAgentNotShareable = errors.New("this agent can't be shared")
)

// SharedAgentProfile is the shared part of an Agent's profile. Its
// SystemPrompt is copied and the adopter always reads it before adding.
type SharedAgentProfile struct {
	DisplayName         string   `json:"displayName"`
	Purpose             string   `json:"purpose"`
	SystemPrompt        string   `json:"systemPrompt"`
	Personality         string   `json:"personality,omitempty"`
	OperatingPrinciples []string `json:"operatingPrinciples,omitempty"`
}

// SharedAgentSlot is one credential slot of an account skill. Only the
// provider is shared, never an account or credential.
type SharedAgentSlot struct {
	Slot         string `json:"slot"`
	Provider     string `json:"provider,omitempty"`
	ProviderName string `json:"providerName,omitempty"`
}

// SharedAgentSkill is one skill of a shared Agent by exact catalog identity.
// It carries no Config, credentials or risk: the risk is recomputed from the
// adopter's catalog.
type SharedAgentSkill struct {
	SkillID        string            `json:"skillId"`
	SkillVersion   string            `json:"skillVersion"`
	SourceIdentity string            `json:"sourceIdentity,omitempty"`
	Name           string            `json:"name,omitempty"`
	AllowedActions []string          `json:"allowedActions"`
	EnablePrompt   bool              `json:"enablePrompt"`
	Slots          []SharedAgentSlot `json:"slots,omitempty"`
}

// SharedAgentExcluded counts what stays with the source Agent.
type SharedAgentExcluded struct {
	Channels  int `json:"channels"`
	Routines  int `json:"routines"`
	Approvals int `json:"approvals"`
}

// SharedAgentRefusal names a skill that keeps the Agent from being shared.
type SharedAgentRefusal struct {
	SkillID string `json:"skillId"`
	Name    string `json:"name"`
	Reason  string `json:"reason"`
}

// SharedAgentExport is the whitelist snapshot of an Agent, built field by
// field. Apps are added by the host.
type SharedAgentExport struct {
	Profile           SharedAgentProfile   `json:"profile"`
	Grants            []SharedAgentSkill   `json:"grants"`
	AccountSkills     []SharedAgentSkill   `json:"accountSkills"`
	Excluded          SharedAgentExcluded  `json:"excluded"`
	DefinitionVersion string               `json:"definitionVersion"`
	Refusals          []SharedAgentRefusal `json:"refusals"`
}

// SharedAgentSource is the published version a copy comes from.
type SharedAgentSource struct {
	ShareID string `json:"shareId"`
	Version int    `json:"version"`
}

// SharedAgentInstallRequest installs a copy in the adopter's scope.
// IdempotencyKey derives the copy's ids, so a replay returns the same Agent.
type SharedAgentInstallRequest struct {
	Scope               Scope              `json:"scope"`
	ActingUserID        string             `json:"actingUserId"`
	IdempotencyKey      string             `json:"idempotencyKey"`
	Source              SharedAgentSource  `json:"source"`
	Profile             SharedAgentProfile `json:"profile"`
	SpaceID             string             `json:"spaceId"`
	DisplayName         string             `json:"displayName,omitempty"` // "" = Profile.DisplayName
	Grants              []SharedAgentSkill `json:"grants"`
	AccountSkills       []SharedAgentSkill `json:"accountSkills"`
	ExpectedMaximumRisk skill.RiskLevel    `json:"expectedMaximumRisk,omitempty"`
}

// SharedAgentPlannedSkill is one skill of an install plan as the adopter's
// catalog resolves it.
type SharedAgentPlannedSkill struct {
	SkillID   string          `json:"skillId"`
	Name      string          `json:"name"`
	Risk      skill.RiskLevel `json:"risk"`
	Available bool            `json:"available"`
	Reason    string          `json:"reason,omitempty"`
}

// SharedAgentPlannedSlot is one account slot the copy needs.
type SharedAgentPlannedSlot struct {
	SkillID       string `json:"skillId"`
	Slot          string `json:"slot"`
	Provider      string `json:"provider,omitempty"`
	PersonAllowed bool   `json:"personAllowed"`
	BindingID     string `json:"bindingId"`
}

// SharedAgentInstallPlan is a resolved install: nothing is written yet. The
// host may replace a per_person credential reference in Bindings with the
// adopter's own vault reference before InstallSharedAgent.
type SharedAgentInstallPlan struct {
	Request      SharedAgentInstallRequest `json:"request"`
	DefinitionID string                    `json:"definitionId"`
	DeploymentID string                    `json:"deploymentId"`
	DisplayName  string                    `json:"displayName"`
	MaximumRisk  skill.RiskLevel           `json:"maximumRisk"`
	Skills       []SharedAgentPlannedSkill `json:"skills"`
	Slots        []SharedAgentPlannedSlot  `json:"slots"`
	// Bindings are the candidate bindings of every grant and account skill,
	// account skills with their credential references.
	Bindings []*skill.Binding `json:"bindings"`
}

// SharedAgentInstalled is the installed copy.
type SharedAgentInstalled struct {
	AgentID        string                            `json:"agentId"`
	DisplayName    string                            `json:"displayName"`
	Space          *Space                            `json:"space"`
	ConversationID string                            `json:"conversationId"`
	Activation     *kernelagent.DefinitionActivation `json:"activation,omitempty"`
	Bindings       []*skill.Binding                  `json:"bindings"`
	Unconnected    []SharedAgentPlannedSlot          `json:"unconnected"`
	Replayed       bool                              `json:"replayed"`
}

// SharedAgentUpdateRequest moves a copy from one published version to
// another: fields where From and To differ take To's value.
type SharedAgentUpdateRequest struct {
	Scope                      Scope             `json:"scope"`
	DeploymentID               string            `json:"deploymentId"`
	ActingUserID               string            `json:"actingUserId"`
	IdempotencyKey             string            `json:"idempotencyKey,omitempty"`
	Source                     SharedAgentSource `json:"source"`
	From                       SharedAgentSpec   `json:"fromSpec"`
	To                         SharedAgentSpec   `json:"toSpec"`
	ExpectedDeploymentRevision int64             `json:"expectedDeploymentRevision,omitempty"`
	ExpectedMaximumRisk        skill.RiskLevel   `json:"expectedMaximumRisk,omitempty"`
}

// SharedAgentSpec is the kernel's view of one published version.
type SharedAgentSpec struct {
	Profile       SharedAgentProfile `json:"profile"`
	Grants        []SharedAgentSkill `json:"grants"`
	AccountSkills []SharedAgentSkill `json:"accountSkills"`
}

// Kinds of a SharedAgentChange.
const (
	SharedAgentChangeProfile      = "profile"
	SharedAgentChangeSkillAdded   = "skill_added"
	SharedAgentChangeSkillRemoved = "skill_removed"
	SharedAgentChangeSkillChanged = "skill_changed"
)

// SharedAgentChange is one difference between two published versions.
// Before and After are filled only for the instructions.
type SharedAgentChange struct {
	Field  string `json:"field"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Target string `json:"target,omitempty"`
	RiskUp bool   `json:"riskUp,omitempty"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// SharedAgentRiskChange is the copy's ceiling before and after an update.
type SharedAgentRiskChange struct {
	From skill.RiskLevel `json:"from"`
	To   skill.RiskLevel `json:"to"`
}

// SharedAgentUpdatePreview is what an update would change on the copy.
type SharedAgentUpdatePreview struct {
	Changes            []SharedAgentChange       `json:"changes"`
	ReplacesYourEdit   []string                  `json:"replacesYourEdit"`
	Risk               SharedAgentRiskChange     `json:"risk"`
	NewSlots           []SharedAgentPlannedSlot  `json:"newSlots"`
	Unavailable        []SharedAgentPlannedSkill `json:"unavailable"`
	DeploymentRevision int64                     `json:"deploymentRevision"`
}

// SharedAgentUpdated is the applied update.
type SharedAgentUpdated struct {
	DefinitionVersion string                            `json:"definitionVersion"`
	Activation        *kernelagent.DefinitionActivation `json:"activation,omitempty"`
	Bindings          []*skill.Binding                  `json:"bindings"`
	Unconnected       []SharedAgentPlannedSlot          `json:"unconnected"`
}
