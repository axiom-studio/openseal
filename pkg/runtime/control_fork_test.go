package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/google/uuid"
)

const forkTestWorker = "fork-worker"

// forkTestStore is a kernel store that also holds dependency groups.
type forkTestStore interface {
	KernelStore
	RunDependencyStore
}

// forkCall is one claimed Run whose finished, applied step called
// control_fork (entry pending), ready to settle.
type forkCall struct {
	t         *testing.T
	ctx       context.Context
	store     forkTestStore
	sessions  AgentSessionStore
	scope     Scope
	run       *AgentRun
	session   *AgentSession
	user      *SessionEntry
	assistant *SessionEntry
	step      *AgentTurn
	entry     *SessionEntry
	exec      *ControlExecutor
	authority agentrun.ControlAuthority
}

func forkStores(t *testing.T, fn func(t *testing.T, store forkTestStore)) {
	t.Helper()
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		forks, ok := store.(forkTestStore)
		if !ok {
			t.Fatalf("%T has no dependency store", store)
		}
		fn(t, forks)
	})
}

// newForkCall creates and claims a Run with budget (nil = unbounded), gives
// it a run session holding one user entry, and records a control_fork call
// with arguments.
func newForkCall(t *testing.T, store forkTestStore, budget *BudgetPolicy, arguments string) *forkCall {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "fork-" + uuid.NewString()[:8]}
	created, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-a"}, AssignedAgentID: "agent-a",
		Goal: "compare two vendors", Source: RunSourceObjective, Budget: budget,
		Context: map[string]interface{}{"projectId": "p1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: forkTestWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != created.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	sessions := AgentSessionStore(store)
	lease := AgentRunLeaseGuard{WorkerID: forkTestWorker, Now: now}
	opened, err := sessions.CreateRunAgentSession(ctx, CreateRunAgentSessionRequest{
		Scope: scope, OwnerRunID: claimed.ID, AgentID: claimed.AssignedAgentID, Lease: lease, Now: now,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	session := opened.Session
	user := &SessionEntry{
		ID: SessionEntryID(session.ID, "user-1"), RunID: claimed.ID, Type: SessionEntryUser,
		User: &UserEntry{Origin: UserEntryChannelMessage, Author: SessionAuthor{Type: "user", ID: "u1"},
			Content: []ContentPart{{Type: ContentPartText, Text: "compare vendor A and vendor B"}}, MessageID: "m1"},
	}
	stepID := claimed.ID + "-step-1"
	callID := "call-fork"
	raw := json.RawMessage(arguments)
	assistant := &SessionEntry{
		ID: SessionEntryID(session.ID, AssistantEntryKey(stepID)), RunID: claimed.ID, StepID: stepID, Step: 1,
		Type: SessionEntryAssistant, Assistant: &AssistantEntry{
			Provider: ModelStepProviderAnthropic, Model: "claude-x", StopReason: ModelStopToolCalls, Text: "Splitting the work.",
			ToolCalls: []ModelToolCall{{ID: callID, Name: string(ControlToolFork), Arguments: raw}},
		},
	}
	tool := &SessionEntry{
		ID: SessionEntryID(session.ID, ToolEntryKey(stepID, callID)), RunID: claimed.ID, StepID: stepID, Step: 1,
		Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: callID,
		Tool: &ToolEntry{Name: string(ControlToolFork), Arguments: raw, SideEffect: ToolSideEffectWrite},
	}
	if _, err := sessions.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: scope, SessionID: session.ID, RunID: claimed.ID, Lease: &lease, Entries: []*SessionEntry{user}, Now: now,
	}); err != nil {
		t.Fatalf("append user entry: %v", err)
	}
	appended, err := sessions.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: scope, SessionID: session.ID, RunID: claimed.ID, Lease: &lease, Entries: []*SessionEntry{assistant, tool}, Now: now,
	})
	if err != nil {
		t.Fatalf("append fork step: %v", err)
	}
	run, err := store.GetAgentRun(ctx, scope, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	run.LastAppliedTurn = 1
	session, err = sessions.GetAgentSession(ctx, scope, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	userStored, err := sessions.GetSessionEntry(ctx, scope, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &forkCall{
		t: t, ctx: ctx, store: store, sessions: sessions, scope: scope, run: run, session: session,
		user: userStored, assistant: appended.Entries[0], entry: appended.Entries[1],
		step: &AgentTurn{ID: stepID, RunID: run.ID, Sequence: 1, Status: AgentTurnStatusCompleted},
		exec: &ControlExecutor{Portfolio: store, Sessions: sessions, Forks: agentrun.NewRunForkCoordinator(store)},
		authority: agentrun.ControlAuthority{ForkAllowed: true, ForkTargets: []agentrun.HostedAgentTarget{
			{ID: "agent-b", DisplayName: "Researcher"},
		}},
	}
}

func (c *forkCall) context() agentrun.ControlContext {
	return NewControlContext(agentrun.StepToolsInput{
		Run: c.run, Step: c.step, Session: c.session, WorkerID: forkTestWorker,
		Binding: &agentrun.TurnRunnerBinding{Authority: c.authority}, Assistant: c.assistant,
	}, c.entry)
}

func (c *forkCall) settle() ControlResult {
	c.t.Helper()
	result, err := c.exec.Settle(c.ctx, c.context())
	if err != nil {
		c.t.Fatalf("settle control_fork: %v", err)
	}
	if result.Entry != nil {
		c.entry = result.Entry
	}
	return result
}

func (c *forkCall) getRun(id string) *AgentRun {
	c.t.Helper()
	run, err := c.store.GetAgentRun(c.ctx, c.scope, id)
	if err != nil {
		c.t.Fatalf("get run %s: %v", id, err)
	}
	return run
}

func (c *forkCall) storedEntry() *SessionEntry {
	c.t.Helper()
	entry, err := c.sessions.GetSessionEntry(c.ctx, c.scope, c.entry.ID)
	if err != nil {
		c.t.Fatal(err)
	}
	return entry
}

func (c *forkCall) meta() *ControlEntryMeta {
	c.t.Helper()
	meta, err := DecodeControlEntryMeta(c.storedEntry())
	if err != nil || meta == nil {
		c.t.Fatalf("control meta = %#v, %v", meta, err)
	}
	return meta
}

// finishChild runs child id to status with an answer, then resolves its edge
// as the worker that finished it would.
func (c *forkCall) finishChild(id string, status AgentRunStatus, answer, failure string) {
	c.t.Helper()
	const worker = "child-worker"
	activity := NewRunActivityService(c.store, c.store)
	child := c.getRun(id)
	for i := 0; child.Status == AgentRunStatusQueued; i++ {
		if i > 10 {
			c.t.Fatalf("child %s was never claimed", id)
		}
		if _, err := c.store.ClaimNextAgentRun(c.ctx, AgentRunClaim{
			Scope: c.scope, WorkerID: worker, Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
		}); err != nil {
			c.t.Fatalf("claim child: %v", err)
		}
		child = c.getRun(id)
	}
	req := RunTransitionRequest{
		ExpectedRevision: child.Revision, Status: status, LeaseOwner: worker,
		Actor: ActivityActor{Type: "worker", ID: worker}, Error: failure,
	}
	if answer != "" {
		req.Output = map[string]interface{}{"summary": answer}
	}
	done, _, err := activity.TransitionRun(c.ctx, c.scope, id, req)
	if err != nil {
		c.t.Fatalf("finish child %s: %v", id, err)
	}
	if _, err := c.exec.Forks.CompleteChild(c.ctx, done, ActivityActor{Type: "worker", ID: worker}); err != nil {
		c.t.Fatalf("complete child %s: %v", id, err)
	}
}

// reclaim claims the queued source again (children may be claimed on the
// way) and reloads its tool entry.
func (c *forkCall) reclaim() {
	c.t.Helper()
	for i := 0; i < 10; i++ {
		claimed, err := c.store.ClaimNextAgentRun(c.ctx, AgentRunClaim{
			Scope: c.scope, WorkerID: forkTestWorker, Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
		})
		if err != nil || claimed == nil {
			c.t.Fatalf("reclaim = %#v, %v", claimed, err)
		}
		if claimed.ID == c.run.ID {
			c.run = claimed
			c.entry = c.storedEntry()
			return
		}
	}
	c.t.Fatal("source was never reclaimed")
}

func (c *forkCall) resume() ControlResume {
	c.t.Helper()
	resumed, err := c.exec.Resume(c.ctx, c.context())
	if err != nil {
		c.t.Fatalf("resume control_fork: %v", err)
	}
	if resumed.Resolved && resumed.Result.Entry != nil {
		c.entry = resumed.Result.Entry
	}
	return resumed
}

// forkResult decodes the entry's JSON result.
func forkResult(t *testing.T, entry *SessionEntry) (map[string]interface{}, string) {
	t.Helper()
	if entry.Tool == nil || entry.Tool.Result == nil || len(entry.Tool.Result.Content) != 1 {
		t.Fatalf("entry result = %#v", entry.Tool)
	}
	raw := string(entry.Tool.Result.Content[0].Value)
	var value map[string]interface{}
	if err := json.Unmarshal(entry.Tool.Result.Content[0].Value, &value); err != nil {
		t.Fatalf("decode result %s: %v", raw, err)
	}
	return value, raw
}

const twoBranchFork = `{"branches":[{"goal":"Research vendor A","context":{"vendor":"A"}},{"goal":"Research vendor B"}]}`

func TestForkParksSourceAndCreatesAgentWorkChildren(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, nil, twoBranchFork)
		checkpointBefore := maps.Clone(c.run.Checkpoint)
		result := c.settle()
		if result.Kind != ControlParked || result.Park == nil || !result.Park.Committed ||
			result.Park.Status != AgentRunStatusWaitingForDependency || result.Park.Wake == nil || result.Park.Wake.Type != "run_dependencies" {
			t.Fatalf("result = %#v park %#v", result, result.Park)
		}
		meta := c.meta()
		if meta.Kind != ControlToolFork || meta.ForkID != "fork-"+c.entry.ID || len(meta.ChildRunIDs) != 2 ||
			meta.GroupID != agentrun.StableForkIdentifier(c.run.ID, meta.ForkID, "group") || result.Park.Wake.Reference != meta.GroupID {
			t.Fatalf("meta = %#v", meta)
		}
		stored := c.storedEntry()
		if stored.State != ToolEntryAwaitingChild || stored.Tool.Wait == nil || stored.Tool.Wait.GroupID != meta.GroupID {
			t.Fatalf("entry = %s %#v", stored.State, stored.Tool.Wait)
		}
		source := c.getRun(c.run.ID)
		if source.Status != AgentRunStatusWaitingForDependency || source.LeaseOwner != "" ||
			source.WakeCondition == nil || source.WakeCondition.Reference != meta.GroupID {
			t.Fatalf("source = %s lease %q wake %#v", source.Status, source.LeaseOwner, source.WakeCondition)
		}
		if fmt.Sprint(source.Checkpoint) != fmt.Sprint(checkpointBefore) {
			t.Fatalf("source checkpoint changed: %v -> %v", checkpointBefore, source.Checkpoint)
		}
		for i, id := range meta.ChildRunIDs {
			child := c.getRun(id)
			if child.Kind != RunKindAgentWork || child.Source != RunSourceFork || child.ParentRunID != c.run.ID ||
				child.Status != AgentRunStatusQueued || child.AssignedAgentID != "agent-a" || child.Budget != nil {
				t.Fatalf("child %d = kind %s source %s parent %s status %s agent %s", i, child.Kind, child.Source, child.ParentRunID, child.Status, child.AssignedAgentID)
			}
			if child.Context["projectId"] != "p1" {
				t.Fatalf("child %d lost the source lineage: %v", i, child.Context)
			}
		}
		if goal := c.getRun(meta.ChildRunIDs[0]).Goal; goal != "Research vendor A" {
			t.Fatalf("child goal = %q", goal)
		}
		if c.getRun(meta.ChildRunIDs[0]).Context["vendor"] != "A" {
			t.Fatalf("branch context lost: %v", c.getRun(meta.ChildRunIDs[0]).Context)
		}
	})
}

