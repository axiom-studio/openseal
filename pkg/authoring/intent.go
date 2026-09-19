package authoring

import "github.com/axiom-studio/openseal/internal/domaincontract"

const AuthoringIntentSchemaVersion = "openseal.authoring-intent/v4"

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
	SchemaVersion  string                   `json:"schemaVersion" jsonschema:"Version of the semantic authoring answer contract."`
	Kind           AuthoringResourceKind    `json:"kind"`
	Name           string                   `json:"name" jsonschema:"minLength=1,pattern=\\S"`
	Purpose        string                   `json:"purpose" jsonschema:"minLength=1,pattern=\\S"`
	Agents         []AuthoringAgentIntent   `json:"agents" jsonschema:"minItems=1"`
	Team           *AuthoringTeamIntent     `json:"team,omitempty"`
	Conversations  []AuthoringChannelIntent `json:"conversations,omitempty"`
	Assumptions    []string                 `json:"assumptions,omitempty"`
	Clarifications []AuthoringClarification `json:"clarifications,omitempty"`
}

// AuthoringIntentRequest is the complete provider projection. Existing state
// is represented through the same semantic answer sheet; canonical resource
// structs and the compiler-owned authoring form never enter model context.
type AuthoringIntentRequest struct {
	AgentName               string                          `json:"agentName,omitempty"`
	ExistingAgentNames      []string                        `json:"existingAgentNames,omitempty"`
	Mode                    Mode                            `json:"mode"`
	Prompt                  string                          `json:"prompt"`
	Existing                *AuthoringIntent                `json:"existing,omitempty"`
	Catalog                 CapabilityCatalog               `json:"catalog"`
	CompositionRequirements *RuntimeCompositionRequirements `json:"compositionRequirements,omitempty"`
	Refinement              *RefinementContext              `json:"refinement,omitempty"`
}

type AuthoringAgentIntent struct {
	Key                 string                     `json:"key" jsonschema:"pattern=^[a-z][a-z0-9-]{0\\,63}$"`
	Name                string                     `json:"name" jsonschema:"minLength=1,pattern=\\S"`
	Purpose             string                     `json:"purpose" jsonschema:"minLength=1,pattern=\\S"`
	Behavior            string                     `json:"behavior" jsonschema:"minLength=1,pattern=\\S"`
	Personality         string                     `json:"personality,omitempty"`
	OperatingPrinciples []string                   `json:"operatingPrinciples,omitempty"`
	Skills              []AuthoringSkillIntent     `json:"skills,omitempty"`
	Objectives          []AuthoringObjectiveIntent `json:"objectives,omitempty"`
	Operations          []AuthoringOperationIntent `json:"operations,omitempty"`
}

type AuthoringSkillIntent struct {
	CatalogID string   `json:"catalogId" jsonschema:"minLength=1,pattern=\\S"`
	Actions   []string `json:"actions,omitempty"`
	Required  bool     `json:"required"`
}

type AuthoringObjectiveIntent struct {
	Key             string   `json:"key" jsonschema:"pattern=^[a-z][a-z0-9-]{0\\,63}$"`
	Title           string   `json:"title" jsonschema:"minLength=1,pattern=\\S"`
	Outcome         string   `json:"outcome" jsonschema:"minLength=1,pattern=\\S"`
	SuccessCriteria []string `json:"successCriteria,omitempty"`
	Constraints     []string `json:"constraints,omitempty"`
	Priority        int      `json:"priority" jsonschema:"minimum=0"`
}

