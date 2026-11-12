package runtime

// Run-loop scenarios, child sessions (run-loop section 12 §5.8, A3/A4): forks,
// delegation through decision/execution/review Runs, conversation tasks,
// foreground promotion and runbook operations, driven through the real
// runloop.Loop by the loop fixture plus a small worker: the claim of the next
// queued Run, the binding the worker's resolver gives it (ResolveLoopBinding
// shape: a Run bound to a session keeps it, any other Run gets its run session
// at first claim, seeded with its brief), and the terminal-child resolution
// the worker pool runs after a claim (fork edges, AgentRequest decisions,
// reviews and children, terminal reporting).
//
// The runbook scenarios read the runbook Run's engine state, so this file is
// exempt from TestLoopTestkitHasNoCheckpointAccess by name.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop/runlooptest"
)

// ---------------------------------------------------------------------------
// Child fixture

const (
	childScout    = "scout"
	childReviewer = "rhea"
	childTeam     = "crew"
)

// ---------------------------------------------------------------------------
// Reading

// ---------------------------------------------------------------------------
// Forks

func TestLoopForkCreatesChildSessionAndResumesWithItsFinalText(t *testing.T) {
	looptest.EachStore(t, "child_fork", func(t *testing.T, store models.KernelStore) {
		answers := map[string]string{"count items": "2 items", "count boxes": "3 boxes"}
		c := newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "branch", When: isForkBranch, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Say(answers[branchGoal(v)])
			}},
			&looptest.ModelRule{Name: "fork", Times: 1, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Counting in parallel.", childForkCall("count items", "count boxes"))
			}},
			&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) models.ModelStepResult {
				content, isError, ok := v.ToolResult(string(models.ControlToolFork))
				if !ok || isError || !strings.Contains(content, "2 items") {
					return looptest.Say("The count failed.")
				}
				return looptest.Say("There are 2 items and 3 boxes.")
			}},
		))
		parent := c.StartRun(models.RunKindAgentWork, "Count the stock")

		// The fork parks the parent and releases its claim.
		if exit, _ := c.drive(parent.ID); exit.Reason != runloop.ExitParked {
			t.Fatalf("parent exit = %s", exit.Reason)
		}
		waiting := c.MustRun(parent.ID)
		if waiting.Status != models.AgentRunStatusWaitingForDependency || waiting.LeaseOwner != "" {
			t.Fatalf("parent = %s lease %q", waiting.Status, waiting.LeaseOwner)
		}
		forkEntry := c.toolEntry(parent.ID, string(models.ControlToolFork))
		if forkEntry.State != models.ToolEntryAwaitingChild {
			t.Fatalf("fork entry = %s", forkEntry.State)
		}
		parentPath := c.path(waiting.SessionID)
		var assistant *models.SessionEntry
		for _, e := range parentPath {
			if e.Type == models.SessionEntryAssistant && e.StepID == forkEntry.StepID {
				assistant = e
			}
		}
		if assistant == nil {
			t.Fatalf("no forking assistant entry in %v", entryIDs(parentPath))
		}
		children := c.children(parent.ID)
		if len(children) != 2 {
			t.Fatalf("children = %d, want 2", len(children))
		}
		// A3: the child session forks just before the forking assistant entry.
		var beforeFork []string
		for _, e := range parentPath {
			if e.Sequence < assistant.Sequence {
				beforeFork = append(beforeFork, e.ID)
			}
		}
		for _, child := range children {
			if child.SessionID == "" || child.SessionID == waiting.SessionID {
				t.Fatalf("child %s session = %q", child.ID, child.SessionID)
			}
			session := c.sessionByID(child.SessionID)
			if session.ParentSessionID != waiting.SessionID || session.ForkSequence != assistant.Sequence-1 ||
				session.ForkEntryID != beforeFork[len(beforeFork)-1] || session.OwnerRunID != child.ID {
				t.Fatalf("child session = parent %q fork %d/%q owner %q; want %q %d/%q",
					session.ParentSessionID, session.ForkSequence, session.ForkEntryID, session.OwnerRunID,
					waiting.SessionID, assistant.Sequence-1, beforeFork[len(beforeFork)-1])
			}
		}

		c.work(10)
		done := c.MustRun(parent.ID)
		c.MustStatus(done, models.AgentRunStatusCompleted)

		// Each child's first request is the parent path up to the fork point,
		// then its own brief; it never sees the fork decision or later parent
		// entries.
		for _, child := range children {
			requests := c.requestsFor(child.ID)
			if len(requests) != 1 {
				t.Fatalf("child %s made %d requests", child.ID, len(requests))
			}
			ids := entryViewIDs(requests[0].Entries)
			if len(ids) < len(beforeFork)+1 || strings.Join(ids[:len(beforeFork)], ",") != strings.Join(beforeFork, ",") {
				t.Fatalf("child path = %v, want prefix %v", ids, beforeFork)
			}
			own := map[string]bool{}
			for _, e := range c.sessionEntries(child.SessionID) {
				if e.RunID != child.ID {
					t.Fatalf("child %s session holds entry %s of run %s", child.ID, e.ID, e.RunID)
				}
				own[e.ID] = true
			}
			for _, id := range ids[len(beforeFork):] {
				if !own[id] {
					t.Fatalf("child %s path holds %s after the fork point, which is not its own", child.ID, id)
				}
			}
			for _, id := range ids {
				if id == assistant.ID || id == forkEntry.ID {
					t.Fatalf("child path holds the fork step entry %s", id)
				}
			}
			if branchGoal(looptest.StepView{Req: requests[0]}) != child.Goal {
				t.Fatalf("child %s brief goal = %q, want %q", child.ID, branchGoal(looptest.StepView{Req: requests[0]}), child.Goal)
			}
		}

		// The parent's tool entry completed with the children's final texts.
		forkEntry = c.toolEntry(parent.ID, string(models.ControlToolFork))
		if forkEntry.State != models.ToolEntryCompleted || forkEntry.Tool.Result.IsError {
			t.Fatalf("fork entry = %s %q", forkEntry.State, childResultText(forkEntry))
		}
		value := childResultJSON(t, forkEntry)
		branches, _ := value["branches"].([]interface{})
		if len(branches) != 2 {
			t.Fatalf("fork result = %s", childResultText(forkEntry))
		}
		for i, want := range []string{"2 items", "3 boxes"} {
			if branch := branches[i].(map[string]interface{}); branch["answer"] != want || branch["status"] != string(models.AgentRunStatusCompleted) {
				t.Fatalf("branch %d = %v", i, branch)
			}
		}
		if got := done.Output[conversationrun.FinalOutputSummaryKey]; got != "There are 2 items and 3 boxes." {
			t.Fatalf("parent answer = %v", done.Output)
		}
		if c.claims[parent.ID] != 2 {
			t.Fatalf("parent claims = %d, want 2", c.claims[parent.ID])
		}
	})
}

