package runtime

import "context"

// The seams the RunLoop calls. Each owning section implements one; the loop
// owns only the order in which they run (SPEC §3.1).

// PromptContribution is the kernel-authored system text the owning section
// adds to every model step of a Run.
type PromptContribution struct {
	AgentInstructions []string
	Sections          []ModelStepSystemSection
}

// SteeringPlan is what consumeSteering decided for this step: the
// interventions in the unconsumed prefix, and the channel message sequence
// the conversation catch-up must cover so steering messages are not
// appended twice.
type SteeringPlan struct {
	Prefix                 []AgentRunIntervention
	ThroughMessageSequence int64
}

// StepContextAppend is a preparer's contribution to the one context append of
// a step (phase 3).
type StepContextAppend struct {
	Entries []*SessionEntry
	// ExpectedChannelWatermark, when set, makes the append a watermark CAS
	// that moves the session's watermark to ChannelWatermark.
	ExpectedChannelWatermark *int64
	ChannelWatermark         int64
	// EarlyFinal ends the Run without a model call.
	EarlyFinal *LoopFinal
}

// StepPreparer runs before each step: run start (prep-5), then conversation
// catch-up (CONV-07a).
type StepPreparer interface {
	BeforeStep(ctx context.Context, in StepInput, steering SteeringPlan) (*StepContextAppend, error)
}

// ModelStepPreparer builds the model request of one step (prep-2).
type ModelStepPreparer interface {
	PrepareModelStep(ctx context.Context, in StepInput) (ModelStepRequest, error)
}

// LoopToolExecutor runs the tool entries of a step (disp-1).
type LoopToolExecutor interface {
	// ExecuteStepTools is the inline phase, on the pending entries the loop appended.
	ExecuteStepTools(ctx context.Context, in StepToolsInput) (StepToolsOutcome, error)
	// SettleStep runs after FinishTurn+applyStep: at most one settle-phase call.
	SettleStep(ctx context.Context, in StepToolsInput) (StepToolsOutcome, error)
	// ResumeOpenTools settles open entries of earlier steps (D7/D6/D14).
	ResumeOpenTools(ctx context.Context, in StepToolsInput) (StepToolsOutcome, error)
	// ReplanPark runs after a park CAS failure; nil means do not park.
	ReplanPark(ctx context.Context, in StepToolsInput) (*LoopPark, error)
}

type StepToolsInput struct {
	Run       *AgentRun
	Step      *AgentTurn
	Session   *AgentSession
	Path      *AgentSessionPath
	Binding   *TurnRunnerBinding
	WorkerID  string
	Assistant *SessionEntry
	Entries   []*SessionEntry // this step's tool entries
}

type StepToolsOutcome struct {
	Run             *AgentRun // latest Run if the executor changed it
	Final           *FinalAnswer
	Park            *LoopPark
	Failure         *LoopFailure
	BudgetExhausted *BudgetExhaustion
	HostToolCalls   int  // → TurnUsage.NativeOperations
	SettlePending   bool // a settle-phase call is pending; the loop calls SettleStep
}

// BudgetExhaustion is the loop's budget exit (Run paused, budget.exhausted)
// reported by an executor, for example a governed call denied because the
// Run's action budget is used up. It is not a park.
type BudgetExhaustion struct {
	Summary      string
	Dimensions   []string         // exhausted budget dimensions, when known
	Admission    *BudgetAdmission // the reservation calculation, when one refused
	ActionCallID string           // the denied governed call, if any
}

// FinalAnswer is produced only by the control section (decodeFinalAnswer /
// PlainTextFinal).
type FinalAnswer struct {
	StepID      string
	ToolEntryID string // "" for plain text
	ToolCallID  string // "" for plain text
	PlainText   bool
	Answer      string
	Output      map[string]interface{}
	Park        *FinalPark // OTP wait or pending setup
}

// FinalPark parks the Run after (Deliver) or instead of posting the answer.
type FinalPark struct {
	Deliver bool
	Park    LoopPark
}

type FinalDelivery struct {
	Run    *AgentRun
	Step   *AgentTurn
	Answer FinalAnswer
	Guard  ReplyRunGuard
}

type FinalDeliveryKind string

const (
	FinalDeliveryComplete   FinalDeliveryKind = "complete"
	FinalDeliveryFail       FinalDeliveryKind = "fail"
	FinalDeliveryRetryFinal FinalDeliveryKind = "retry_final"
	FinalDeliveryContinue   FinalDeliveryKind = "continue"
	FinalDeliveryPark       FinalDeliveryKind = "park"
)

type FinalDeliveryResult struct {
	Kind          FinalDeliveryKind
	Output        map[string]interface{}
	RejectionText string
	Failure       *LoopFailure
}

// FinalAnswerHandler posts the final answer (CONV-08 for chats,
// runOutputFinalHandler otherwise). ErrSteeringArrived means continue.
type FinalAnswerHandler interface {
	Deliver(ctx context.Context, d FinalDelivery) (FinalDeliveryResult, error)
}

// StepCompactor compacts the session path at step start (section 08 G3).
type StepCompactor interface {
	Due(in StepInput) bool
	CompactIfNeeded(ctx context.Context, in StepInput) (ModelStepUsage, error)
	Compact(ctx context.Context, in StepInput, force bool) (ModelStepUsage, error)
}

// ToolCallCheck may refuse a tool entry before it runs (conversation
// CheckToolCall). A non-empty refusal becomes the entry's error result.
type ToolCallCheck func(ctx context.Context, in StepToolsInput, entry *SessionEntry) (refusal string, err error)

// AfterToolEntryHook runs after a tool entry became terminal (conversation
// artifact receipts).
type AfterToolEntryHook func(ctx context.Context, in StepToolsInput, entry *SessionEntry) error
