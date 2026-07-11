// Package kernelapi defines the versioned HTTP contract shared by the
// standalone OpenSeal daemon and thin clients such as the terminal UI.
package kernelapi

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

const (
	Version                       = "1"
	AgentRunsCapabilityID         = "agent-runs"
	AgentRunsCapabilityVersion    = "1"
	ObjectivesCapabilityID        = "objectives"
	ObjectivesCapabilityVersion   = "1"
	ArtifactsCapabilityID         = "artifacts"
	ArtifactsCapabilityVersion    = "1"
	TeamChannelsCapabilityID      = "team-channels"
	TeamChannelsCapabilityVersion = "1"
)

const (
	OperationCreate     = "create"
	OperationGet        = "get"
	OperationList       = "list"
	OperationPause      = "pause"
	OperationResume     = "resume"
	OperationCancel     = "cancel"
	OperationIntervene  = "intervene"
	OperationUpdate     = "update"
	OperationRegister   = "register"
	OperationUpload     = "upload"
	OperationDownload   = "download"
	OperationResolve    = "resolve"
	OperationPost       = "post"
	OperationCoordinate = "coordinate"
	OperationRead       = "read"
	OperationPresence   = "presence"
	OperationAudit      = "audit"
	OperationChanges    = "changes"
)

// CapabilityDocument is the authoritative product surface advertised by an
// OpenSeal server. Clients must not infer operations that are absent here.
type CapabilityDocument struct {
	Version      string       `json:"version"`
	Capabilities []Capability `json:"capabilities"`
}

type Capability struct {
	ID         string   `json:"id"`
	Version    string   `json:"version"`
	Available  bool     `json:"available"`
	Operations []string `json:"operations"`
}

func (d CapabilityDocument) Find(id, version string) (Capability, bool) {
	for _, candidate := range d.Capabilities {
		if candidate.ID == id && candidate.Version == version {
			return candidate, true
		}
	}
	return Capability{}, false
}

func (c Capability) Supports(operation string) bool {
	if !c.Available {
		return false
	}
	for _, candidate := range c.Operations {
		if candidate == operation {
			return true
		}
	}
	return false
}

func AgentRunsCapability() Capability {
	return Capability{
		ID: AgentRunsCapabilityID, Version: AgentRunsCapabilityVersion, Available: true,
		Operations: []string{
			OperationCreate, OperationGet, OperationList, OperationPause,
			OperationResume, OperationCancel, OperationIntervene,
		},
	}
}

func ObjectivesCapability() Capability {
	return Capability{
		ID: ObjectivesCapabilityID, Version: ObjectivesCapabilityVersion, Available: true,
		Operations: []string{OperationCreate, OperationGet, OperationList, OperationUpdate},
	}
}

func Capabilities() CapabilityDocument {
	return CapabilityDocument{
		Version:      Version,
		Capabilities: []Capability{ObjectivesCapability(), AgentRunsCapability(), TeamChannelsCapability()},
	}
}

func ArtifactCapability(contentOperations ...string) Capability {
	capability := Capability{
		ID: ArtifactsCapabilityID, Version: ArtifactsCapabilityVersion, Available: true,
		Operations: []string{OperationRegister, OperationGet, OperationList},
	}
	for _, operation := range contentOperations {
		if operation != OperationUpload && operation != OperationDownload && operation != OperationResolve {
			continue
		}
		if !capability.Supports(operation) {
			capability.Operations = append(capability.Operations, operation)
		}
	}
	return capability
}

func TeamChannelsCapability() Capability {
	return Capability{
		ID: TeamChannelsCapabilityID, Version: TeamChannelsCapabilityVersion, Available: true,
		Operations: []string{OperationCreate, OperationGet, OperationList, OperationPost, OperationCoordinate, OperationRead, OperationPresence, OperationAudit, OperationChanges},
	}
}

func NewCapabilityDocument(capabilities ...Capability) CapabilityDocument {
	return CapabilityDocument{Version: Version, Capabilities: append([]Capability(nil), capabilities...)}
}

// CreateAgentRunRequest is the public request body for canonical durable work.
// Scope is explicit because standalone OpenSeal can host more than one local
// workspace even though the TUI defaults to local/default.
type CreateAgentRunRequest struct {
	Scope           runtime.Scope              `json:"scope"`
	Kind            runtime.RunKind            `json:"kind,omitempty"`
	ObjectiveID     string                     `json:"objectiveId,omitempty"`
	ParentRunID     string                     `json:"parentRunId,omitempty"`
	Owner           runtime.ObjectiveOwner     `json:"owner"`
	AssignedAgentID string                     `json:"assignedAgentId,omitempty"`
	ConcurrencyKey  string                     `json:"concurrencyKey,omitempty"`
	Goal            string                     `json:"goal"`
	Source          runtime.RunSource          `json:"source"`
	Priority        int                        `json:"priority,omitempty"`
	Deadline        *time.Time                 `json:"deadline,omitempty"`
	AvailableAt     *time.Time                 `json:"availableAt,omitempty"`
	Context         map[string]interface{}     `json:"context,omitempty"`
	Plan            map[string]interface{}     `json:"plan,omitempty"`
	Checkpoint      map[string]interface{}     `json:"checkpoint,omitempty"`
	WakeCondition   *runtime.WakeCondition     `json:"wakeCondition,omitempty"`
	Budget          *runtime.BudgetPolicy      `json:"budget,omitempty"`
	Policy          map[string]interface{}     `json:"policy,omitempty"`
	IdempotencyKey  string                     `json:"idempotencyKey,omitempty"`
	Actor           runtime.ActivityActor      `json:"actor,omitempty"`
	Visibility      runtime.ActivityVisibility `json:"visibility,omitempty"`
}