func TestLoopTwoParallelForksJoinBeforeResume(t *testing.T) {
	looptest.EachStore(t, "child_fork_join", func(t *testing.T, store models.KernelStore) {
		c := newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "branch", When: isForkBranch, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Say("done: " + branchGoal(v))
			}},
			// Two fork calls in one step: the kernel runs one settle-phase
			// call per assistant entry and refuses the second.
			&looptest.ModelRule{Name: "fork", Times: 1, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Splitting the survey.", childForkCall("survey north", "survey south"), childForkCall("survey east", "survey west"))
			}},
			&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Both surveys are done.") }},
		))
		parent := c.StartRun(models.RunKindAgentWork, "Survey both regions")
		c.drive(parent.ID)
		children := c.children(parent.ID)
		if len(children) != 2 {
			t.Fatalf("children = %d", len(children))
		}
		forks := c.toolEntries(parent.ID, string(models.ControlToolFork))
		if len(forks) != 2 || forks[0].State != models.ToolEntryAwaitingChild || forks[1].State != models.ToolEntryError ||
			!strings.Contains(childResultText(forks[1]), "control_one_settle_call") {
			t.Fatalf("fork entries = %v / %q", c.EntryTypes(parent.ID), childResultText(forks[len(forks)-1]))
		}
		// The first child finishes; the parent stays parked.
		first := children[0]
		c.drive(first.ID)
		c.MustStatus(first, models.AgentRunStatusCompleted)
		if got := c.MustRun(parent.ID); got.Status != models.AgentRunStatusWaitingForDependency {
			t.Fatalf("parent woke after one of two branches: %s", got.Status)
		}
		if entry := c.toolEntries(parent.ID, string(models.ControlToolFork))[0]; entry.State != models.ToolEntryAwaitingChild {
			t.Fatalf("fork entry after one branch = %s", entry.State)
		}
		if n := len(c.requestsFor(parent.ID)); n != 1 {
			t.Fatalf("parent made %d model requests before the join", n)
		}
		c.drive(children[1].ID)
		if got := c.MustRun(parent.ID); got.Status != models.AgentRunStatusQueued {
			t.Fatalf("parent after both branches = %s", got.Status)
		}
		c.work(4)
		c.MustStatus(parent, models.AgentRunStatusCompleted)
		forks = c.toolEntries(parent.ID, string(models.ControlToolFork))
		entry := forks[0]
		branches, _ := childResultJSON(t, entry)["branches"].([]interface{})
		if entry.State != models.ToolEntryCompleted || len(branches) != 2 || forks[1].State != models.ToolEntryError || forks[0].Sequence > forks[1].Sequence {
			t.Fatalf("fork entries = %s %s / %s", entry.State, childResultText(entry), forks[1].State)
		}
		// Branch results come back in call order, not finish order.
		for i, want := range []string{"done: survey north", "done: survey south"} {
			if branch := branches[i].(map[string]interface{}); branch["answer"] != want || branch["branch"] != fmt.Sprintf("branch-%d", i+1) {
				t.Fatalf("branch %d = %v", i, branch)
			}
		}
		// The resumed step saw the joined result.
		requests := c.requestsFor(parent.ID)
		if len(requests) != 2 {
			t.Fatalf("parent requests = %d", len(requests))
		}
		results := (looptest.StepView{Req: requests[1]}).ToolResultsSinceUser()
		if len(results) != 2 || results[0].Tool.Result.IsError || !results[1].Tool.Result.IsError ||
			!strings.Contains(looptest.PartsText(results[0].Tool.Result.Content), "survey north") ||
			!strings.Contains(looptest.PartsText(results[0].Tool.Result.Content), "survey south") {
			t.Fatalf("resumed step: %s", looptest.StepView{Req: requests[1]}.Describe())
		}
	})
}