func TestForkResumeReturnsEachBranchFinalAnswer(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, nil, twoBranchFork)
		c.settle()
		meta := c.meta()
		c.finishChild(meta.ChildRunIDs[0], AgentRunStatusCompleted, "Vendor A costs 10.", "")
		if source := c.getRun(c.run.ID); source.Status != AgentRunStatusWaitingForDependency {
			t.Fatalf("source woke after one of two branches: %s", source.Status)
		}
		c.finishChild(meta.ChildRunIDs[1], AgentRunStatusCompleted, "Vendor B costs 12.", "")
		if source := c.getRun(c.run.ID); source.Status != AgentRunStatusQueued {
			t.Fatalf("source after both branches = %s", source.Status)
		}
		c.reclaim()
		resumed := c.resume()
		if !resumed.Resolved || resumed.Result.Kind != ControlContinue {
			t.Fatalf("resume = %#v", resumed)
		}
		if c.entry.State != ToolEntryCompleted || c.entry.Tool.Result.IsError {
			t.Fatalf("entry = %s %#v", c.entry.State, c.entry.Tool.Result)
		}
		value, raw := forkResult(t, c.entry)
		branches, _ := value["branches"].([]interface{})
		if value["status"] != "satisfied" || value["mode"] != "all" || len(branches) != 2 {
			t.Fatalf("result = %s", raw)
		}
		for i, want := range []string{"Vendor A costs 10.", "Vendor B costs 12."} {
			branch := branches[i].(map[string]interface{})
			if branch["answer"] != want || branch["status"] != "completed" || branch["branch"] != fmt.Sprintf("branch-%d", i+1) ||
				branch["runId"] != meta.ChildRunIDs[i] {
				t.Fatalf("branch %d = %v", i, branch)
			}
		}
	})
}

