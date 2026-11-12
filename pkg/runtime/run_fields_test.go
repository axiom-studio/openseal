package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type runFieldsStore interface {
	models.PortfolioStore
	models.RunActivityStore
}

func runFieldsBackends(t *testing.T, test func(t *testing.T, store runFieldsStore)) {
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.New(filepath.Join(t.TempDir(), "runtime.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		test(t, store)
	})
}

func runFieldsRun(t *testing.T, store runFieldsStore, sessionID string) *models.AgentRun {
	t.Helper()
	run, err := portfolio.NewService(store).CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
		Scope: models.Scope{Kind: "local", ID: "run-fields"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"},
		Goal: "work", Source: models.RunSourceManual, SessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestCreateAgentRunStampsLoopEngine(t *testing.T) {
	runFieldsBackends(t, func(t *testing.T, store runFieldsStore) {
		run := runFieldsRun(t, store, "session-1")
		if run.Engine != models.RunEngineLoop || run.SessionID != "session-1" {
			t.Fatalf("created run: engine %q session %q", run.Engine, run.SessionID)
		}
		stored, err := store.GetAgentRun(context.Background(), run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Engine != models.RunEngineLoop || stored.SessionID != "session-1" {
			t.Fatalf("stored run: engine %q session %q", stored.Engine, stored.SessionID)
		}
	})
}

func TestTransitionRunRequireInterventionCount(t *testing.T) {
	runFieldsBackends(t, func(t *testing.T, store runFieldsStore) {
		ctx := context.Background()
		activity := runtimeactivity.NewRunActivityService(store, store)
		run := runFieldsRun(t, store, "")
		run, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, runtimeactivity.RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning, Summary: "started",
		})
		if err != nil {
			t.Fatal(err)
		}
		run, _, err = activity.TransitionRun(ctx, run.Scope, run.ID, runtimeactivity.RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning, Summary: "steered",
			Intervention: &models.AgentRunIntervention{ID: "steer-1", Actor: models.ActivityActor{Type: "user", ID: "kev"},
				Instruction: "also check the calendar", CreatedAt: time.Now().UTC(), ActionCallID: "call-1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		zero := 0
		if _, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, runtimeactivity.RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: models.AgentRunStatusCompleted, Summary: "done",
			RequireInterventionCount: &zero, Output: map[string]interface{}{"summary": "done"},
		}); !errors.Is(err, models.ErrSteeringArrived) {
			t.Fatalf("stale intervention count: %v", err)
		}
		stored, err := store.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != models.AgentRunStatusRunning || stored.Revision != run.Revision || stored.Output != nil {
			t.Fatalf("guarded transition wrote: %#v", stored)
		}
		if len(stored.PendingInterventions) != 1 || stored.PendingInterventions[0].ActionCallID != "call-1" {
			t.Fatalf("intervention action call id lost: %#v", stored.PendingInterventions)
		}
		one := 1
		done, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, runtimeactivity.RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: models.AgentRunStatusCompleted, Summary: "done",
			RequireInterventionCount: &one, Output: map[string]interface{}{"summary": "done"},
		})
		if err != nil || done.Status != models.AgentRunStatusCompleted {
			t.Fatalf("matching intervention count: %#v %v", done, err)
		}
	})
}

func TestTerminalCreditErrorFromFailureExplanation(t *testing.T) {
	credits := &models.RunFailureExplanation{Kind: "credits", Code: models.RunCreditErrorWorkspaceCreditsExhausted, Message: "The Workspace is out of credits."}
	if got := models.TerminalCreditError(models.AgentRunStatusFailed, credits); got == nil || got.Code != models.RunCreditErrorWorkspaceCreditsExhausted {
		t.Fatalf("credit failure: %#v", got)
	}
	for name, test := range map[string]struct {
		status  models.AgentRunStatus
		failure *models.RunFailureExplanation
	}{
		"not failed":    {models.AgentRunStatusCompleted, credits},
		"no failure":    {models.AgentRunStatusFailed, nil},
		"other failure": {models.AgentRunStatusFailed, &models.RunFailureExplanation{Kind: "execution", Code: "provider_rate_limited", Message: "busy"}},
	} {
		if got := models.TerminalCreditError(test.status, test.failure); got != nil {
			t.Fatalf("%s: %#v", name, got)
		}
	}
	runFieldsBackends(t, func(t *testing.T, store runFieldsStore) {
		ctx := context.Background()
		activity := runtimeactivity.NewRunActivityService(store, store)
		run := runFieldsRun(t, store, "")
		run, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, runtimeactivity.RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning, Summary: "started",
		})
		if err != nil {
			t.Fatal(err)
		}
		failed, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, runtimeactivity.RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: models.AgentRunStatusFailed, Summary: "failed",
			Error: credits.Message, FailureExplanation: credits,
		})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range []*models.AgentRun{failed, stored} {
			if got.CreditError == nil || got.CreditError.Code != models.RunCreditErrorWorkspaceCreditsExhausted ||
				!reflect.DeepEqual(got.FailureExplanation, credits) {
				t.Fatalf("credit stop: %#v %#v", got.CreditError, got.FailureExplanation)
			}
		}
	})
}

func TestRunFailureExplanationJSON(t *testing.T) {
	explanation := models.RunFailureExplanation{Kind: "execution", Code: "execution_failed", Message: "The step failed.",
		StepError: "timeout", StepErrorCode: "step_host_unavailable", Explained: true}
	data, err := json.Marshal(explanation)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"execution","code":"execution_failed","message":"The step failed.","stepError":"timeout","stepErrorCode":"step_host_unavailable","explained":true}`
	if string(data) != want {
		t.Fatalf("json:\n got %s\nwant %s", data, want)
	}
	data, _ = json.Marshal(models.RunFailureExplanation{Kind: "budget", Code: "budget_exhausted", Message: "Out of budget."})
	if string(data) != `{"kind":"budget","code":"budget_exhausted","message":"Out of budget."}` {
		t.Fatalf("omitempty json: %s", data)
	}
	runFieldsBackends(t, func(t *testing.T, store runFieldsStore) {
		ctx := context.Background()
		activity := runtimeactivity.NewRunActivityService(store, store)
		run := runFieldsRun(t, store, "session-json")
		run, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, runtimeactivity.RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning, Summary: "started",
		})
		if err != nil {
			t.Fatal(err)
		}
		pending := explanation
		pending.Explained = false
		if _, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, runtimeactivity.RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: models.AgentRunStatusQueued, Summary: "explaining", FailureExplanation: &pending,
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