type AuthoringOperationIntent struct {
	Key                 string                    `json:"key" jsonschema:"pattern=^[a-z][a-z0-9-]{0\\,63}$"`
	Name                string                    `json:"name" jsonschema:"minLength=1,pattern=\\S"`
	Goal                string                    `json:"goal" jsonschema:"minLength=1,pattern=\\S"`
	ObjectiveKey        string                    `json:"objectiveKey" jsonschema:"pattern=^[a-z][a-z0-9-]{0\\,63}$"`
	Wake                AuthoringOperationWake    `json:"wake"`
	Schedule            string                    `json:"schedule,omitempty"`
	EventType           string                    `json:"eventType,omitempty"`
	SkillCatalogIDs     []string                  `json:"skillCatalogIds,omitempty"`
	Approval            AuthoringApprovalIntent   `json:"approval"`
	ApprovalDelivery    AuthoringApprovalDelivery `json:"approvalDelivery"`
	ApprovalChannelKeys []string                  `json:"approvalChannelKeys,omitempty"`
	ReportProgress      bool                      `json:"reportProgress"`
}

// AuthoringApprovalDelivery makes the reviewed approval surface explicit.
// Channel keys are semantic references which the compiler resolves into the
// one canonical endpoint-purpose and Agent-authority relationship.
type AuthoringApprovalDelivery string

const (
	AuthoringApprovalDeliveryPlatform AuthoringApprovalDelivery = "platform"
	AuthoringApprovalDeliveryChannels AuthoringApprovalDelivery = "channels"
)

func (AuthoringApprovalDelivery) ContractValues() []string {
	return []string{string(AuthoringApprovalDeliveryPlatform), string(AuthoringApprovalDeliveryChannels)}
}

func (delivery AuthoringApprovalDelivery) Valid() bool {
	return domaincontract.Allows(string(delivery), delivery)
}

type AuthoringTeamIntent struct {
	Key                 string                     `json:"key" jsonschema:"pattern=^[a-z][a-z0-9-]{0\\,63}$"`
	Name                string                     `json:"name" jsonschema:"minLength=1,pattern=\\S"`
	Purpose             string                     `json:"purpose" jsonschema:"minLength=1,pattern=\\S"`
	OperatingPrinciples []string                   `json:"operatingPrinciples,omitempty"`
	Roles               []AuthoringRoleIntent      `json:"roles" jsonschema:"minItems=1"`
	Objectives          []AuthoringObjectiveIntent `json:"objectives,omitempty"`
}

type AuthoringRoleIntent struct {
	Key                string   `json:"key" jsonschema:"pattern=^[a-z][a-z0-9-]{0\\,63}$"`
	Name               string   `json:"name" jsonschema:"minLength=1,pattern=\\S"`
	Purpose            string   `json:"purpose" jsonschema:"minLength=1,pattern=\\S"`
	AgentKeys          []string `json:"agentKeys" jsonschema:"minItems=1"`
	SkillCatalogIDs    []string `json:"skillCatalogIds,omitempty"`
	CanSpeakInChannels bool     `json:"canSpeakInChannels"`
}

type AuthoringChannelIntent struct {
	Key             string `json:"key" jsonschema:"pattern=^[a-z][a-z0-9-]{0\\,63}$"`
	Name            string `json:"name" jsonschema:"minLength=1,pattern=\\S"`
	OwnerKey        string `json:"ownerKey" jsonschema:"pattern=^[a-z][a-z0-9-]{0\\,63}$"`
	Provider        string `json:"provider" jsonschema:"minLength=1,pattern=\\S"`
	Destination     string `json:"destination,omitempty"`
	ReceiveMessages bool   `json:"receiveMessages"`
	ReplyInThread   bool   `json:"replyInThread"`
}

// AuthoringClarification is deliberately prose-level. OpenSeal maps it to a
// canonical, answerable refinement question and owns category, blocking scope,
// provenance, validation, and credential-safe input kinds.
type AuthoringClarification struct {
	Key       string   `json:"key" jsonschema:"pattern=^[a-z][a-z0-9-]{0\\,63}$"`
	Question  string   `json:"question" jsonschema:"minLength=1,pattern=\\S"`
	WhyNeeded string   `json:"whyNeeded" jsonschema:"minLength=1,pattern=\\S"`
	Choices   []string `json:"choices,omitempty"`
}