func TestForkAnyCancelsLosersAndReportsWinner(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, nil, `{"mode":"any","branches":[{"goal":"Try mirror one"},{"goal":"Try mirror two"},{"goal":"Try mirror three"}]}`)
		c.settle()
		meta := c.meta()
		c.finishChild(meta.ChildRunIDs[1], AgentRunStatusCompleted, "Mirror two works.", "")
		for _, id := range []string{meta.ChildRunIDs[0], meta.ChildRunIDs[2]} {
			if loser := c.getRun(id); loser.Status != AgentRunStatusCanceled {
				t.Fatalf("loser %s = %s", id, loser.Status)
			}
		}
		c.reclaim()
		c.resume()
		value, raw := forkResult(t, c.entry)
		if c.entry.State != ToolEntryCompleted || value["status"] != "satisfied" || value["mode"] != "any" {
			t.Fatalf("entry %s result %s", c.entry.State, raw)
		}
		branches := value["branches"].([]interface{})
		statuses := []string{}
		for _, branch := range branches {
			statuses = append(statuses, branch.(map[string]interface{})["status"].(string))
		}
		if strings.Join(statuses, ",") != "canceled,completed,canceled" || branches[1].(map[string]interface{})["answer"] != "Mirror two works." {
			t.Fatalf("result = %s", raw)
		}
	})
}

