package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type runFieldsStore interface {
	PortfolioStore
	RunActivityStore
}

func runFieldsBackends(t *testing.T, test func(t *testing.T, store runFieldsStore)) {
	t.Run("memory", func(t *testing.T) { test(t, NewMemoryStore()) })
	t.Run("sqlite", func(t *testing.T) {
		store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "runtime.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		test(t, store)
	})
}

func runFieldsRun(t *testing.T, store runFieldsStore, sessionID string) *AgentRun {
	t.Helper()
	run, err := NewPortfolioService(store).CreateAgentRun(context.Background(), CreateAgentRunRequest{
		Scope: Scope{Kind: "local", ID: "run-fields"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		Goal: "work", Source: RunSourceManual, SessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestCreateAgentRunStampsLoopEngine(t *testing.T) {
	runFieldsBackends(t, func(t *testing.T, store runFieldsStore) {
		run := runFieldsRun(t, store, "session-1")
		if run.Engine != RunEngineLoop || run.SessionID != "session-1" {
			t.Fatalf("created run: engine %q session %q", run.Engine, run.SessionID)
		}
		stored, err := store.GetAgentRun(context.Background(), run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Engine != RunEngineLoop || stored.SessionID != "session-1" {
			t.Fatalf("stored run: engine %q session %q", stored.Engine, stored.SessionID)
		}
	})
}

func TestTransitionRunRequireInterventionCount(t *testing.T) {
	runFieldsBackends(t, func(t *testing.T, store runFieldsStore) {
		ctx := context.Background()
		activity := NewRunActivityService(store, store)
		run := runFieldsRun(t, store, "")
		run, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, Summary: "started",
		})
		if err != nil {
			t.Fatal(err)
		}
		run, _, err = activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, Summary: "steered",
			Intervention: &AgentRunIntervention{ID: "steer-1", Actor: ActivityActor{Type: "user", ID: "kev"},
				Instruction: "also check the calendar", CreatedAt: time.Now().UTC(), ActionCallID: "call-1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		zero := 0
		if _, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusCompleted, Summary: "done",
			RequireInterventionCount: &zero, Output: map[string]interface{}{"summary": "done"},
		}); !errors.Is(err, ErrSteeringArrived) {
			t.Fatalf("stale intervention count: %v", err)
		}
		stored, err := store.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != AgentRunStatusRunning || stored.Revision != run.Revision || stored.Output != nil {
			t.Fatalf("guarded transition wrote: %#v", stored)
		}
		if len(stored.PendingInterventions) != 1 || stored.PendingInterventions[0].ActionCallID != "call-1" {
			t.Fatalf("intervention action call id lost: %#v", stored.PendingInterventions)
		}
		one := 1
		done, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusCompleted, Summary: "done",
			RequireInterventionCount: &one, Output: map[string]interface{}{"summary": "done"},
		})
		if err != nil || done.Status != AgentRunStatusCompleted {
			t.Fatalf("matching intervention count: %#v %v", done, err)
		}
	})
}

func TestTerminalCreditErrorFromFailureExplanation(t *testing.T) {
	credits := &RunFailureExplanation{Kind: "credits", Code: RunCreditErrorWorkspaceCreditsExhausted, Message: "The Workspace is out of credits."}
	if got := terminalCreditError(AgentRunStatusFailed, credits); got == nil || got.Code != RunCreditErrorWorkspaceCreditsExhausted {
		t.Fatalf("credit failure: %#v", got)
	}
	for name, test := range map[string]struct {
		status  AgentRunStatus
		failure *RunFailureExplanation
	}{
		"not failed":    {AgentRunStatusCompleted, credits},
		"no failure":    {AgentRunStatusFailed, nil},
		"other failure": {AgentRunStatusFailed, &RunFailureExplanation{Kind: "execution", Code: "provider_rate_limited", Message: "busy"}},
	} {
		if got := terminalCreditError(test.status, test.failure); got != nil {
			t.Fatalf("%s: %#v", name, got)
		}
	}
	runFieldsBackends(t, func(t *testing.T, store runFieldsStore) {
		ctx := context.Background()
		activity := NewRunActivityService(store, store)
		run := runFieldsRun(t, store, "")
		run, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, Summary: "started",
		})
		if err != nil {
			t.Fatal(err)
		}
		failed, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusFailed, Summary: "failed",
			Error: credits.Message, FailureExplanation: credits,
		})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range []*AgentRun{failed, stored} {
			if got.CreditError == nil || got.CreditError.Code != RunCreditErrorWorkspaceCreditsExhausted ||
				!reflect.DeepEqual(got.FailureExplanation, credits) {
				t.Fatalf("credit stop: %#v %#v", got.CreditError, got.FailureExplanation)
			}
		}
	})
}

func TestRunFailureExplanationJSON(t *testing.T) {
	explanation := RunFailureExplanation{Kind: "execution", Code: "execution_failed", Message: "The step failed.",
		StepError: "timeout", StepErrorCode: "step_host_unavailable", Explained: true}
	data, err := json.Marshal(explanation)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"execution","code":"execution_failed","message":"The step failed.","stepError":"timeout","stepErrorCode":"step_host_unavailable","explained":true}`
	if string(data) != want {
		t.Fatalf("json:\n got %s\nwant %s", data, want)
	}
	data, _ = json.Marshal(RunFailureExplanation{Kind: "budget", Code: "budget_exhausted", Message: "Out of budget."})
	if string(data) != `{"kind":"budget","code":"budget_exhausted","message":"Out of budget."}` {
		t.Fatalf("omitempty json: %s", data)
	}
	runFieldsBackends(t, func(t *testing.T, store runFieldsStore) {
		ctx := context.Background()
		activity := NewRunActivityService(store, store)
		run := runFieldsRun(t, store, "session-json")
		run, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, Summary: "started",
		})
		if err != nil {
			t.Fatal(err)
		}
		pending := explanation
		pending.Explained = false
		if _, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusQueued, Summary: "explaining", FailureExplanation: &pending,
		}); err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(stored.FailureExplanation, &pending) || stored.CreditError != nil || stored.SessionID != "session-json" {
			t.Fatalf("stored failure explanation: %#v", stored)
		}
		payload, _ := json.Marshal(stored)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["failureExplanation"]; !ok {
			t.Fatalf("run payload has no failureExplanation: %s", payload)
		}
		if string(fields["engine"]) != `"loop"` || string(fields["sessionId"]) != `"session-json"` {
			t.Fatalf("run payload engine/session: %s", payload)
		}
	})
}
