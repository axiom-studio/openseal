package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/google/uuid"
)

func TestExplainableFailureRunsToolLessStepThenFailsExplained(t *testing.T) {
	actiontest.ForEachStore(t, func(t *testing.T, store KernelStore) {
		ctx := t.Context()
		run := claimedFailureRun(t, store, "explained")
		cause := errors.New("the model returned malformed tool arguments")
		if !modelExplainsFailure(run) || !turnFailureCanBeExplained(cause, "tool_arguments_invalid") {
			t.Fatal("an ordinary step failure must be explainable")
		}
		activity := NewRunActivityService(store, store)
		explaining, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, LeaseOwner: run.LeaseOwner,
			FailureExplanation: beginRunFailureExplanation(run, RunFailureKindExecution, "tool_arguments_invalid", "The model supplied invalid arguments.", cause.Error(), "producer_failed"),
			EventType:          "run.failure_recorded", Summary: "The attempt stopped; the model will explain it",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !runExplainingFailure(explaining) || explaining.CreditError != nil || explaining.FailureExplanation.StepErrorCode != "producer_failed" {
			t.Fatalf("Run is not in explain mode: %+v", explaining.FailureExplanation)
		}
		note := failureExplanationNote(explaining.SessionID, explaining.ID, "step-2", explaining.FailureExplanation)
		if err := note.Validate(); err != nil {
			t.Fatal(err)
		}
		var data map[string]string
		if note.Type != SessionEntryNote || note.Note.Kind != NoteFailureExplanation || note.SourceKey != FailureNoteSourceKey(explaining.ID, "step-2") ||
			note.ID != SessionEntryID(explaining.SessionID, note.SourceKey) || json.Unmarshal(note.Note.Data, &data) != nil || data["code"] != "tool_arguments_invalid" {
			t.Fatalf("note = %+v", note)
		}
		for _, want := range []string{"This attempt has stopped: The model supplied invalid arguments. Give one brief", "Do not call tools"} {
			if !strings.Contains(note.Note.Text, want) {
				t.Fatalf("note text %q lacks %q", note.Note.Text, want)
			}
		}
		if strings.Contains(note.Note.Text, "continuationCheckpoint") || strings.Contains(note.Note.Text, "runOutput") {
			t.Fatalf("note text names the old form: %q", note.Note.Text)
		}

		// The tool-free step posted its reply.
		failed, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: explaining.Revision, Status: AgentRunStatusFailed, LeaseOwner: run.LeaseOwner,
			Error: explaining.FailureExplanation.Message, FailureExplanation: explainedRunFailure(explaining.FailureExplanation),
			Output:    map[string]interface{}{"summary": "I couldn't read that file, so I stopped.", "messageId": "m-1"},
			EventType: "run.failed", Summary: "Run failed",
		})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if runExplainingFailure(stored) || stored.FailureExplanation == nil || !stored.FailureExplanation.Explained || stored.FailureExplanation.Code != "tool_arguments_invalid" || stored.Error != "The model supplied invalid arguments." {
			t.Fatalf("explained failure not persisted: %+v %q", stored.FailureExplanation, stored.Error)
		}
		if reply, ok := externalConversationRunReply(ctx, nil, nil, failed); !ok || reply != "I couldn't read that file, so I stopped." {
			t.Fatalf("explained reply = %q %v", reply, ok)
		}
		if retry, err := agentrun.NewRunCommandService(store).ConversationRetryEligibility(ctx, stored.Scope, stored.ID); err != nil || retry.Available || !strings.Contains(retry.Reason, "already has a reply") {
			t.Fatalf("explained attempt was retryable: %+v %v", retry, err)
		}
	})
}

func TestNonExplainableFailureFailsWithKernelReply(t *testing.T) {
	for _, code := range []string{"gateway_credits_exhausted", "provider_unavailable", "budget_exhausted", TurnHostContextExceeded, RunFailureCodeCompactionFailed, "agent_behavior_fixed", "model_image_input_unsupported"} {
		if turnFailureCanBeExplained(errors.New("failed"), code) {
			t.Fatalf("%s must not get an explanation step", code)
		}
	}
	for _, err := range []error{ErrTurnHostUnavailable, ErrBudgetExhausted, context.Canceled, ErrLeaseLost} {
		if turnFailureCanBeExplained(err, "execution_failed") {
			t.Fatalf("%v must not get an explanation step", err)
		}
	}
	actiontest.ForEachStore(t, func(t *testing.T, store KernelStore) {
		ctx := t.Context()
		run := claimedFailureRun(t, store, "kernel-reply")
		failed, _, err := NewRunActivityService(store, store).TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusFailed, LeaseOwner: run.LeaseOwner, Error: "provider down",
			FailureExplanation: beginRunFailureExplanation(run, RunFailureKindExecution, "provider_unavailable", "The model provider was unavailable.", "upstream 503 SECRET", "host_unavailable"),
			Output:             map[string]interface{}{"summary": "stale model text"},
			EventType:          "run.failed", Summary: "Run failed",
		})
		if err != nil {
			t.Fatal(err)
		}
		if failed.FailureExplanation.Explained || runExplainingFailure(failed) {
			t.Fatalf("unexplained failure marked explained: %+v", failed.FailureExplanation)
		}
		reply, ok := externalConversationRunReply(ctx, store, store, failed)
		if !ok || reply != TerminalFailureReply("provider_unavailable") || strings.Contains(reply, "SECRET") {
			t.Fatalf("kernel reply = %q %v", reply, ok)
		}
		if retry, err := agentrun.NewRunCommandService(store).ConversationRetryEligibility(ctx, failed.Scope, failed.ID); err != nil || !retry.Available || retry.Reason != "" {
			t.Fatalf("unexplained failure must be retryable: %+v %v", retry, err)
		}
	})
}

func TestCreditErrorFromTypedFailure(t *testing.T) {
	actiontest.ForEachStore(t, func(t *testing.T, store KernelStore) {
		ctx := t.Context()
		run := claimedFailureRun(t, store, "credits")
		failed, _, err := NewRunActivityService(store, store).TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusFailed, LeaseOwner: run.LeaseOwner, Error: "no credits",
			FailureExplanation: beginRunFailureExplanation(run, RunFailureKindCredits, "gateway_credits_exhausted", "This workspace has no model credits available.", "", ""),
			EventType:          "run.failed", Summary: "Run failed",
		})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.CreditError == nil || stored.CreditError.Code != RunCreditErrorWorkspaceCreditsExhausted || failed.CreditError == nil {
			t.Fatalf("typed credit stop lost its credit error: %+v %+v", stored.CreditError, stored.FailureExplanation)
		}
		if terminalCreditError(AgentRunStatusRunning, stored.FailureExplanation) != nil || terminalCreditError(AgentRunStatusFailed, nil) != nil ||
			terminalCreditError(AgentRunStatusFailed, &RunFailureExplanation{Code: "provider_unavailable"}) != nil {
			t.Fatal("credit error from an unrelated failure")
		}
	})
}

// claimedFailureRun is a leased Agent conversation Run in a fresh scope.
func claimedFailureRun(t *testing.T, store KernelStore, name string) *AgentRun {
	t.Helper()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: name + "-" + uuid.NewString()[:8]}
	created, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Reply", Source: RunSourceChat, SessionID: "session-" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %+v %v", run, err)
	}
	return run
}