func TestForkFailedBranchIsErrorResultNotRunFailure(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, nil, twoBranchFork)
		c.settle()
		meta := c.meta()
		c.finishChild(meta.ChildRunIDs[0], AgentRunStatusFailed, "", "vendor A site is down")
		source := c.getRun(c.run.ID)
		if source.Status != AgentRunStatusQueued {
			t.Fatalf("source after a failed branch = %s", source.Status)
		}
		if source.FailureExplanation != nil {
			t.Fatalf("failed group recorded a final failure: %#v", source.FailureExplanation)
		}
		c.reclaim()
		resumed := c.resume()
		if !resumed.Resolved || resumed.Result.Kind != ControlContinue {
			t.Fatalf("resume = %#v", resumed)
		}
		if c.entry.State != ToolEntryError || !c.entry.Tool.Result.IsError || c.entry.Tool.Result.Status != ToolResultFailed {
			t.Fatalf("entry = %s %#v", c.entry.State, c.entry.Tool.Result)
		}
		value, raw := forkResult(t, c.entry)
		first := value["branches"].([]interface{})[0].(map[string]interface{})
		if value["status"] != "failed" || first["status"] != "failed" || first["error"] != "vendor A site is down" {
			t.Fatalf("result = %s", raw)
		}
		if run := c.getRun(c.run.ID); run.Status != AgentRunStatusRunning {
			t.Fatalf("source after the failed result = %s", run.Status)
		}
	})
}

