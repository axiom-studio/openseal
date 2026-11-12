package runtime

// Run-loop scenario for the budget of a conversation task (run-loop section
// 12 §5.8, tests-T17b; ported from hosted_turn_task_test.go): a bounded chat
// Run funds its task from what it has left, and cannot start one when what it
// has left is below the child minimum (A18, controlMinimumChildTurns).

import (
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/google/uuid"
)

// taskMinimumTurns is the fewest Turns a task's Run may be given (the
// control child minimum); a source with less left cannot start one.
const taskMinimumTurns int64 = 8

// taskBudgetChat starts a chat with a first exchange, then posts the task
// request and bounds its Run with budget before that Run's first claim. The
// first exchange is there because the loop plans a bounded step's budget from
// the session before the chat preparer appends the trigger, and an empty lane
// session cannot be planned (chat Runs are unbounded in production, so only
// this scenario bounds one).
func taskBudgetChat(c *childFixture, budget BudgetPolicy) (*Conversation, *AgentRun) {
	c.T.Helper()
	conversation, first := c.StartChat("hello finn")
	if _, done := c.drive(first.ID); done.Status != AgentRunStatusCompleted {
		c.T.Fatalf("first exchange = %s (%#v)", done.Status, done.FailureExplanation)
	}
	run := convSchedule(c.Fixture, conversation.ID, convPost(c.Fixture, conversation.ID, "compile the quarterly report please", nil))
	c.Clock.Advance(time.Second)
	current := c.MustRun(run.ID)
	bounded := cloneAgentRun(current)
	bounded.Budget = &budget
	bounded.Revision++
	bounded.UpdatedAt = c.Clock.Now()
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: c.Scope, EventType: "run.budget_set", Severity: ActivitySeverityInfo, AgentID: looptest.FixtureAgent,
		RunID: run.ID, Actor: ActivityActor{Type: "user", ID: looptest.FixtureUser}, Summary: "Budget set", Visibility: ActivityVisibilityScope,
		CreatedAt: c.Clock.Now(),
	}
	if _, err := c.Raw.UpdateAgentRunWithEvent(c.Ctx, bounded, current.Revision, event, nil); err != nil {
		c.T.Fatalf("bound chat run: %v", err)
	}
	return conversation, c.MustRun(run.ID)
}

