package authoring

import "github.com/axiom-studio/openseal/internal/domaincontract"

const AuthoringIntentSchemaVersion = "openseal.authoring-intent/v1"

// AuthoringResourceKind is the product resource shape requested by the user.
// It is intentionally semantic: the compiler, not the model, constructs the
// corresponding immutable Agent and Team definitions.
type AuthoringResourceKind string

const (
	AuthoringResourceAgent     AuthoringResourceKind = "agent"
	AuthoringResourceTeam      AuthoringResourceKind = "team"
	AuthoringResourceWorkforce AuthoringResourceKind = "workforce"
)

func (AuthoringResourceKind) ContractValues() []string {
	return []string{string(AuthoringResourceAgent), string(AuthoringResourceTeam), string(AuthoringResourceWorkforce)}
}

func (kind AuthoringResourceKind) Valid() bool { return domaincontract.Allows(string(kind), kind) }

// AuthoringOperationWake describes why an operation begins. Friendly schedule
// text remains semantic input; OpenSeal parses it into the canonical six-field
// cron contract and asks a typed refinement question when it is incomplete.
type AuthoringOperationWake string

const (
	AuthoringWakeOnDemand AuthoringOperationWake = "on_demand"
	AuthoringWakeSchedule AuthoringOperationWake = "schedule"
	AuthoringWakeEvent    AuthoringOperationWake = "event"
)

func (AuthoringOperationWake) ContractValues() []string {
	return []string{string(AuthoringWakeOnDemand), string(AuthoringWakeSchedule), string(AuthoringWakeEvent)}
}

func (wake AuthoringOperationWake) Valid() bool { return domaincontract.Allows(string(wake), wake) }

// AuthoringApprovalIntent records user-facing approval intent. It never grants
// authority: the deterministic compiler intersects it with catalog risk and
// host authority constraints.
type AuthoringApprovalIntent string

const (
	AuthoringApprovalByPolicy AuthoringApprovalIntent = "by_policy"
	AuthoringApprovalRequired AuthoringApprovalIntent = "required"
	AuthoringApprovalStanding AuthoringApprovalIntent = "standing_authority"
)

func (AuthoringApprovalIntent) ContractValues() []string {
	return []string{string(AuthoringApprovalByPolicy), string(AuthoringApprovalRequired), string(AuthoringApprovalStanding)}
}

func (intent AuthoringApprovalIntent) Valid() bool {
	return domaincontract.Allows(string(intent), intent)
}

// AuthoringIntent is the complete provider-facing answer sheet. It contains
// no runtime graph, JSON Pointer, authority grant, binding, deployment, budget,
// version, or generated identifier. OpenSeal compiles those details.
type AuthoringIntent struct {
	SchemaVersion  string                    `json:"schemaVersion" jsonschema:"Version of the semantic authoring answer contract."`
	Kind           AuthoringResourceKind     `json:"kind"`
	Name           string                    `json:"name"`
	Purpose        string                    `json:"purpose"`
	Agents         []AuthoringAgentIntent    `json:"agents"`
	Team           *AuthoringTeamIntent      `json:"team,omitempty"`
	Conversations  []AuthoringChannelIntent  `json:"conversations,omitempty"`
	Activation     WorkforceActivationIntent `json:"activation"`
	Assumptions    []string                  `json:"assumptions,omitempty"`
	Clarifications []AuthoringClarification  `json:"clarifications,omitempty"`
}

type AuthoringAgentIntent struct {
	Key                 string                     `json:"key"`
	Name                string                     `json:"name"`
	Purpose             string                     `json:"purpose"`
	Behavior            string                     `json:"behavior"`
	Personality         string                     `json:"personality,omitempty"`
	OperatingPrinciples []string                   `json:"operatingPrinciples,omitempty"`
	Skills              []AuthoringSkillIntent     `json:"skills,omitempty"`
	Objectives          []AuthoringObjectiveIntent `json:"objectives,omitempty"`
	Operations          []AuthoringOperationIntent `json:"operations,omitempty"`
}

type AuthoringSkillIntent struct {
	CatalogID string   `json:"catalogId"`
	Actions   []string `json:"actions,omitempty"`
	Required  bool     `json:"required"`
}

type AuthoringObjectiveIntent struct {
	Key             string   `json:"key"`
	Title           string   `json:"title"`
	Outcome         string   `json:"outcome"`
	SuccessCriteria []string `json:"successCriteria,omitempty"`
	Constraints     []string `json:"constraints,omitempty"`
	Priority        int      `json:"priority"`
}

type AuthoringOperationIntent struct {
	Key             string                  `json:"key"`
	Name            string                  `json:"name"`
	Goal            string                  `json:"goal"`
	ObjectiveKey    string                  `json:"objectiveKey"`
	Wake            AuthoringOperationWake  `json:"wake"`
	Schedule        string                  `json:"schedule,omitempty"`
	EventType       string                  `json:"eventType,omitempty"`
	SkillCatalogIDs []string                `json:"skillCatalogIds,omitempty"`
	Approval        AuthoringApprovalIntent `json:"approval"`
	ReportProgress  bool                    `json:"reportProgress"`
}

type AuthoringTeamIntent struct {
	Key                 string                     `json:"key"`
	Name                string                     `json:"name"`
	Purpose             string                     `json:"purpose"`
	OperatingPrinciples []string                   `json:"operatingPrinciples,omitempty"`
	Roles               []AuthoringRoleIntent      `json:"roles"`
	Objectives          []AuthoringObjectiveIntent `json:"objectives,omitempty"`
}

type AuthoringRoleIntent struct {
	Key                string   `json:"key"`
	Name               string   `json:"name"`
	Purpose            string   `json:"purpose"`
	AgentKeys          []string `json:"agentKeys"`
	SkillCatalogIDs    []string `json:"skillCatalogIds,omitempty"`
	CanSpeakInChannels bool     `json:"canSpeakInChannels"`
}

type AuthoringChannelIntent struct {
	Key           string   `json:"key"`
	Name          string   `json:"name"`
	OwnerKey      string   `json:"ownerKey"`
	Provider      string   `json:"provider"`
	Destination   string   `json:"destination,omitempty"`
	Purposes      []string `json:"purposes"`
	ReplyInThread bool     `json:"replyInThread"`
}

// AuthoringClarification is deliberately prose-level. OpenSeal maps it to a
// canonical, answerable refinement question and owns category, blocking scope,
// provenance, validation, and credential-safe input kinds.
type AuthoringClarification struct {
	Key       string   `json:"key"`
	Question  string   `json:"question"`
	WhyNeeded string   `json:"whyNeeded"`
	Choices   []string `json:"choices,omitempty"`
}
