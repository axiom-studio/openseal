package runtime

import "github.com/axiom-studio/openseal/pkg/runtime/action"

// Run failure kinds and codes raised by the kernel guards (repeat detection,
// the error streak and compaction). They end in AgentRun.FailureExplanation
// and Run.Error. The context-overflow rejection keeps the host code
// TurnHostContextExceeded ("model_context_exceeded").
const (
	RunFailureKindRepeatLimit     = "repeat_limit"
	RunFailureKindContextOverflow = "context_overflow"

	RunFailureCodeErrorStreak      = "error_streak"
	RunFailureCodeCompactionFailed = "compaction_failed"
)

// The exact Run.Error messages of the guard failures.
const (
	runFailureMessageRepeatLimit      = "The agent repeated the same call without making progress, so the run was stopped."
	runFailureMessageErrorStreak      = "The agent's tool calls kept failing without making progress, so the run was stopped."
	runFailureMessageContextExceeded  = "The conversation is too long for the model's context, even after compacting it."
	runFailureMessageCompactionFailed = "The conversation could not be compacted to fit the model's context."
)

// guardRunFailure is one guard failure as the loop reports it. Explainable
// failures get the loop's one tool-free explanation step before the Run ends;
// the context failures do not, because no model call can fit.
type guardRunFailure struct {
	Kind        string
	Code        string
	Message     string
	Explainable bool
}

var guardRunFailures = map[string]guardRunFailure{
	action.RunFailureCodeRepeatLimit: {
		Kind: RunFailureKindRepeatLimit, Code: action.RunFailureCodeRepeatLimit,
		Message: runFailureMessageRepeatLimit, Explainable: true,
	},
	RunFailureCodeErrorStreak: {
		Kind: RunFailureKindRepeatLimit, Code: RunFailureCodeErrorStreak,
		Message: runFailureMessageErrorStreak, Explainable: true,
	},
	TurnHostContextExceeded: {
		Kind: RunFailureKindContextOverflow, Code: TurnHostContextExceeded,
		Message: runFailureMessageContextExceeded,
	},
	RunFailureCodeCompactionFailed: {
		Kind: RunFailureKindContextOverflow, Code: RunFailureCodeCompactionFailed,
		Message: runFailureMessageCompactionFailed,
	},
}

// guardRunFailureFor returns the guard failure for code, or false when code
// is not a guard failure code.
func guardRunFailureFor(code string) (guardRunFailure, bool) {
	failure, ok := guardRunFailures[code]
	return failure, ok
}