// TestLoopTaskAuthorityRequiresChildBudget: a bounded chat Run whose
// remaining Turns cover the child minimum starts the task with exactly the
// remaining capacity; one below it gets a task_rejected tool error and
// answers in the chat instead.
func TestLoopTaskAuthorityRequiresChildBudget(t *testing.T) {
	const ack = "On it, I will compile the report in the background."
	const inline = "I can't start background work with the budget left, so here is what I have: revenue grew 4%."
	newTaskFixture := func(t *testing.T, store KernelStore) *childFixture {
		c := newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "task-work", When: func(v looptest.StepView) bool {
				return strings.Contains(v.LastUserText(), "Compile the quarterly report") && !strings.Contains(v.LastUserText(), "please")
			}, Reply: func(looptest.StepView) ModelStepResult {
				return looptest.Say("The quarterly report is ready: revenue grew 4%.")
			}},
			&looptest.ModelRule{Name: "hello", When: func(v looptest.StepView) bool { return strings.Contains(v.LastUserText(), "hello finn") },
				Reply: func(looptest.StepView) ModelStepResult { return looptest.Say("Hi Ana.") }},
			&looptest.ModelRule{Name: "inline", When: portLastToolFailed, Reply: func(looptest.StepView) ModelStepResult { return looptest.Say(inline) }},
			&looptest.ModelRule{Name: "start", Times: 1, Reply: func(looptest.StepView) ModelStepResult {
				return looptest.Act("", looptest.Call(string(ControlToolStartTask), map[string]any{"goal": "Compile the quarterly report", "acknowledgment": ack}))
			}},
		))
		c.authority = func(run *AgentRun, a *agentrun.ControlAuthority) {
			if run.Kind == RunKindConversation && run.Context[ConversationTaskContextKey] == nil {
				conversationID, _ := run.Context[conversationRunContextConversationID].(string)
				a.TaskStart = &HostedConversationTaskContext{ConversationID: conversationID, CanStart: true}
			}
		}
		return c
	}
	tasksOf := func(c *childFixture, conversation *Conversation, source *AgentRun) []*ConversationTaskResult {
		c.T.Helper()
		tasks, err := c.Chats.(ConversationTaskKernelStore).ListConversationTasks(c.Ctx, ConversationTaskFilter{
			Scope: c.Scope, Owner: source.Owner, ConversationID: conversation.ID, Limit: 10,
		})
		if err != nil {
			c.T.Fatal(err)
		}
		return tasks
	}

	t.Run("remaining_capacity_funds_the_task", func(t *testing.T) {
		looptest.EachStore(t, "task_budget_funded", func(t *testing.T, store KernelStore) {
			c := newTaskFixture(t, store)
			// One Turn goes to the step that starts the task; the remaining 8
			// meet the child minimum exactly.
			conversation, source := taskBudgetChat(c, BudgetPolicy{MaxTurns: taskMinimumTurns + 1})
			_, ended := c.drive(source.ID)
			if ended.Status != AgentRunStatusCompleted {
				t.Fatalf("source = %s (%#v), log %v", ended.Status, ended.FailureExplanation, c.EntryTypes(source.ID))
			}
			start := c.toolEntry(source.ID, string(ControlToolStartTask))
			if start.State != ToolEntryCompleted || start.Tool.Result.IsError {
				t.Fatalf("start entry = %s %q", start.State, childResultText(start))
			}
			tasks := tasksOf(c, conversation, source)
			if len(tasks) != 1 || tasks[0].WorkRun == nil {
				t.Fatalf("tasks = %#v", tasks)
			}
			work := c.MustRun(tasks[0].WorkRun.ID)
			if work.Budget == nil || work.Budget.MaxTurns != taskMinimumTurns || work.Budget.MaxInputTokens != 0 || work.Budget.MaxCostMicros != 0 {
				t.Fatalf("task budget = %#v, want the remaining %d Turns and nothing else bounded", work.Budget, taskMinimumTurns)
			}
			allocation, ok := c.MustRun(source.ID).BudgetAllocations[work.ID]
			if !ok || allocation.MaxTurns != taskMinimumTurns {
				t.Fatalf("source allocations = %#v, want the task's %d Turns", c.MustRun(source.ID).BudgetAllocations, taskMinimumTurns)
			}
			messages := c.Messages(conversation.ID)
			if len(messages) != 4 || messages[3].Content != ack {
				t.Fatalf("channel = %s", looptest.DescribeMessages(messages))
			}
		})
	})

	t.Run("below_child_minimum_is_a_tool_error", func(t *testing.T) {
		looptest.EachStore(t, "task_budget_short", func(t *testing.T, store KernelStore) {
			c := newTaskFixture(t, store)
			conversation, source := taskBudgetChat(c, BudgetPolicy{MaxTurns: taskMinimumTurns})
			_, ended := c.drive(source.ID)
			if ended.Status != AgentRunStatusCompleted {
				t.Fatalf("source = %s (%#v), log %v", ended.Status, ended.FailureExplanation, c.EntryTypes(source.ID))
			}
			start := c.toolEntry(source.ID, string(ControlToolStartTask))
			if start.State != ToolEntryError || !start.Tool.Result.IsError || !strings.Contains(childResultText(start), "task_rejected") ||
				!strings.Contains(childResultText(start), "below the child minimum") {
				t.Fatalf("start entry = %s %q, want a task_rejected budget error", start.State, childResultText(start))
			}
			if tasks := tasksOf(c, conversation, source); len(tasks) != 0 {
				t.Fatalf("an unfundable task was admitted: %#v", tasks)
			}
			if children := c.children(source.ID); len(children) != 0 {
				t.Fatalf("children = %d", len(children))
			}
			if done := c.MustRun(source.ID); len(done.BudgetAllocations) != 0 || done.BudgetUsage.Turns != 2 {
				t.Fatalf("source budget = usage %#v allocations %#v", done.BudgetUsage, done.BudgetAllocations)
			}
			messages := c.Messages(conversation.ID)
			if len(messages) != 4 || messages[3].Content != inline || messages[3].Sender.ID != looptest.FixtureAgent {
				t.Fatalf("channel = %s, want the inline answer", looptest.DescribeMessages(messages))
			}
		})
	})
}