// crashingForkStore fails the first dependency-group create the way a
// killed worker does: the transaction never commits.
type crashingForkStore struct {
	forkTestStore
	crashes int
}

func (s *crashingForkStore) CreateRunDependencyGroup(ctx context.Context, record RunDependencyGroupCreateRecord) (*RunDependencyResult, error) {
	if s.crashes > 0 {
		s.crashes--
		return nil, context.Canceled
	}
	return s.forkTestStore.CreateRunDependencyGroup(ctx, record)
}

func TestForkCrashBeforeCreateResettlesOnce(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, nil, twoBranchFork)
		crashing := &crashingForkStore{forkTestStore: store, crashes: 1}
		c.exec.Forks = agentrun.NewRunForkCoordinator(crashing)
		if _, err := c.exec.Settle(c.ctx, c.context()); !errors.Is(err, context.Canceled) {
			t.Fatalf("crashed settle error = %v", err)
		}
		if stored := c.storedEntry(); stored.State != ToolEntryPending || len(stored.Tool.Control) != 0 {
			t.Fatalf("entry after the crash = %s control %s", stored.State, stored.Tool.Control)
		}
		children, err := store.ListAgentRuns(c.ctx, AgentRunFilter{Scope: c.scope, ParentRunID: c.run.ID, Limit: 10})
		if err != nil || len(children) != 0 {
			t.Fatalf("children after the crash = %d, %v", len(children), err)
		}
		if run := c.getRun(c.run.ID); run.Status != AgentRunStatusRunning || run.LeaseOwner != forkTestWorker {
			t.Fatalf("source after the crash = %s %q", run.Status, run.LeaseOwner)
		}
		// The next claim's resume settles the still-pending entry again.
		c.run = c.getRun(c.run.ID)
		c.run.LastAppliedTurn = 1
		result := c.settle()
		if result.Kind != ControlParked || !result.Park.Committed {
			t.Fatalf("re-settle = %#v", result)
		}
		children, err = store.ListAgentRuns(c.ctx, AgentRunFilter{Scope: c.scope, ParentRunID: c.run.ID, Limit: 10})
		if err != nil || len(children) != 2 {
			t.Fatalf("children after the re-settle = %d, %v", len(children), err)
		}
		meta := c.meta()
		group, err := store.GetRunDependencyGroup(c.ctx, c.scope, meta.GroupID)
		if err != nil || group == nil || group.ExpectedCount != 2 {
			t.Fatalf("group = %#v, %v", group, err)
		}
		// A parked entry is not settled again.
		if _, err := c.exec.Settle(c.ctx, c.context()); !errors.Is(err, ErrLeaseLost) && !errors.Is(err, ErrControlEntryState) {
			t.Fatalf("settle of a parked fork = %v", err)
		}
	})
}

