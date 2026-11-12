package conversation

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

func TestBeginRunFailureExplanationClassifiesAndBounds(t *testing.T) {
	for code, want := range map[string]string{
		"gateway_credits_exhausted":         "workspace_credits_exhausted",
		"provider_model_unavailable":        "provider_unavailable",
		modelstep.RunFailureCodeErrorStreak: modelstep.RunFailureCodeErrorStreak,
		modelstep.TurnHostContextExceeded:   modelstep.TurnHostContextExceeded,
		"task_completion_rejected":          "task_completion_rejected",
		"":                                  "execution_failed",
		"provider said: TOKEN=SECRET":       "execution_failed",
		strings.Repeat("x", 100):            "execution_failed",
		"unverified_approval_claim":         "unverified_approval_claim",
		"  final_answer_rejected  ":         "final_answer_rejected",
		"agent_behavior_fixed":              "agent_behavior_fixed",
		"workspace_turn_budget_exhausted":   "budget_exhausted",
	} {
		f := BeginRunFailureExplanation(&models.AgentRun{}, "", code, "m", "", "")
		if f.Code != want || f.Kind != RunFailureKindExecution || f.Explained {
			t.Fatalf("code %q = %+v, want %s", code, f, want)
		}
	}
	long := BeginRunFailureExplanation(nil, RunFailureKindWorkspace, "workspace_operation_failed", strings.Repeat("é", 2000), strings.Repeat("e", 4000), "bad code!")
	if len(long.Message) > maximumTurnFailureTextBytes+len("…") || len(long.StepError) > maximumTurnFailureTextBytes+len("…") || long.StepErrorCode != "" || long.Kind != RunFailureKindWorkspace {
		t.Fatalf("failure not bounded: %d %d %q", len(long.Message), len(long.StepError), long.StepErrorCode)
	}
	if empty := BeginRunFailureExplanation(nil, "", "", "  ", "", ""); empty.Message == "" {
		t.Fatal("a failure needs a message")
	}
	first := &models.RunFailureExplanation{Kind: RunFailureKindExecution, Code: "execution_failed", Message: "first"}
	if again := BeginRunFailureExplanation(&models.AgentRun{FailureExplanation: first}, "", "provider_unavailable", "second", "", ""); again == first || *again != *first {
		t.Fatalf("a second failure replaced the first cause: %+v", again)
	}
}
