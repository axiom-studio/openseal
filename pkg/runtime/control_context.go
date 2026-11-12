package runtime

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

type FinalAnswerMode string

const (
	FinalModeDefault  FinalAnswerMode = "default"
	FinalModeDecision FinalAnswerMode = "decision" // Run.Context[AgentRequestInboxContextKey]
	FinalModeReview   FinalAnswerMode = "review"   // Run.Context[AgentRequestCompletionReviewContextKey]
)

// ControlAuthority is resolved once per claim (ResolveControlAuthority,
// control_authority.go) and is constant for the claim, so the advertised tool
// list is stable. It carries the conversation flags of section 05 §7.
type ControlAuthority struct {
	AgentID           string
	FinalMode         FinalAnswerMode // "" = default
	SkillPromptRefs   []string
	VisualAllowed     bool
	BroadcastAllowed  bool
	ForkAllowed       bool
	ForkTargets       []HostedAgentTarget // other Agents a branch may target; empty = self only
	DelegateTargets   []HostedAgentTarget
	Operations        []HostedRunbookOperation
	TaskStart         *models.HostedConversationTaskContext // offered iff non-nil && CanStart && !VoiceCallStarted
	TaskWork          bool                                  // the Run is the work of a conversation task
	CanReportProgress bool
	CanReact          bool
	CanAskUser        bool
	CanWait           bool
	// Conversation flags (section 05 §7).
	ExternalChannelReply bool
	VoiceCallStarted     bool
	ApprovalMode         models.ConversationApprovalMode
	// RoutineOccurrence marks a Run started by a routine (schedule or
	// event), not a chat reply: it may finish silent when nothing needs the
	// person and it changed nothing.
	RoutineOccurrence bool
}

func (a ControlAuthority) finalMode() FinalAnswerMode {
	if a.FinalMode == "" {
		return FinalModeDefault
	}
	return a.FinalMode
}

// ControlOrigin says who authored a control call: the model, or a
// deterministic producer (runbook engine, section 04 §12) whose calls carry a
// kernel directive and are preauthorized.
type ControlOrigin string

// ControlContext is built by the loop for each control call.
type ControlContext struct {
	Run       *models.AgentRun // re-read; Status running; LeaseOwner == WorkerID
	WorkerID  string
	Step      *models.AgentTurn // settle phase: the entry's producing step: completed && Sequence <= Run.LastAppliedTurn
	Session   *models.AgentSession
	Path      *models.AgentSessionPath // the step's session path (URL provenance, §11.4)
	Binding   *TurnRunnerBinding
	Assistant *models.SessionEntry // assistant entry holding the call
	Entry     *models.SessionEntry // this call's tool entry
	Authority ControlAuthority
	Origin    ControlOrigin
	// Arguments is the strictly decoded call input (rule 4), set by the
	// dispatcher before a handler runs. Handlers type-assert it to the value
	// their decode returned.
	Arguments interface{}
}

func (cc ControlContext) toolName() models.ControlToolName {
	if cc.Entry == nil || cc.Entry.Tool == nil {
		return ""
	}
	return models.ControlToolName(cc.Entry.Tool.Name)
}

func (cc ControlContext) lease(now time.Time) models.AgentRunLeaseGuard {
	return models.AgentRunLeaseGuard{WorkerID: cc.WorkerID, Now: now}
}

// offersCapabilities is false for decision and review Runs: they are offered
// only their own final tool, no capability, host or governed tool.
func (a ControlAuthority) offersCapabilities() bool {
	mode := a.finalMode()
	return mode != FinalModeDecision && mode != FinalModeReview
}