func TestLoopChildFailureIsAToolError(t *testing.T) {
	looptest.EachStore(t, "child_fork_failure", func(t *testing.T, store models.KernelStore) {
		c := newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "branch-fail", When: func(v looptest.StepView) bool { return isForkBranch(v) && branchGoal(v) == "read the ledger" },
				Err: modelstep.NewTurnHostFailure("step_invalid_request", "the ledger cannot be read", false)},
			&looptest.ModelRule{Name: "branch", When: isForkBranch, Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Inventory counted.") }},
			&looptest.ModelRule{Name: "fork", Times: 1, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Checking both.", childForkCall("read the ledger", "count the inventory"))
			}},
			&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) models.ModelStepResult {
				if _, isError, _ := v.ToolResult(string(models.ControlToolFork)); isError {
					return looptest.Say("One check failed.")
				}
				return looptest.Say("All checks passed.")
			}},
		))
		parent := c.StartRun(models.RunKindAgentWork, "Check the books")
		c.work(12)
		c.MustStatus(parent, models.AgentRunStatusCompleted)
		var failed *models.AgentRun
		for _, child := range c.children(parent.ID) {
			if child.Goal == "read the ledger" {
				failed = c.MustRun(child.ID)
			}
		}
		if failed == nil || failed.Status != models.AgentRunStatusFailed || failed.FailureExplanation == nil || failed.FailureExplanation.Message == "" {
			t.Fatalf("failed child = %#v", failed)
		}
		entry := c.toolEntry(parent.ID, string(models.ControlToolFork))
		if entry.State != models.ToolEntryError || !entry.Tool.Result.IsError {
			t.Fatalf("fork entry = %s error=%v", entry.State, entry.Tool.Result.IsError)
		}
		if text := childResultText(entry); !strings.Contains(text, failed.FailureExplanation.Message) && !strings.Contains(text, failed.Error) {
			t.Fatalf("fork result %q lacks the child's failure %q", text, failed.FailureExplanation.Message)
		}
		if got := c.MustRun(parent.ID).Output[conversationrun.FinalOutputSummaryKey]; got != "One check failed." {
			t.Fatalf("parent answer = %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Delegation: decision, execution and review Runs

func TestLoopDelegationRunsDecisionExecutionAndReviewInChildSessions(t *testing.T) {
	looptest.EachStore(t, "child_delegation", func(t *testing.T, store models.KernelStore) {
		c := newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "decision", When: func(v looptest.StepView) bool { return v.HasTool(string(models.ControlToolRequestDecision)) },
				Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.Act("", looptest.Call(string(models.ControlToolRequestDecision), map[string]any{"decision": "accept", "message": "I can check the release."}))
				}},
			&looptest.ModelRule{Name: "review", When: func(v looptest.StepView) bool { return v.HasTool(string(models.ControlToolCompletionReview)) },
				Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.Act("", looptest.Call(string(models.ControlToolCompletionReview), map[string]any{"decision": "approve", "message": "The check lists every open issue."}))
				}},
			&looptest.ModelRule{Name: "execution", When: func(v looptest.StepView) bool {
				for _, text := range v.UserTexts() {
					if strings.Contains(text, "Requested work. Goal:") {
						return true
					}
				}
				return false
			}, Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Release 7 has 2 open issues.") }},
			&looptest.ModelRule{Name: "delegate", Times: 1, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Asking scout.", looptest.Call(string(models.ControlToolDelegate), map[string]any{"agentId": childScout, "goal": "Check release 7"}))
			}},
			&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) models.ModelStepResult {
				content, _, _ := v.ToolResult(string(models.ControlToolDelegate))
				return looptest.Say("Scout says: " + content)
			}},
		))
		c.authority = func(run *models.AgentRun, a *agentrun.ControlAuthority) {
			if run.AssignedAgentID == looptest.FixtureAgent && a.EffectiveFinalMode() == agentrun.FinalModeDefault {
				a.DelegateTargets = []agentrun.HostedAgentTarget{{ID: childScout, DisplayName: "Scout"}}
			}
		}
		created, err := c.Portfolio.CreateAgentRun(c.Ctx, models.CreateAgentRunRequest{
			Scope: c.Scope, Kind: models.RunKindAgentWork, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: childTeam}, AssignedAgentID: looptest.FixtureAgent,
			Goal: "Get release 7 checked", Source: models.RunSourceObjective,
		})
		if err != nil {
			t.Fatal(err)
		}
		order := c.work(12)
		parent := c.MustRun(created.ID)
		c.MustStatus(parent, models.AgentRunStatusCompleted)

		requests, err := c.collab.ListAgentRequests(c.Ctx, models.AgentRequestFilter{Scope: c.Scope, SourceRunID: parent.ID, Limit: 10})
		if err != nil || len(requests) != 1 {
			t.Fatalf("requests = %d, %v", len(requests), err)
		}
		request := requests[0]
		if request.Status != models.AgentRequestStatusCompleted {
			t.Fatalf("request = %s", request.Status)
		}
		// Every claimed Run but the parent is one of the request's Runs.
		var decision, execution, review *models.AgentRun
		for _, id := range order {
			run := c.MustRun(id)
			switch run.Source {
			case models.RunSourceRequestDecision:
				decision = run
			case models.RunSourceCompletionReview:
				review = run
			case models.RunSourceRequest:
				execution = run
			}
		}
		if decision == nil || execution == nil || review == nil {
			t.Fatalf("claimed runs %v lack a decision/execution/review Run", order)
		}
		if decision.AssignedAgentID != childScout || execution.AssignedAgentID != childScout || review.AssignedAgentID != childReviewer ||
			execution.ID != request.ChildRunID {
			t.Fatalf("assignments: decision %s execution %s review %s (request child %s)",
				decision.AssignedAgentID, execution.AssignedAgentID, review.AssignedAgentID, request.ChildRunID)
		}
		for _, run := range []*models.AgentRun{decision, review} {
			if run.Budget == nil || run.Budget.MaxTurns != 12 {
				t.Fatalf("%s budget = %#v, want 12 turns", run.Source, run.Budget)
			}
			if run.Status != models.AgentRunStatusCompleted {
				t.Fatalf("%s = %s", run.Source, run.Status)
			}
		}
		// One session per Run. Cross-Agent Runs get fresh parentless
		// sessions (A3): another Agent never sees the requester's context.
		seen := map[string]string{}
		for _, run := range []*models.AgentRun{parent, decision, execution, review} {
			if run.SessionID == "" {
				t.Fatalf("%s %s has no session", run.Source, run.ID)
			}
			if other, dup := seen[run.SessionID]; dup {
				t.Fatalf("runs %s and %s share session %s", other, run.ID, run.SessionID)
			}
			seen[run.SessionID] = run.ID
			session := c.sessionByID(run.SessionID)
			if session.OwnerRunID != run.ID || session.ParentSessionID != "" || session.AgentID != run.AssignedAgentID {
				t.Fatalf("%s session = owner %s parent %q agent %s", run.Source, session.OwnerRunID, session.ParentSessionID, session.AgentID)
			}
			for _, e := range c.path(run.SessionID) {
				if e.RunID != run.ID {
					t.Fatalf("%s session holds entry %s of run %s", run.Source, e.ID, e.RunID)
				}
			}
		}
		// Decision and review Runs are offered only their typed final tool.
		for _, run := range []*models.AgentRun{decision, review} {
			for _, req := range c.requestsFor(run.ID) {
				for _, tool := range req.Tools {
					if tool.Name != string(models.ControlToolRequestDecision) && tool.Name != string(models.ControlToolCompletionReview) {
						t.Fatalf("%s offered %s", run.Source, tool.Name)
					}
				}
			}
		}
		// The review's verdict is on the request; the parent resumes only
		// after it, and its tool result is the execution's final text.
		if len(request.CompletionReviews) != 1 || !request.CompletionReviews[0].Approve ||
			request.CompletionReviews[0].Reviewer.ID != childReviewer || !strings.Contains(request.CompletionReviews[0].Summary, "every open issue") {
			t.Fatalf("completion reviews = %#v", request.CompletionReviews)
		}
		entry := c.toolEntry(parent.ID, string(models.ControlToolDelegate))
		value := childResultJSON(t, entry)
		if entry.State != models.ToolEntryCompleted || value["status"] != "completed" || value["answer"] != "Release 7 has 2 open issues." ||
			value["agentId"] != childScout || value["requestId"] != request.ID {
			t.Fatalf("delegate entry = %s %s", entry.State, childResultText(entry))
		}
		if steps := c.requestsFor(parent.ID); len(steps) != 2 {
			t.Fatalf("parent made %d model requests, want 2 (delegate, answer)", len(steps))
		}
		if got, _ := parent.Output[conversationrun.FinalOutputSummaryKey].(string); !strings.Contains(got, "Release 7 has 2 open issues.") {
			t.Fatalf("parent answer = %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Conversation tasks and promotion

func TestLoopConversationTaskStartsWithoutPark(t *testing.T) {
	looptest.EachStore(t, "child_task", func(t *testing.T, store models.KernelStore) {
		const ack = "On it, I will compile the report in the background."
		c := newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "task-work", When: func(v looptest.StepView) bool {
				for _, text := range v.UserTexts() {
					if strings.Contains(text, "Compile the quarterly report") && !strings.Contains(text, "compile the quarterly report please") {
						return true
					}
				}
				return false
			}, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Say("The quarterly report is ready: revenue grew 4%.")
			}},
			&looptest.ModelRule{Name: "start", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("", looptest.Call(string(models.ControlToolStartTask), map[string]any{"goal": "Compile the quarterly report", "acknowledgment": ack}))
			}},
			&looptest.ModelRule{Name: "follow-up", Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Say("Revenue grew 4%, as the task reported.")
			}},
		))
		c.authority = func(run *models.AgentRun, a *agentrun.ControlAuthority) {
			if run.Kind == models.RunKindConversation && run.Context[models.ConversationTaskContextKey] == nil {
				conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
				threadRootID, _ := run.Context["threadRootMessageId"].(string)
				a.TaskStart = &models.HostedConversationTaskContext{ConversationID: conversationID, ThreadRootID: threadRootID, CanStart: true}
			}
		}
		conversation, source := c.StartChat("compile the quarterly report please")

		// One claim: the tool entry completes and the Run ends with the ack.
		exit, ended := c.drive(source.ID)
		if ended.Status != models.AgentRunStatusCompleted {
			t.Fatalf("source = %s (exit %s)", ended.Status, exit.Reason)
		}
		start := c.toolEntry(source.ID, string(models.ControlToolStartTask))
		if start.State != models.ToolEntryCompleted || start.Tool.Result.IsError || !strings.Contains(strings.ToLower(childResultText(start)), "started") {
			t.Fatalf("start entry = %s %q", start.State, childResultText(start))
		}
		if len(c.requestsFor(source.ID)) != 1 {
			t.Fatalf("source made %d model requests", len(c.requestsFor(source.ID)))
		}
		messages := c.Messages(conversation.ID)
		if len(messages) != 2 || messages[1].Content != ack || messages[1].Sender.ID != looptest.FixtureAgent {
			t.Fatalf("channel after start = %s", looptest.DescribeMessages(messages))
		}

		// The task Run's session forks the chat session at the tool call.
		tasks, err := c.Chats.(models.ConversationTaskKernelStore).ListConversationTasks(c.Ctx, models.ConversationTaskFilter{
			Scope: c.Scope, Owner: source.Owner, ConversationID: conversation.ID, Limit: 10,
		})
		if err != nil || len(tasks) != 1 || tasks[0].WorkRun == nil {
			t.Fatalf("tasks = %#v, %v", tasks, err)
		}
		work := c.MustRun(tasks[0].WorkRun.ID)
		chat := c.MustRun(source.ID).SessionID
		session := c.sessionByID(work.SessionID)
		assistant := c.sessionEntries(chat)
		var forkingAssistant *models.SessionEntry
		for _, e := range assistant {
			if e.Type == models.SessionEntryAssistant && e.StepID == start.StepID {
				forkingAssistant = e
			}
		}
		if forkingAssistant == nil || session.ParentSessionID != chat || session.ForkSequence != forkingAssistant.Sequence-1 || session.OwnerRunID != work.ID {
			t.Fatalf("task session = parent %q fork %d owner %s (chat %s)", session.ParentSessionID, session.ForkSequence, session.OwnerRunID, chat)
		}

		// The task works on its own session and reports to the chat.
		c.work(6)
		c.MustStatus(work, models.AgentRunStatusCompleted)
		for _, e := range c.sessionEntries(work.SessionID) {
			if e.RunID != work.ID {
				t.Fatalf("task session holds entry %s of run %s", e.ID, e.RunID)
			}
		}
		messages = c.Messages(conversation.ID)
		var report *models.ChannelMessage
		for _, m := range messages {
			if strings.Contains(m.Content, "revenue grew 4%") {
				report = m
			}
		}
		if report == nil {
			t.Fatalf("no task report in %s", looptest.DescribeMessages(messages))
		}

		// The next chat Run catches the report up as a user entry from the
		// task's Agent.
		c.PostUser(conversation.ID, looptest.FixtureUser, "what did it find?")
		c.work(4)
		var nextRun *models.AgentRun
		for _, run := range c.listRuns() {
			if run.Kind == models.RunKindConversation && run.ID != source.ID && run.Context[models.ConversationTaskContextKey] == nil {
				nextRun = run
			}
		}
		if nextRun == nil {
			t.Fatal("the follow-up message started no Run")
		}
		c.MustStatus(nextRun, models.AgentRunStatusCompleted)
		if nextRun.SessionID != chat {
			t.Fatalf("follow-up session = %s, want the chat session %s", nextRun.SessionID, chat)
		}
		first := c.requestsFor(nextRun.ID)[0]
		// The chat session's user entry for the report is attributed to the
		// task's Agent; the model sees it in the message header (a channel
		// message renders its sender there, not as an entry author).
		var caught *models.SessionEntry
		for _, e := range c.sessionEntries(chat) {
			if e.Type == models.SessionEntryUser && e.User != nil && e.User.MessageID == report.ID {
				caught = e
			}
		}
		if caught == nil || caught.RunID != nextRun.ID || caught.User.Author.Type != string(models.ConversationParticipantAgent) ||
			caught.User.Author.ID != looptest.FixtureAgent {
			t.Fatalf("report catch-up entry = %#v", caught)
		}
		var rendered string
		for _, text := range (looptest.StepView{Req: first}).UserTexts() {
			if strings.Contains(text, "revenue grew 4%") {
				rendered = text
			}
		}
		if header, _, _ := strings.Cut(rendered, "\n"); !strings.Contains(header, looptest.FixtureAgent) || !strings.HasPrefix(header, "[Agent") {
			t.Fatalf("report renders as %q, want a header from the task's Agent", rendered)
		}
	})
}

