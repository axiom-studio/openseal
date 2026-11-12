package agentbuilder_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder/agentbuildertest"
)

func TestAgentTrialStartsChildWithoutDefinition(t *testing.T) {
	f := agentbuildertest.NewDispatchFixture(t)
	ctx := context.Background()
	before, err := f.Agents.ListDeployments(ctx, kernelagent.AgentDeploymentFilter{Scope: models.ScopeReference(f.Scope)})
	if err != nil {
		t.Fatal(err)
	}
	_, child := agentbuildertest.StartTrial(t, f, "trial-call")
	if child.ID != agentbuilder.AgentTrialRunID(f.Run.ID, "trial-call") {
		t.Fatalf("trial child id = %q", child.ID)
	}

	if _, err := f.Agents.GetDefinition(ctx, agentbuilder.AgentBuilderResourceID("agent-definition", f.Scope, "trial-call"), agentbuilder.AgentBuilderDefinitionVersion); !errors.Is(err, kernelagent.ErrDefinitionNotFound) {
		t.Fatalf("try_agent registered a definition: %v", err)
	}
	after, err := f.Agents.ListDeployments(ctx, kernelagent.AgentDeploymentFilter{Scope: models.ScopeReference(f.Scope)})
	if err != nil || len(after) != len(before) {
		t.Fatalf("deployments %d -> %d, %v", len(before), len(after), err)
	}
	if child.Kind != models.RunKindAgentWork || child.ParentRunID != f.Run.ID || child.AssignedAgentID != "finn" ||
		child.Owner != f.Run.Owner || child.Source != models.RunSourceHandoff || child.Status != models.AgentRunStatusQueued ||
		child.Goal != "What would you do with an angry refund mail?" {
		t.Fatalf("child = %#v", child)
	}
	if child.Budget == nil || child.Budget.MaxTurns != agentbuilder.AgentTrialMaxTurns || child.Budget.MaxOutputTokens != 0 {
		t.Fatalf("child budget = %#v", child.Budget)
	}
	if child.Context[agentbuilder.AgentTrialModelContextKey] != "gpt-6-luna" {
		t.Fatalf("child model = %#v", child.Context)
	}
	trial, ok, err := agentbuilder.AgentTrialContextForRun(child)
	if err != nil || !ok || trial.ParentRunID != f.Run.ID || trial.ActionCallID != "trial-call" ||
		trial.Profile.SystemPrompt != "Read new support mail and sort it by urgency." || trial.Profile.DisplayName != "Inbox Triage" ||
		trial.Profile.Personality != "Friendly" || !slices.Equal(trial.Profile.OperatingPrinciples, []string{"Flag anything legal."}) ||
		trial.Profile.Model != "" {
		t.Fatalf("trial context = %#v ok=%v err=%v", trial, ok, err)
	}
	if conversationID, _ := child.Context[models.ConversationRunConversationIDContextKey].(string); conversationID != "" {
		t.Fatalf("trial child posts to a chat: %#v", child.Context)
	}

	// A replay of the call finds the same child.
	arguments := f.Resolve(t, models.AgentActionTryAgent, agentbuildertest.TrialArguments())
	again, err := f.Execute(t, models.AgentActionTryAgent, "trial-call", arguments, agentbuildertest.Approver)
	if err != nil || again["trialRunId"] != child.ID {
		t.Fatalf("replay = %#v, %v", again, err)
	}
	children, err := f.Store.ListAgentRuns(ctx, models.AgentRunFilter{Scope: f.Scope, ParentRunID: f.Run.ID})
	if err != nil || len(children) != 1 {
		t.Fatalf("children = %d, %v", len(children), err)
	}
}

func TestAgentTrialRefusesSensitiveMessage(t *testing.T) {
	f := agentbuildertest.NewDispatchFixture(t)
	arguments := agentbuildertest.TrialArguments()
	arguments["message"] = "Log in with password: hunter2-correct-horse"
	if _, err := f.Propose(t, models.AgentActionTryAgent, arguments); err == nil {
		t.Fatal("sensitive trial message accepted")
	}
}

func TestAgentTrialFinalizerHeldParentKeepsStatus(t *testing.T) {
	f := agentbuildertest.NewTrialFinalizerFixture(t, models.AgentRunStatusRunning)
	child := f.EndChild(t, models.AgentRunStatusFailed, nil, "model unavailable")
	if err := agentbuilder.NewAgentTrialFinalizer(f.Store, nil, nil).FinalizeRun(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	parent := agentbuildertest.MustRun(t, f.Store, f.Scope, f.Parent.ID)
	if parent.Status != models.AgentRunStatusRunning || parent.Revision != f.Parent.Revision+1 {
		t.Fatalf("parent = %s rev %d (was %d)", parent.Status, parent.Revision, f.Parent.Revision)
	}
	receipt := agentbuilder.AgentTrialReceiptFor(f.Call.ID, child.ID, parent.PendingInterventions)
	if receipt == nil || receipt.Status != agentbuilder.AgentTrialStatusFailed || receipt.FailureMessage != "model unavailable" {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestAgentTrialFinalizerIgnoresOtherRuns(t *testing.T) {
	f := agentbuildertest.NewTrialFinalizerFixture(t, models.AgentRunStatusWaitingForEvent)
	// A canceled trial reaches only a parent waiting on it; the parent that
	// canceled it at the deadline already reported timed_out.
	parent := agentbuildertest.MustRun(t, f.Store, f.Scope, f.Parent.ID)
	parent.Status, parent.WakeCondition = models.AgentRunStatusRunning, nil
	agentbuildertest.PutTrialRun(t, f.Store, parent)
	child := f.EndChild(t, models.AgentRunStatusCanceled, nil, "")
	finalizer := agentbuilder.NewAgentTrialFinalizer(f.Store, nil, nil)
	if err := finalizer.FinalizeRun(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	// A Run without a trial context is not touched.
	other := models.CloneAgentRun(parent)
	other.ID, other.Status, other.Context = "other", models.AgentRunStatusCompleted, nil
	if err := finalizer.FinalizeRun(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if after := agentbuildertest.MustRun(t, f.Store, f.Scope, f.Parent.ID); after.Revision != parent.Revision || len(after.PendingInterventions) != len(parent.PendingInterventions) {
		t.Fatalf("parent changed: %#v", after)
	}
}