type CreateObjectiveRequest struct {
	Scope            runtime.Scope           `json:"scope"`
	Owner            runtime.ObjectiveOwner  `json:"owner"`
	Title            string                  `json:"title"`
	Goal             string                  `json:"goal"`
	Status           runtime.ObjectiveStatus `json:"status,omitempty"`
	Priority         int                     `json:"priority,omitempty"`
	Cadence          map[string]interface{}  `json:"cadence,omitempty"`
	EventRules       map[string]interface{}  `json:"eventRules,omitempty"`
	Budget           *runtime.BudgetPolicy   `json:"budget,omitempty"`
	Constraints      map[string]interface{}  `json:"constraints,omitempty"`
	SuccessCriteria  map[string]interface{}  `json:"successCriteria,omitempty"`
	NextEvaluationAt *time.Time              `json:"nextEvaluationAt,omitempty"`
	IdempotencyKey   string                  `json:"idempotencyKey,omitempty"`
}

type UpdateObjectiveRequest struct {
	ExpectedRevision int64                    `json:"expectedRevision"`
	Title            *string                  `json:"title,omitempty"`
	Goal             *string                  `json:"goal,omitempty"`
	Status           *runtime.ObjectiveStatus `json:"status,omitempty"`
	Priority         *int                     `json:"priority,omitempty"`
	Cadence          map[string]interface{}   `json:"cadence,omitempty"`
	EventRules       map[string]interface{}   `json:"eventRules,omitempty"`
	Budget           *runtime.BudgetPolicy    `json:"budget,omitempty"`
	Constraints      map[string]interface{}   `json:"constraints,omitempty"`
	SuccessCriteria  map[string]interface{}   `json:"successCriteria,omitempty"`
	ProgressSummary  *string                  `json:"progressSummary,omitempty"`
	NextEvaluationAt *time.Time               `json:"nextEvaluationAt,omitempty"`
}

type ObjectiveDetail struct {
	Objective *runtime.Objective  `json:"objective"`
	Runs      []*runtime.AgentRun `json:"runs"`
}

type AgentRunCommandRequest struct {
	ExpectedRevision int64                       `json:"expectedRevision"`
	Kind             runtime.AgentRunCommandKind `json:"kind"`
	Actor            runtime.ActivityActor       `json:"actor,omitempty"`
	Summary          string                      `json:"summary,omitempty"`
	Instruction      string                      `json:"instruction,omitempty"`
	Visibility       runtime.ActivityVisibility  `json:"visibility,omitempty"`
}

type ResolveArtifactContentRequest struct {
	Actor      runtime.ActivityActor `json:"actor"`
	Purpose    string                `json:"purpose"`
	TTLSeconds int64                 `json:"ttlSeconds"`
}

type CreateConversationRequest struct {
	ID             string                 `json:"id,omitempty"`
	Scope          runtime.Scope          `json:"scope"`
	Owner          runtime.ObjectiveOwner `json:"owner"`
	Title          string                 `json:"title"`
	IdempotencyKey string                 `json:"idempotencyKey,omitempty"`
}

type PostChannelMessageRequest struct {
	ID                  string                            `json:"id,omitempty"`
	Scope               runtime.Scope                     `json:"scope"`
	ExpectedRevision    int64                             `json:"expectedRevision"`
	Sender              runtime.ConversationParticipant   `json:"sender"`
	Intent              runtime.ConversationMessageIntent `json:"intent"`
	Content             string                            `json:"content"`
	Audience            runtime.ConversationAudience      `json:"audience"`
	ReplyToMessageID    string                            `json:"replyToMessageId,omitempty"`
	Mentions            []runtime.ConversationParticipant `json:"mentions,omitempty"`
	References          []runtime.ConversationReference   `json:"references,omitempty"`
	RequiresResponse    bool                              `json:"requiresResponse,omitempty"`
	ResolvesMessageID   string                            `json:"resolvesMessageId,omitempty"`
	SupersedesMessageID string                            `json:"supersedesMessageId,omitempty"`
	IdempotencyKey      string                            `json:"idempotencyKey,omitempty"`
}

type CoordinateParticipationRequest struct {
	ID               string                                `json:"id,omitempty"`
	Scope            runtime.Scope                         `json:"scope"`
	ExpectedRevision int64                                 `json:"expectedRevision"`
	TriggerMessageID string                                `json:"triggerMessageId,omitempty"`
	Policy           runtime.ConversationArbitrationPolicy `json:"policy,omitempty"`
	Proposals        []runtime.ParticipationProposal       `json:"proposals"`
	IdempotencyKey   string                                `json:"idempotencyKey,omitempty"`
}

type AdvanceConversationCursorRequest struct {
	Scope             runtime.Scope                   `json:"scope"`
	Participant       runtime.ConversationParticipant `json:"participant"`
	ExpectedRevision  int64                           `json:"expectedRevision,omitempty"`
	DeliveredSequence int64                           `json:"deliveredSequence"`
	ReadSequence      int64                           `json:"readSequence"`
}

type SetConversationPresenceRequest struct {
	Scope            runtime.Scope                     `json:"scope"`
	Participant      runtime.ConversationParticipant   `json:"participant"`
	State            runtime.ConversationPresenceState `json:"state"`
	Summary          string                            `json:"summary,omitempty"`
	RunID            string                            `json:"runId,omitempty"`
	LeaseID          string                            `json:"leaseId,omitempty"`
	ExpectedRevision int64                             `json:"expectedRevision,omitempty"`
	TTLSeconds       int64                             `json:"ttlSeconds,omitempty"`
}

type ReleaseConversationPresenceRequest struct {
	Scope       runtime.Scope                   `json:"scope"`
	Participant runtime.ConversationParticipant `json:"participant"`
	LeaseID     string                          `json:"leaseId"`
}
