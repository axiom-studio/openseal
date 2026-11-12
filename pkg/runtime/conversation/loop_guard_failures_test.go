package conversation

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

func TestGuardRunFailureFor(t *testing.T) {
	cases := []struct {
		code        string
		kind        string
		message     string
		explainable bool
	}{
		{"repeat_limit", "repeat_limit", "The agent repeated the same call without making progress, so the run was stopped.", true},
		{"error_streak", "repeat_limit", "The agent's tool calls kept failing without making progress, so the run was stopped.", true},
		{"model_context_exceeded", "context_overflow", "The conversation is too long for the model's context, even after compacting it.", false},
		{"compaction_failed", "context_overflow", "The conversation could not be compacted to fit the model's context.", false},
	}
	for _, tc := range cases {
		failure, ok := modelstep.GuardRunFailureFor(tc.code)
		if !ok {
			t.Fatalf("%s: not a guard failure", tc.code)
		}
		if failure.Code != tc.code || failure.Kind != tc.kind || failure.Message != tc.message || failure.Explainable != tc.explainable {
			t.Fatalf("%s: got %+v", tc.code, failure)
		}
		if len(failure.Message) > maximumTurnFailureTextBytes {
			t.Fatalf("%s: message over the failure text bound", tc.code)
		}
	}
	if _, ok := modelstep.GuardRunFailureFor("provider_unavailable"); ok {
		t.Fatal("provider_unavailable is not a guard failure")
	}
}