func TestSameAgentBranchInheritsParentPathOtherAgentDoesNot(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, nil, `{"branches":[{"goal":"Research vendor A"},{"goal":"Research vendor B","agentId":"agent-b"}]}`)
		c.settle()
		meta := c.meta()
		self, other := c.getRun(meta.ChildRunIDs[0]), c.getRun(meta.ChildRunIDs[1])
		if self.SessionID != RunAgentSessionID(c.scope, self.ID) {
			t.Fatalf("same-Agent child session = %q", self.SessionID)
		}
		session, err := c.sessions.GetAgentSession(c.ctx, c.scope, self.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if session.ParentSessionID != c.session.ID || session.ForkSequence != c.assistant.Sequence-1 || session.ForkEntryID != c.user.ID {
			t.Fatalf("child session = parent %q fork %d entry %q", session.ParentSessionID, session.ForkSequence, session.ForkEntryID)
		}
		path, err := c.sessions.LoadAgentSessionPath(c.ctx, c.scope, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(path.Entries) != 2 || path.Entries[0].ID != c.user.ID || path.Entries[1].Type != SessionEntryUser ||
			path.Entries[1].User.Origin != UserEntryGoal || path.Entries[1].User.GoalKind != "fork_branch" ||
			!strings.Contains(path.Entries[1].User.Content[0].Text, "Research vendor A") {
			t.Fatalf("child path = %#v", path.Entries)
		}
		for _, entry := range path.Entries {
			if entry.Type == SessionEntryAssistant || entry.Type == SessionEntryTool {
				t.Fatalf("child path holds the fork decision: %#v", entry)
			}
		}
		if other.AssignedAgentID != "agent-b" || other.SessionID != "" {
			t.Fatalf("other-Agent child = agent %q session %q", other.AssignedAgentID, other.SessionID)
		}
		if _, err := c.sessions.GetAgentSession(c.ctx, c.scope, RunAgentSessionID(c.scope, other.ID)); !errors.Is(err, ErrAgentSessionNotFound) {
			t.Fatalf("other-Agent child has a session before its claim: %v", err)
		}
	})
}

func TestForkOtherAgentNotOfferedIsToolError(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, nil, `{"branches":[{"goal":"A"},{"goal":"B","agentId":"agent-z"}]}`)
		result := c.settle()
		value, raw := forkResult(t, c.entry)
		if result.Kind != ControlContinue || c.entry.State != ToolEntryError || value["error"] != "agent_not_eligible" {
			t.Fatalf("result %#v entry %s %s", result.Kind, c.entry.State, raw)
		}
	})
}

func TestForkInsufficientBudgetIsToolError(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, &BudgetPolicy{MaxTurns: 10}, twoBranchFork)
		result := c.settle()
		if result.Kind != ControlContinue || c.entry.State != ToolEntryError {
			t.Fatalf("result = %#v entry %s", result, c.entry.State)
		}
		value, raw := forkResult(t, c.entry)
		if value["error"] != modelstep.ControlBudgetErrorCode || !strings.Contains(raw, "cannot fund 2 more child Runs") {
			t.Fatalf("result = %s", raw)
		}
		children, err := store.ListAgentRuns(c.ctx, AgentRunFilter{Scope: c.scope, ParentRunID: c.run.ID, Limit: 10})
		if err != nil || len(children) != 0 {
			t.Fatalf("children = %d, %v", len(children), err)
		}
		if run := c.getRun(c.run.ID); run.Status != AgentRunStatusRunning {
			t.Fatalf("source = %s", run.Status)
		}
	})
}

func TestForkBoundedRunFundsMinimumChildren(t *testing.T) {
	const childMinimumTurns = 8 // the Turns every child is funded with at least
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, &BudgetPolicy{MaxTurns: 40}, twoBranchFork)
		if result := c.settle(); result.Kind != ControlParked {
			t.Fatalf("result = %#v", result)
		}
		for _, id := range c.meta().ChildRunIDs {
			if child := c.getRun(id); child.Budget == nil || child.Budget.MaxTurns != childMinimumTurns {
				t.Fatalf("child budget = %#v", child.Budget)
			}
		}
	})
}