func (c *childFixture) listRuns() []*models.AgentRun {
	c.T.Helper()
	runs, err := c.Raw.ListAgentRuns(c.Ctx, models.AgentRunFilter{Scope: c.Scope})
	if err != nil {
		c.T.Fatal(err)
	}
	return runs
}

func TestLoopForegroundPromotionForksTheSession(t *testing.T) {
	looptest.EachStore(t, "child_promotion", func(t *testing.T, store models.KernelStore) {
		var c *childFixture
		var promoted *models.ConversationTaskResult
		var slowRunID string
		c = newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "slow", Times: 1, When: func(v looptest.StepView) bool { return strings.Contains(v.LastUserText(), "audit") },
				Before: func(ctx context.Context, v looptest.StepView) error {
					// The step outlives the foreground timeout; the promoter
					// moves the Run off the lane.
					c.Clock.Advance(models.ConversationTaskForegroundTimeout)
					run := c.MustRun(slowRunID)
					result, err := c.Raw.(runlooptest.PromotingChatStore).PromoteConversationTask(ctx, models.ConversationTaskPromotionRequest{
						Scope: run.Scope, RunID: run.ID, Now: run.CreatedAt.Add(models.ConversationTaskForegroundTimeout),
					})
					if err != nil || result == nil {
						return fmt.Errorf("promote: %#v, %v", result, err)
					}
					promoted = result
					return nil
				},
				Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.Act("Auditing the ledger.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "ledger"}))
				}},
			&looptest.ModelRule{Name: "audit-done", When: func(v looptest.StepView) bool { return len(v.ToolResultsSinceUser()) > 0 },
				Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("The audit found no gaps.") }},
			&looptest.ModelRule{Name: "hello", When: func(v looptest.StepView) bool { return strings.Contains(v.LastUserText(), "hello") },
				Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Hi Ana.") }},
		))
		conversation, run := c.StartChat("please audit the ledger")
		slowRunID = run.ID
		lane := run.SessionID
		if lane == "" {
			lane = models.ConversationSessionID(c.Scope, run.ConcurrencyKey)
		}
		c.driveToRestChild(run.ID, 4)
		if promoted == nil || promoted.Task == nil {
			t.Fatal("the Run was not promoted")
		}
		done := c.MustRun(run.ID)
		c.MustStatus(done, models.AgentRunStatusCompleted)
		childSession := done.SessionID
		if childSession == "" || childSession == lane || done.ConcurrencyKey == run.ConcurrencyKey {
			t.Fatalf("promoted Run session %q lane %q key %q", childSession, lane, done.ConcurrencyKey)
		}
		child := c.sessionByID(childSession)
		if child.ParentSessionID != lane || child.OwnerRunID != run.ID {
			t.Fatalf("promotion session = parent %q owner %q", child.ParentSessionID, child.OwnerRunID)
		}
		// The promoted Run's work after promotion is in the child session.
		var childTypes []string
		for _, e := range c.sessionEntries(childSession) {
			if e.RunID != run.ID {
				t.Fatalf("child session holds entry %s of run %s", e.ID, e.RunID)
			}
			childTypes = append(childTypes, string(e.Type))
		}
		if !strings.Contains(strings.Join(childTypes, ","), "assistant,tool") {
			t.Fatalf("child session = %v, want the step's assistant and tool entries", childTypes)
		}
		// The promoted work still answers in the chat, once.
		answers := 0
		for _, m := range c.Messages(conversation.ID) {
			if strings.Contains(m.Content, "The audit found no gaps.") {
				answers++
			}
		}
		if answers != 1 {
			t.Fatalf("audit answers in the chat = %d: %s", answers, looptest.DescribeMessages(c.Messages(conversation.ID)))
		}
		chatBefore := c.sessionEntries(lane)
		for _, e := range chatBefore {
			if e.RunID == run.ID && (e.Type == models.SessionEntryAssistant || e.Type == models.SessionEntryTool) {
				t.Fatalf("chat session holds the promoted Run's %s entry %s", e.Type, e.ID)
			}
		}

		// A new message starts a new Run on the chat session.
		c.PostUser(conversation.ID, looptest.FixtureUser, "hello again")
		c.work(4)
		var next *models.AgentRun
		for _, r := range c.listRuns() {
			if r.Kind == models.RunKindConversation && r.ID != run.ID {
				next = r
			}
		}
		if next == nil {
			t.Fatal("the new message started no Run")
		}
		c.MustStatus(next, models.AgentRunStatusCompleted)
		if next.SessionID != lane {
			t.Fatalf("new Run session = %s, want the chat session %s", next.SessionID, lane)
		}
		for _, e := range c.sessionEntries(lane) {
			if e.Sequence <= chatBefore[len(chatBefore)-1].Sequence {
				continue
			}
			if e.RunID != next.ID {
				t.Fatalf("chat session gained entry %s of run %s after promotion", e.ID, e.RunID)
			}
		}
		for _, e := range c.sessionEntries(childSession) {
			if e.RunID != run.ID {
				t.Fatalf("child session gained entry %s of run %s", e.ID, e.RunID)
			}
		}
	})
}

