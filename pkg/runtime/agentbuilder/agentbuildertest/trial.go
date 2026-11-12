package agentbuildertest

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/google/uuid"
)

// TrialArguments are try_agent arguments for an inbox triage draft.
func TrialArguments() map[string]interface{} {
	return map[string]interface{}{
		"displayName":         "Inbox Triage",
		"purpose":             "Sort the support inbox every morning.",
		"systemPrompt":        "Read new support mail and sort it by urgency.",
		"personality":         "Friendly",
		"operatingPrinciples": []interface{}{"Flag anything legal."},
		"model":               "gpt-6-luna",
		"message":             "  What would you do with an angry refund mail?  ",
	}
}

// PutTrialRun stores run in whatever state it is in, bypassing
// transition rules: it creates the Run, or saves it as the next revision of
// the stored one and sets run.Revision to that revision.
func PutTrialRun(t *testing.T, store *sqlite.Store, run *models.AgentRun) *models.AgentRun {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil {
		if err := store.CreateAgentRun(ctx, models.CloneAgentRun(run)); err != nil {
			t.Fatalf("create run %s: %v", run.ID, err)
		}
		return run
	}
	run.Revision = current.Revision + 1
	event := &models.ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, EventType: "run.updated",
		Summary: "Put agent trial test run", Actor: models.ActivityActor{Type: "test", ID: "agent-trial"},
		Visibility: models.ActivityVisibilityScope, CreatedAt: time.Now().UTC()}
	if _, err := store.UpdateAgentRunWithEvent(ctx, models.CloneAgentRun(run), current.Revision, event, nil); err != nil {
		t.Fatalf("update run %s: %v", run.ID, err)
	}
	return run
}

// MustRun reads the Run id, failing the test when it is missing.
func MustRun(t *testing.T, store models.PortfolioStore, scope models.Scope, id string) *models.AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil || run == nil {
		t.Fatalf("run %s = %#v, %v", id, run, err)
	}
	return run
}

// StartTrial dispatches try_agent for the fixture's builder Run.
func StartTrial(t *testing.T, f *DispatchFixture, callID string) (*models.ActionCall, *models.AgentRun) {
	t.Helper()
	arguments := f.Resolve(t, models.AgentActionTryAgent, TrialArguments())
	output, err := f.Execute(t, models.AgentActionTryAgent, callID, arguments, Approver)
	if err != nil {
		t.Fatal(err)
	}
	childID, _ := output["trialRunId"].(string)
	if output["resourceType"] != "agent_trial" || output["status"] != "running" || childID == "" {
		t.Fatalf("try_agent output = %#v", output)
	}
	call := &models.ActionCall{
		ID: callID, Scope: f.Scope, RunID: f.Run.ID, DeploymentID: "finn", SkillID: models.AgentManagementSkillID,
		SkillVersion: models.AgentManagementSkillVersion, Action: models.AgentActionTryAgent, Status: models.ActionCallStatusSucceeded,
		Origin: models.ActionOriginLoop, Output: output,
	}
	return call, MustRun(t, f.Store, f.Scope, childID)
}

// TrialFinalizerFixture is a builder Run waiting on its trial child.
type TrialFinalizerFixture struct {
	Store  *sqlite.Store
	Scope  models.Scope
	Parent *models.AgentRun
	Child  *models.AgentRun
	Call   *models.ActionCall
}

// NewTrialFinalizerFixture starts a trial and leaves its parent in
// parentStatus, parked on the trial deadline when it waits for an event.
func NewTrialFinalizerFixture(t *testing.T, parentStatus models.AgentRunStatus) *TrialFinalizerFixture {
	t.Helper()
	f := NewDispatchFixture(t)
	call, child := StartTrial(t, f, "trial-call")
	parent := MustRun(t, f.Store, f.Scope, f.Run.ID)
	parent.Status = parentStatus
	if parentStatus == models.AgentRunStatusWaitingForEvent {
		deadline := agentbuilder.AgentTrialDeadline(child)
		parent.WakeCondition = &models.WakeCondition{Type: agentrun.AgentTrialWakeType, Reference: child.ID, WakeAt: &deadline}
	}
	PutTrialRun(t, f.Store.Store, parent)
	return &TrialFinalizerFixture{Store: f.Store.Store, Scope: f.Scope, Parent: parent, Child: child, Call: call}
}

func (f *TrialFinalizerFixture) EndChild(t *testing.T, status models.AgentRunStatus, output map[string]interface{}, message string) *models.AgentRun {
	t.Helper()
	child := MustRun(t, f.Store, f.Scope, f.Child.ID)
	ended := child.CreatedAt.Add(30 * time.Second)
	child.Status, child.Output, child.Error, child.CompletedAt, child.UpdatedAt = status, output, message, &ended, ended
	PutTrialRun(t, f.Store, child)
	return child
}
