package runloop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder/agentbuildertest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

func TestAgentTrialParksParentUntilDeadline(t *testing.T) {
	f := agentbuildertest.NewDispatchFixture(t)
	call, child := agentbuildertest.StartTrial(t, f, "trial-call")
	now := child.CreatedAt.Add(time.Minute)
	parker := newGovernedParker(f.Store, nil, func() time.Time { return now })

	wake, summary, err := parker.governedCardWait(context.Background(), f.Run, call)
	if err != nil {
		t.Fatal(err)
	}
	deadline := child.CreatedAt.Add(5 * time.Minute) // the trial timeout
	if wake == nil || wake.Type != agentrun.AgentTrialWakeType || wake.Reference != child.ID || wake.WakeAt == nil ||
		!wake.WakeAt.Equal(deadline) || summary != "Trying the draft" {
		t.Fatalf("wake = %#v %q", wake, summary)
	}
	// Past the deadline the Run does not park again: the resume times out.
	now = deadline
	if wake, _, err := parker.governedCardWait(context.Background(), f.Run, call); err != nil || wake != nil {
		t.Fatalf("wake past deadline = %#v, %v", wake, err)
	}
	// A finished trial never parks.
	now = child.CreatedAt.Add(time.Minute)
	child.Status = models.AgentRunStatusCompleted
	agentbuildertest.PutTrialRun(t, f.Store.Store, child)
	if wake, _, err := parker.governedCardWait(context.Background(), f.Run, call); err != nil || wake != nil {
		t.Fatalf("wake after completion = %#v, %v", wake, err)
	}
}

func TestAgentTrialFinalizerDeliversReceiptAndQueuesParent(t *testing.T) {
	f := agentbuildertest.NewTrialFinalizerFixture(t, models.AgentRunStatusWaitingForEvent)
	child := f.EndChild(t, models.AgentRunStatusCompleted, map[string]interface{}{agentbuilder.FinalOutputReportKey: "I would apologise and refund."}, "")
	var woken []models.Scope
	nextCalls := 0
	next := agentrun.RunTerminalFinalizerFunc(func(context.Context, *models.AgentRun) error { nextCalls++; return nil })
	finalizer := agentbuilder.NewAgentTrialFinalizer(f.Store, next, func(scope models.Scope) { woken = append(woken, scope) })
	for range 2 {
		if err := finalizer.FinalizeRun(context.Background(), child); err != nil {
			t.Fatal(err)
		}
	}
	parent := agentbuildertest.MustRun(t, f.Store, f.Scope, f.Parent.ID)
	if parent.Status != models.AgentRunStatusQueued || parent.WakeCondition != nil {
		t.Fatalf("parent = %s %#v", parent.Status, parent.WakeCondition)
	}
	receipts := 0
	for _, intervention := range parent.PendingInterventions {
		if intervention.AgentTrial != nil {
			receipts++
			if intervention.ActionCallID != f.Call.ID || *intervention.AgentTrial != (models.AgentTrialReceipt{
				RunID: child.ID, Status: agentbuilder.AgentTrialStatusCompleted, Answer: "I would apologise and refund.",
			}) {
				t.Fatalf("intervention = %#v receipt=%#v", intervention, intervention.AgentTrial)
			}
		}
	}
	if receipts != 1 || len(woken) != 1 || woken[0] != f.Scope || nextCalls != 2 {
		t.Fatalf("receipts=%d woken=%v next=%d", receipts, woken, nextCalls)
	}

	// The resume renders the receipt as the tool result.
	outcome := settleAgentTrialCall(t, f, parent, parent.PendingInterventions, time.Now())
	output := agentTrialResultOutput(t, outcome)
	if outcome.State != models.ToolEntryCompleted || output["status"] != agentbuilder.AgentTrialStatusCompleted ||
		output["reply"] != "I would apologise and refund." || output["trialRunId"] != child.ID {
		t.Fatalf("outcome = %#v output=%#v", outcome, output)
	}
}

func TestAgentTrialResumeRendersFailure(t *testing.T) {
	f := agentbuildertest.NewTrialFinalizerFixture(t, models.AgentRunStatusRunning)
	child := f.EndChild(t, models.AgentRunStatusFailed, nil, "")
	child.FailureExplanation = &models.RunFailureExplanation{Kind: conversation.RunFailureKindExecution, Code: "model", Message: "The model is unavailable."}
	agentbuildertest.PutTrialRun(t, f.Store, child)
	// No receipt (the finalizer missed it): the resume reads the child.
	outcome := settleAgentTrialCall(t, f, f.Parent, nil, time.Now())
	output := agentTrialResultOutput(t, outcome)
	if output["status"] != agentbuilder.AgentTrialStatusFailed || output["error"] != "The model is unavailable." || output["trialRunId"] != child.ID {
		t.Fatalf("output = %#v", output)
	}
}

