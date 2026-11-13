package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// Kinds of RunFailureExplanation written by the kernel (SPEC §2.2). The guard
// kinds repeat_limit and context_overflow are in
// modelstep/loop_guard_failures.go.
const (
	RunFailureKindExecution          = "execution"
	RunFailureKindWorkspace          = "workspace"
	RunFailureKindRoutineScript      = "routine_script"
	RunFailureKindTaskCompletion     = "task_completion"
	RunFailureKindAgentBehaviorFixed = "agent_behavior_fixed"
	RunFailureKindBudget             = "budget"
	RunFailureKindCredits            = "credits"
)

// RunFailureCodeFinalAnswerRejected fails a Run whose final answer was
// rejected three times in a row: the work was done but no answer could be
// delivered, which the terminal reply says.
const RunFailureCodeFinalAnswerRejected = "final_answer_rejected"

const maximumRunFailureCodeBytes = 64

// BeginRunFailureExplanation records the failure of a stopped attempt. The
// caller stores the result through RunTransitionRequest.FailureExplanation.
// While the Run is still running with it, the Run is in explain mode: the
// loop's next step offers no tools (runExplainingFailure). A Run that already
// records a failure keeps it, so the first cause stays the reported one.
//
// Code is kept as the kernel classified it (run codes such as error_streak
// stay distinct); host codes with a terminalFailureCode class are normalized
// to it, so a gateway credit refusal becomes workspace_credits_exhausted.
func BeginRunFailureExplanation(run *models.AgentRun, kind, code, message, stepError, stepErrorCode string) *models.RunFailureExplanation {
	if run != nil && run.FailureExplanation != nil {
		existing := *run.FailureExplanation
		return &existing
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = RunFailureKindExecution
	}
	message = BoundedTurnFailureText(message)
	if message == "" {
		message = "The requested work did not complete."
	}
	return &models.RunFailureExplanation{
		Kind: kind, Code: runFailureCode(code), Message: message,
		StepError: BoundedTurnFailureText(stepError), StepErrorCode: RunFailureCodeOrEmpty(stepErrorCode),
	}
}

// ExplainedRunFailure is the failure after its model explanation was posted.
func ExplainedRunFailure(f *models.RunFailureExplanation) *models.RunFailureExplanation {
	if f == nil {
		return nil
	}
	explained := *f
	explained.Explained = true
	return &explained
}

func runFailureCode(code string) string {
	if classified := terminalFailureCode(code); classified != "execution_failed" {
		return classified
	}
	if code := RunFailureCodeOrEmpty(code); code != "" {
		return code
	}
	return "execution_failed"
}

// RunFailureCodeOrEmpty keeps a kernel code only when it is a plain bounded
// identifier; anything else (provider text, for example) is dropped.
func RunFailureCodeOrEmpty(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > maximumRunFailureCodeBytes {
		return ""
	}
	for _, r := range code {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
			return ""
		}
	}
	return code
}

// RunExplainingFailure reports whether the Run's next step is its one
// tool-free failure explanation.
func RunExplainingFailure(run *models.AgentRun) bool {
	return run != nil && run.FailureExplanation != nil && !run.FailureExplanation.Explained && !models.IsTerminalAgentRunStatus(run.Status)
}

// failureExplanationNoteText is the kernel instruction of the explanation
// step. It is followed only by the model's tool-free reply.
func failureExplanationNoteText(f *models.RunFailureExplanation) string {
	return "This attempt has stopped: " + strings.TrimRight(f.Message, ". ") + ". Give one brief, useful final reply in your configured voice explaining what did not work and any next step the user actually needs. Ground it in this failure and the successful tool results above. Do not expose internal tool names, ids or governance details, and do not guess that the user's plan, account or permissions are wrong without evidence. Do not call tools, retry, repair, set anything up, schedule, wait, fork or delegate."
}

// FailureExplanationNote is the note the loop appends at the start of the
// explanation step (stepID). Its SourceKey is attempt-scoped, so a replayed
// append of the same step is a no-op and a later attempt gets its own note.
func FailureExplanationNote(sessionID, runID, stepID string, f *models.RunFailureExplanation) *models.SessionEntry {
	if f == nil {
		return nil
	}
	key := models.FailureNoteSourceKey(runID, stepID)
	data, _ := json.Marshal(map[string]string{"code": f.Code})
	return &models.SessionEntry{
		ID: models.SessionEntryID(sessionID, key), SessionID: sessionID, RunID: runID, StepID: stepID,
		Type: models.SessionEntryNote, SourceKey: key,
		Note: &models.NoteEntry{Kind: models.NoteFailureExplanation, Text: failureExplanationNoteText(f), Data: data},
	}
}

// NoModelTurnFailureCode names the host failures under which no model can
// answer this turn: the Workspace has no credits, or the model provider or
// its configuration is unavailable, or the context no longer fits. Every
// other failure can be explained.
func NoModelTurnFailureCode(code string) bool {
	switch strings.TrimSpace(code) {
	case modelstep.TurnHostContextExceeded, modelstep.RunFailureCodeCompactionFailed:
		// No model call fits the context, so none can explain it either.
		return true
	}
	switch terminalFailureCode(code) {
	case "workspace_credits_exhausted", "workspace_daily_limit_reached", "model_configuration_failed", "budget_exhausted",
		"provider_authentication_failed", "provider_access_denied", "provider_quota_exhausted", "provider_rate_limited", "provider_unavailable":
		return true
	}
	return false
}

const maximumTurnFailureTextBytes = 1024

func BoundedTurnFailureText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maximumTurnFailureTextBytes {
		cut := maximumTurnFailureTextBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

// ModelExplainsFailure reports whether a stopped attempt of this Run gets an
// answer-only model turn to explain its failure before it ends. Hosted Agent
// work and conversations do; other kinds end with the kernel's reply.
func ModelExplainsFailure(run *models.AgentRun) bool {
	return run != nil && (run.Kind == models.RunKindAgentWork || run.Kind == models.RunKindConversation)
}

// TurnFailureCanBeExplained reports whether a failed turn leaves a model able
// to answer: not when the host or model provider is unavailable, the
// Workspace has no credits, the budget is spent, or the work was canceled.
func TurnFailureCanBeExplained(err error, code string) bool {
	if err == nil || errors.Is(err, modelstep.ErrTurnHostUnavailable) || errors.Is(err, modelstep.ErrTurnHostConfiguration) ||
		errors.Is(err, models.ErrBudgetExhausted) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, models.ErrLeaseLost) || errors.Is(err, models.ErrTurnLeaseHeld) {
		return false
	}
	switch terminalFailureCode(code) {
	case "model_image_input_unsupported", "agent_behavior_fixed":
		// These have their own exact kernel reply.
		return false
	}
	return !NoModelTurnFailureCode(code)
}

// creditStopFailureCode is a Workspace credit stop, whose typed cause the
// Run keeps even after an earlier tool failure.
func creditStopFailureCode(code string) bool {
	switch terminalFailureCode(code) {
	case "workspace_credits_exhausted", "workspace_daily_limit_reached":
		return true
	}
	return false
}
