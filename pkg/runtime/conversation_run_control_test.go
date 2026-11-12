package runtime

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop/runlooptest"
)

// The chat binding offers the run-control actions whether or not the chat
// has active work, and always with the catalog schema: no per-step run id
// enum and no tool appearing with work (both broke the prompt cache). A run
// id that is not active work is refused at call time instead
// (conversationrun/conversation_loop.go checkToolCall).
func TestConversationRunBindingOffersRunControlWithAndWithoutActiveWork(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		definition := agentrun.RunManagementSkill()
		cancel := capability.ModelAction{
			Name: "openseal.runs.cancel", SkillID: RunManagementSkillID, Version: agentrun.RunManagementSkillVersion,
			Action: RunActionCancel, InputSchema: definition.Actions[RunActionCancel].InputSchema,
		}
		other := capability.ModelAction{Name: "openseal.objectives.create", SkillID: "openseal.objectives", Version: "1.0.0", Action: "create"}
		resolver := &chatRunResolver{store: f.Store, producer: &runlooptest.ChatProducer{}, actions: []capability.ModelAction{cancel, other}}
		runner := f.ChatRunner(resolver, false)
		run := f.StartRun(f.Post("control", "user-1", "Stop the review."))
		binding, err := runner.ResolveTurnRunner(f.Ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if len(binding.ModelActions) != 2 || binding.ModelActions[0].SkillID != RunManagementSkillID {
			t.Fatalf("actions without active work = %#v", binding.ModelActions)
		}
		now := time.Now().UTC()
		child := &AgentRun{
			ID: "review-" + run.ID, Kind: RunKindAgentWork, Scope: run.Scope, Owner: run.Owner, AssignedAgentID: run.AssignedAgentID,
			ParentRunID: run.ID, RootRunID: run.ID, Goal: "Review the release", Source: RunSourceFork,
			Status: AgentRunStatusQueued, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := f.Store.CreateAgentRun(f.Ctx, child); err != nil {
			t.Fatal(err)
		}
		binding, err = runner.ResolveTurnRunner(f.Ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if len(binding.ModelActions) != 2 || binding.ModelActions[0].SkillID != RunManagementSkillID {
			t.Fatalf("actions with active work = %#v", binding.ModelActions)
		}
		runID := binding.ModelActions[0].InputSchema["properties"].(map[string]interface{})["runId"].(map[string]interface{})
		if _, enum := runID["enum"]; enum {
			t.Fatalf("run id schema carries a per-step enum: %#v", runID)
		}
	})
}