func TestForkRefusedWhenMessageArrived(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, nil, twoBranchFork)
		// A message arrives after the step was planned; the claim keeps its lease.
		activity := NewRunActivityService(store, store)
		if _, _, err := activity.TransitionRun(c.ctx, c.scope, c.run.ID, RunTransitionRequest{
			ExpectedRevision: c.getRun(c.run.ID).Revision, Status: AgentRunStatusRunning, LeaseOwner: forkTestWorker,
			Actor:        ActivityActor{Type: "user", ID: "u1"},
			Intervention: &AgentRunIntervention{ID: "steer-1", Actor: ActivityActor{Type: "user", ID: "u1"}, Instruction: "only vendor A", CreatedAt: time.Now().UTC()},
		}); err != nil {
			t.Fatalf("steer: %v", err)
		}
		result := c.settle()
		value, raw := forkResult(t, c.entry)
		if result.Kind != ControlContinue || c.entry.State != ToolEntryError || value["error"] != forkNewMessageCode {
			t.Fatalf("result %s entry %s %s", result.Kind, c.entry.State, raw)
		}
		children, err := store.ListAgentRuns(c.ctx, AgentRunFilter{Scope: c.scope, ParentRunID: c.run.ID, Limit: 10})
		if err != nil || len(children) != 0 {
			t.Fatalf("children = %d, %v", len(children), err)
		}
	})
}

func TestForkResultNeverContainsChildCheckpoint(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, nil, twoBranchFork)
		c.settle()
		meta := c.meta()
		for _, id := range meta.ChildRunIDs {
			c.finishChild(id, AgentRunStatusCompleted, "done "+id[:4], "")
		}
		source := c.getRun(c.run.ID)
		groups, _ := source.Output["dependencyGroups"].(map[string]interface{})
		if !strings.Contains(fmt.Sprint(groups), "forkChild") {
			t.Fatalf("the runbook join lost the child checkpoint: %v", groups)
		}
		c.reclaim()
		c.resume()
		_, raw := forkResult(t, c.entry)
		for _, leaked := range []string{"checkpoint", "forkChild", "dependencyId", "groupId"} {
			if strings.Contains(raw, leaked) {
				t.Fatalf("result leaks %q: %s", leaked, raw)
			}
		}
	})
}

func TestForkResumeWhileGroupWaitsReparks(t *testing.T) {
	forkStores(t, func(t *testing.T, store forkTestStore) {
		c := newForkCall(t, store, nil, twoBranchFork)
		c.settle()
		meta := c.meta()
		// Steering queues the waiting source before its branches finish.
		activity := NewRunActivityService(store, store)
		source := c.getRun(c.run.ID)
		if _, _, err := activity.TransitionRun(c.ctx, c.scope, c.run.ID, RunTransitionRequest{
			ExpectedRevision: source.Revision, Status: AgentRunStatusQueued, Actor: ActivityActor{Type: "user", ID: "u1"},
		}); err != nil {
			t.Fatalf("requeue: %v", err)
		}
		c.reclaim()
		resumed := c.resume()
		if resumed.Resolved || resumed.Park == nil || resumed.Park.Committed || resumed.Park.Status != AgentRunStatusWaitingForDependency ||
			resumed.Park.Wake.Reference != meta.GroupID {
			t.Fatalf("resume = %#v", resumed)
		}
		if stored := c.storedEntry(); stored.State != ToolEntryAwaitingChild {
			t.Fatalf("entry = %s", stored.State)
		}
	})
}

func TestForkSpecSameWithAndWithoutTargets(t *testing.T) {
	self := forkHandler{}.Spec(agentrun.ControlAuthority{ForkAllowed: true})
	targets := forkHandler{}.Spec(agentrun.ControlAuthority{ForkAllowed: true, ForkTargets: []agentrun.HostedAgentTarget{{ID: "agent-b"}}})
	if !reflect.DeepEqual(self, targets) {
		t.Fatal("fork targets changed the fork tool")
	}
	items := targets.InputSchema["properties"].(map[string]interface{})["branches"].(map[string]interface{})["items"].(map[string]interface{})
	agent, ok := items["properties"].(map[string]interface{})["agentId"].(map[string]interface{})
	if _, enum := agent["enum"]; !ok || enum {
		t.Fatalf("agentId = %#v", agent)
	}
}