// driveToRestChild is driveToRest through the child worker's binding.
func (c *childFixture) driveToRestChild(runID string, maxClaims int) *models.AgentRun {
	c.T.Helper()
	for i := 0; i < maxClaims; i++ {
		_, run := c.drive(runID)
		if run.Status != models.AgentRunStatusQueued {
			return run
		}
		if run.AvailableAt.After(c.Clock.Now()) {
			c.Clock.Advance(run.AvailableAt.Sub(c.Clock.Now()) + time.Millisecond)
		}
	}
	c.T.Fatalf("run %s not at rest after %d claims", runID, maxClaims)
	return nil
}

// ---------------------------------------------------------------------------
// Runbook operations

var childEngageOperation = agentrun.HostedRunbookOperation{
	DefinitionID: "def-engage", DefinitionVersion: "1", Entrypoint: "engage-now",
	Name: "Engage accounts", Description: "Send the reviewed engagement sequence.",
	InputSchema: map[string]interface{}{
		"type": "object", "additionalProperties": false, "required": []interface{}{"account"},
		"properties": map[string]interface{}{"account": map[string]interface{}{"type": "string"}},
	},
}

func childEngageRunbook() *runbook.Definition {
	return &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: childEngageOperation.DefinitionID, Version: childEngageOperation.DefinitionVersion, Name: "Engage",
		Entrypoints: map[string]string{childEngageOperation.Entrypoint: "done"},
		Steps: map[string]runbook.Step{
			"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"summary": runbookLiteral("Engaged acme.")}}},
		},
	}
}

