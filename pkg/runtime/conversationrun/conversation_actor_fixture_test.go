package conversationrun_test

import (
	"context"
	"slices"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Copies of runtime's test helpers for the external conversation run tests
// (actor authority, clarifications and Team approval replacement), which
// cannot see runtime's in-package test helpers. Change them together with
// the originals named on each. The ask_user helpers come from controltest.

// Copied from pkg/runtime/conversation_task_clarification_test.go
// clarificationQuestionKeyOf is the idempotency key of the question a Run
// waiting on an ask_user entry posted: the ask_user settle and the question
// reconciler post under it, so either replays the other. The tests spell it
// out to pin that key.
func clarificationQuestionKeyOf(run *models.AgentRun) string {
	if run == nil || run.WakeCondition == nil {
		return ""
	}
	return "conversation-clarification:" + run.ID + ":" + run.WakeCondition.Reference
}

// Copied from pkg/runtime/conversation_thread_start_test.go
// runHasFollowUp reports whether messageID was delivered into run as a
// follow-up.
func runHasFollowUp(run *models.AgentRun, messageID string) bool {
	return slices.ContainsFunc(models.ConversationRunFollowUps(run), func(delivery models.ConversationMessageDelivery) bool {
		return delivery.MessageID == messageID
	})
}

// Copied from pkg/runtime/run_cancellation_test.go
func cancellationActionCatalog(t *testing.T) (*skill.Catalog, models.Scope) {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "cancellation"}
	catalog := skill.NewCatalog()
	definition := &skill.Definition{
		ID: "forum", Version: "1.0.0", Name: "Forum", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://forum.invalid"},
		Actions: map[string]skill.Action{"reply": {
			Name: "reply", Description: "Post a reviewed reply", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal,
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"body": map[string]interface{}{"type": "string"}}, "required": []interface{}{"body"}},
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencyRequired,
		}},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "forum-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "research-agent",
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{"reply"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog, scope
}