func TestAgentTrialResumeReadsMissedChild(t *testing.T) {
	f := agentbuildertest.NewTrialFinalizerFixture(t, models.AgentRunStatusRunning)
	const replyLimit = 8 << 10 // the trial reply bound
	f.EndChild(t, models.AgentRunStatusCompleted, map[string]interface{}{agentbuilder.FinalOutputReportKey: strings.Repeat("a", replyLimit+100)}, "")
	outcome := settleAgentTrialCall(t, f, f.Parent, nil, time.Now())
	output := agentTrialResultOutput(t, outcome)
	reply, _ := output["reply"].(string)
	if output["status"] != agentbuilder.AgentTrialStatusCompleted || len(reply) > replyLimit || !strings.Contains(reply, "truncated") {
		t.Fatalf("output status=%v reply=%d bytes", output["status"], len(reply))
	}
}

func TestAgentTrialTimerWakeCancelsRunningChild(t *testing.T) {
	f := agentbuildertest.NewTrialFinalizerFixture(t, models.AgentRunStatusWaitingForEvent)
	ctx := context.Background()
	deadline := agentbuilder.AgentTrialDeadline(f.Child)

	// The timer wakes the waiting parent only at the deadline.
	wakes := agentrun.NewWakeService(f.Store, f.Store)
	early, err := wakes.WakeDueTimers(ctx, f.Scope, deadline.Add(-time.Second))
	if err != nil || len(early.Runs) != 0 || early.NextWakeAt == nil || !early.NextWakeAt.Equal(deadline) {
		t.Fatalf("early wake = %#v, %v", early, err)
	}
	due, err := wakes.WakeDueTimers(ctx, f.Scope, deadline.Add(time.Second))
	if err != nil || len(due.Runs) != 1 || due.Runs[0].Run.ID != f.Parent.ID || due.Runs[0].Run.Status != models.AgentRunStatusQueued {
		t.Fatalf("due wake = %#v, %v", due, err)
	}

	// The resume cancels the child that is still running and times out.
	parent := agentbuildertest.MustRun(t, f.Store, f.Scope, f.Parent.ID)
	outcome := settleAgentTrialCall(t, f, parent, nil, deadline.Add(time.Second))
	output := agentTrialResultOutput(t, outcome)
	if outcome.State != models.ToolEntryCompleted || output["status"] != agentbuilder.AgentTrialStatusTimedOut || output["trialRunId"] != f.Child.ID {
		t.Fatalf("outcome = %#v output=%#v", outcome, output)
	}
	if child := agentbuildertest.MustRun(t, f.Store, f.Scope, f.Child.ID); child.Status != models.AgentRunStatusCanceled {
		t.Fatalf("child = %s", child.Status)
	}
}

// settleAgentTrialCall settles the fixture's try_agent call for parent at
// now, as the loop's resume does.
func settleAgentTrialCall(t *testing.T, f *agentbuildertest.TrialFinalizerFixture, parent *models.AgentRun, settled []models.AgentRunIntervention, now time.Time) GovernedToolOutcome {
	t.Helper()
	clock := func() time.Time { return now }
	executor := &GovernedToolExecutor{governedParker: newGovernedParker(f.Store, nil, clock), store: f.Store, now: clock}
	call := GovernedToolCall{Run: parent, ToolCallID: "tool-1", chain: &governedRunChain{known: parent}}
	outcome, err := executor.settle(context.Background(), call, f.Call, settled, false)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Park != nil {
		t.Fatalf("settled trial parked: %#v", outcome.Park)
	}
	return outcome
}

func agentTrialResultOutput(t *testing.T, outcome GovernedToolOutcome) map[string]interface{} {
	t.Helper()
	if outcome.Result == nil || len(outcome.Result.Content) == 0 {
		t.Fatalf("outcome has no result: %#v", outcome)
	}
	var value struct {
		Output map[string]interface{} `json:"output"`
	}
	if err := json.Unmarshal(outcome.Result.Content[0].Value, &value); err != nil {
		t.Fatal(err)
	}
	return value.Output
}