func newRunbookChildFixture(t *testing.T, store models.KernelStore) *childFixture {
	t.Helper()
	c := newChildFixture(t, store, looptest.WithRules(
		&looptest.ModelRule{Name: "operation", Times: 1, Reply: func(looptest.StepView) models.ModelStepResult {
			return looptest.Act("Starting the engagement.", looptest.Call(string(models.ControlToolRunOperation), map[string]any{
				"entrypoint": childEngageOperation.Entrypoint, "arguments": map[string]any{"account": "acme"},
			}))
		}},
		&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) models.ModelStepResult {
			content, isError, _ := v.ToolResult(string(models.ControlToolRunOperation))
			if isError {
				return looptest.Say("The engagement failed.")
			}
			return looptest.Say("Done: " + content)
		}},
	))
	c.runbooks[childEngageOperation.DefinitionID] = childEngageRunbook()
	c.authority = func(run *models.AgentRun, a *agentrun.ControlAuthority) {
		if run.Entrypoint == "" {
			a.Operations = []agentrun.HostedRunbookOperation{childEngageOperation}
		}
	}
	return c
}

func TestLoopRunbookStartParksForChild(t *testing.T) {
	looptest.EachStore(t, "child_runbook_start", func(t *testing.T, store models.KernelStore) {
		c := newRunbookChildFixture(t, store)
		parent := c.StartRun(models.RunKindAgentWork, "Engage acme")
		if exit, _ := c.drive(parent.ID); exit.Reason != runloop.ExitParked {
			t.Fatalf("parent exit = %s", exit.Reason)
		}
		c.MustStatus(parent, models.AgentRunStatusWaitingForDependency)
		entry := c.toolEntry(parent.ID, string(models.ControlToolRunOperation))
		if entry.State != models.ToolEntryAwaitingChild {
			t.Fatalf("operation entry = %s %q", entry.State, childResultText(entry))
		}
		children := c.children(parent.ID)
		if len(children) != 1 || children[0].Entrypoint != childEngageOperation.Entrypoint {
			t.Fatalf("children = %#v", children)
		}
		child := children[0]
		order := c.work(6)
		if len(order) != 2 || order[0] != child.ID || order[1] != parent.ID {
			t.Fatalf("claims = %v, want the runbook child then the parent", order)
		}
		c.MustStatus(child, models.AgentRunStatusCompleted)
		c.MustStatus(parent, models.AgentRunStatusCompleted)
		entry = c.toolEntry(parent.ID, string(models.ControlToolRunOperation))
		value := childResultJSON(t, entry)
		if entry.State != models.ToolEntryCompleted || value["status"] != string(models.AgentRunStatusCompleted) || value["runId"] != child.ID ||
			value["entrypoint"] != childEngageOperation.Entrypoint {
			t.Fatalf("operation entry = %s %s", entry.State, childResultText(entry))
		}
		if got, _ := c.MustRun(parent.ID).Output[conversationrun.FinalOutputSummaryKey].(string); !strings.HasPrefix(got, "Done: ") {
			t.Fatalf("parent answer = %q", got)
		}
	})
}

// TestDeterministicRunbookHasNoRenderedSession is §5.8's
// TestDeterministicRunbookHasNoSession under A3: a deterministic Run gets a
// run session that is never rendered. No model step runs for it, its session
// holds no model entries and is not forked from anything, and its engine
// state stays in the Run.
func TestDeterministicRunbookHasNoRenderedSession(t *testing.T) {
	looptest.EachStore(t, "child_runbook_session", func(t *testing.T, store models.KernelStore) {
		c := newRunbookChildFixture(t, store)
		parent := c.StartRun(models.RunKindAgentWork, "Engage acme")
		c.work(6)
		c.MustStatus(parent, models.AgentRunStatusCompleted)
		children := c.children(parent.ID)
		if len(children) != 1 {
			t.Fatalf("children = %d", len(children))
		}
		child := c.MustRun(children[0].ID)
		c.MustStatus(child, models.AgentRunStatusCompleted)
		if n := len(c.requestsFor(child.ID)); n != 0 {
			t.Fatalf("the runbook Run made %d model requests", n)
		}
		if child.SessionID == "" {
			t.Fatal("the runbook Run has no run session")
		}
		session := c.sessionByID(child.SessionID)
		if session.ParentSessionID != "" || session.OwnerRunID != child.ID {
			t.Fatalf("runbook session = parent %q owner %q", session.ParentSessionID, session.OwnerRunID)
		}
		for _, e := range c.sessionEntries(child.SessionID) {
			if e.Type == models.SessionEntryAssistant || e.Type == models.SessionEntryUser {
				t.Fatalf("runbook session holds a %s entry", e.Type)
			}
		}
		if _, err := routine.RunbookTraceFromCheckpoint(child.Checkpoint); err != nil || len(child.Checkpoint) == 0 {
			t.Fatalf("runbook engine state is not in the Run: %v", err)
		}
	})
}
