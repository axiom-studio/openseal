package runtime

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func taskProposalFixture() *TurnTaskProposal {
	return &TurnTaskProposal{TaskKey: "release-review", Goal: "Review the release and report the outcome", Acknowledgment: "I’ve started the release review."}
}

func TestHostedTaskWorkRestrictsTargetsAndRejectsDelegation(t *testing.T) {
	for _, taskWork := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary work", true: "task work"}[taskWork], func(t *testing.T) {
			run := &AgentRun{ID: "run", Kind: RunKindAgentWork, AssignedAgentID: "agent", Scope: Scope{Kind: "tenant", ID: "one"}, Context: map[string]interface{}{}}
			if taskWork {
				run.Context[ConversationTaskContextKey] = "task-hint"
			}
			targets := []HostedAgentTarget{{ID: "agent", DisplayName: "Current Agent"}, {ID: "other", DisplayName: "Other Agent"}}
			host := &recordingTurnHost{response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn", ModelProvider: "test", Model: "test", NextRunStatus: AgentRunStatusRunning,
				ProposedDelegation: &TurnDelegationProposal{StepID: "review", AssignedAgentID: "agent", Goal: "Review the evidence"},
			}}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1", EligibleAgents: targets})
			if err != nil {
				t.Fatal(err)
			}
			request, err := runner.buildRequest(TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}})
			if err != nil {
				t.Fatal(err)
			}
			wantTargets := 2
			if taskWork {
				wantTargets = 1
			}
			if len(request.EligibleAgents) != wantTargets || request.EligibleAgents[0].ID != run.AssignedAgentID || len(targets) != 2 {
				t.Fatalf("wrong hosted task targets: %#v", request.EligibleAgents)
			}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}})
			if taskWork {
				if err == nil || outcome != nil {
					t.Fatalf("task work delegated despite fork-only execution: %#v %v", outcome, err)
				}
			} else if err != nil || outcome == nil || outcome.ProposedDelegation == nil {
				t.Fatalf("ordinary delegation changed: %#v %v", outcome, err)
			}
		})
	}
}

func TestHostedTaskAuthorityRequiresRemainingChildBudget(t *testing.T) {
	for name, run := range map[string]*AgentRun{
		"remaining attempts": {Budget: &BudgetPolicy{MaxAttempts: 6}, BudgetUsage: BudgetUsage{Attempts: 4}},
		"remaining tokens":   {Budget: &BudgetPolicy{MaxInputTokens: 32768}, BudgetUsage: BudgetUsage{InputTokens: 32000}},
		"available":          {Budget: &BudgetPolicy{MaxAttempts: 6, MaxInputTokens: 65536}},
	} {
		t.Run(name, func(t *testing.T) {
			run.ID, run.Kind, run.Scope = "foreground", RunKindConversation, Scope{Kind: "tenant", ID: "one"}
			run.Context = map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "message"}
			trusted := &HostedConversationTaskContext{ConversationID: "chat", CanStart: true}
			runner, err := NewHostedTurnRunner(&recordingTurnHost{}, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1", ConversationTasks: trusted})
			if err != nil {
				t.Fatal(err)
			}
			request, err := runner.buildRequest(TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}})
			if err != nil || request.ConversationTasks.CanStart != (name == "available") || !trusted.CanStart {
				t.Fatalf("budget task authority: %#v %v", request.ConversationTasks, err)
			}
		})
	}
}

func TestHostedTaskBudgetCompletesRemainingCapacityAndChecksFloor(t *testing.T) {
	for name, allocation := range map[string]*BudgetPolicy{
		"omitted": nil, "partial": {MaxAttempts: 4}, "below floor": {MaxAttempts: 1}, "above capacity": {MaxAttempts: 7},
	} {
		t.Run(name, func(t *testing.T) {
			run := &AgentRun{ID: "foreground", Kind: RunKindConversation, Scope: Scope{Kind: "tenant", ID: "one"},
				Budget:      &BudgetPolicy{MaxAttempts: 6, MaxTurns: 6, MaxInputTokens: 262144, MaxOutputTokens: 65536, MaxTotalTokens: 327680},
				BudgetUsage: BudgetUsage{Attempts: 1, Turns: 1, InputTokens: 1000, OutputTokens: 100},
				Context:     map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "message"}}
			proposal := taskProposalFixture()
			proposal.Budget = allocation
			host := &recordingTurnHost{response: &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: "turn", ModelProvider: "test", Model: "test", ProposedTask: proposal, NextRunStatus: AgentRunStatusRunning}}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1", ConversationTasks: &HostedConversationTaskContext{ConversationID: "chat", CanStart: true}})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}})
			if name == "below floor" || name == "above capacity" {
				if err == nil || outcome != nil {
					t.Fatalf("unsafe task budget accepted: %#v %v", outcome, err)
				}
				return
			}
			if err != nil || outcome == nil || outcome.ProposedTask == nil || outcome.ProposedTask.Budget == nil {
				t.Fatalf("remaining task budget omitted: %#v %v", outcome, err)
			}
			budget := outcome.ProposedTask.Budget
			attempts := int64(5)
			if allocation != nil {
				attempts = 4
			}
			if budget.MaxAttempts != attempts || budget.MaxTurns != 5 || budget.MaxInputTokens != 261144 || budget.MaxOutputTokens != 65436 || budget.MaxTotalTokens != 326580 || budget.MaxCostMicros != 0 {
				t.Fatalf("task lost remaining bounds: %#v", budget)
			}
		})
	}
}

func TestTurnTaskProposalRejectsUnboundedIntent(t *testing.T) {
	for name, mutate := range map[string]func(*TurnTaskProposal){
		"key":            func(p *TurnTaskProposal) { p.TaskKey = strings.Repeat("k", 129) },
		"key identity":   func(p *TurnTaskProposal) { p.TaskKey = "source/run" },
		"goal":           func(p *TurnTaskProposal) { p.Goal = strings.Repeat("g", 16385) },
		"empty goal":     func(p *TurnTaskProposal) { p.Goal = " " },
		"acknowledgment": func(p *TurnTaskProposal) { p.Acknowledgment = strings.Repeat("a", 601) },
		"empty ack":      func(p *TurnTaskProposal) { p.Acknowledgment = " " },
		"budget":         func(p *TurnTaskProposal) { p.Budget = &BudgetPolicy{MaxTurns: -1} },
	} {
		t.Run(name, func(t *testing.T) {
			proposal := taskProposalFixture()
			mutate(proposal)
			if proposal.Validate() == nil {
				t.Fatal("invalid task intent was accepted")
			}
		})
	}
	if err := taskProposalFixture().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestHostedTaskProposalRequiresCanonicalForegroundAndCommitBeforeAck(t *testing.T) {
	for name, mutate := range map[string]func(*AgentRun, *HostedTurnResponse, *HostedConversationTaskContext){
		"authorized": func(*AgentRun, *HostedTurnResponse, *HostedConversationTaskContext) {},
		"task worker": func(run *AgentRun, _ *HostedTurnResponse, _ *HostedConversationTaskContext) {
			run.Kind = RunKindAgentWork
		},
		"nested foreground": func(run *AgentRun, _ *HostedTurnResponse, _ *HostedConversationTaskContext) {
			run.ParentRunID = "parent"
		},
		"different conversation": func(_ *AgentRun, _ *HostedTurnResponse, taskContext *HostedConversationTaskContext) {
			taskContext.ConversationID = "other"
		},
		"different thread": func(_ *AgentRun, _ *HostedTurnResponse, taskContext *HostedConversationTaskContext) {
			taskContext.ThreadRootID = "other"
		},
		"unavailable": func(_ *AgentRun, _ *HostedTurnResponse, taskContext *HostedConversationTaskContext) {
			taskContext.CanStart = false
		},
		"completion before commit": func(_ *AgentRun, response *HostedTurnResponse, _ *HostedConversationTaskContext) {
			response.NextRunStatus = AgentRunStatusCompleted
		},
		"summary before commit": func(_ *AgentRun, response *HostedTurnResponse, _ *HostedConversationTaskContext) {
			response.OutputSummary = response.ProposedTask.Acknowledgment
		},
		"output before commit": func(_ *AgentRun, response *HostedTurnResponse, _ *HostedConversationTaskContext) {
			response.RunOutput = map[string]interface{}{"summary": response.ProposedTask.Acknowledgment}
		},
		"second proposal": func(_ *AgentRun, response *HostedTurnResponse, _ *HostedConversationTaskContext) {
			response.ProposedRunbook = &TurnRunbookProposal{Entrypoint: "release-review", Summary: "Run release review"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := &AgentRun{ID: "foreground", Kind: RunKindConversation, Scope: Scope{Kind: "tenant", ID: "one"},
				Context: map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "user-message", "threadRootMessageId": "thread"}}
			response := &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: "turn", ModelProvider: "test", Model: "test",
				ProposedTask: taskProposalFixture(), NextRunStatus: AgentRunStatusRunning}
			taskContext := &HostedConversationTaskContext{ConversationID: "chat", ThreadRootID: "thread", CanStart: true}
			mutate(run, response, taskContext)
			host := &recordingTurnHost{response: response}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1", ConversationTasks: taskContext})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}})
			if name != "authorized" {
				if err == nil || outcome != nil {
					t.Fatalf("unauthorized task accepted: outcome=%#v err=%v", outcome, err)
				}
				return
			}
			if err != nil || outcome == nil || !reflect.DeepEqual(outcome.ProposedTask, response.ProposedTask) || outcome.ProposedTask == response.ProposedTask {
				t.Fatalf("task proposal was lost or aliased: outcome=%#v err=%v", outcome, err)
			}
			if outcome.NextRunStatus != AgentRunStatusRunning || outcome.OutputSummary != "" || len(outcome.RunOutput) != 0 {
				t.Fatal("proposal exposed its acknowledgment before durable admission")
			}
		})
	}
}

func TestHostedTaskFormSchemaAndRoundTrip(t *testing.T) {
	form := HostedTurnForm{SchemaVersion: HostedTurnFormSchemaVersion, ProposedTask: taskProposalFixture(), NextRunStatus: AgentRunStatusRunning}
	response, err := CompileHostedTurnForm(form, nil)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := HostedTurnFormFromResponse(*response)
	if err != nil || !reflect.DeepEqual(projected.ProposedTask, form.ProposedTask) {
		t.Fatalf("task proposal did not round trip: %#v %v", projected, err)
	}
	for _, available := range []bool{false, true} {
		schema, err := HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{CanStartTask: available})
		if err != nil {
			t.Fatal(err)
		}
		properties := schema["properties"].(map[string]interface{})
		proposal, included := properties["proposedTask"]
		if included != available {
			t.Fatalf("task schema authority mismatch: %#v", properties)
		}
		if included {
			contract := proposal.(map[string]interface{})
			fields := contract["properties"].(map[string]interface{})
			if len(fields) != 4 || contract["additionalProperties"] != false {
				t.Fatalf("task schema exposed execution authority: %#v", fields)
			}
		}
	}
	for name, mutate := range map[string]func(*HostedTurnForm){
		"streamed ack": func(form *HostedTurnForm) { form.ProgressSummary = "Started." },
		"summary ack":  func(form *HostedTurnForm) { form.OutputSummary = "Started." },
		"completed":    func(form *HostedTurnForm) { form.NextRunStatus = AgentRunStatusCompleted },
		"fork":         func(form *HostedTurnForm) { form.ProposedFork = &TurnForkProposal{} },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := form
			mutate(&invalid)
			if _, err := CompileHostedTurnForm(invalid, nil); err == nil {
				t.Fatal("invalid task form was accepted")
			}
		})
	}
}

func TestTaskTurnCloneAndOutcomeValidation(t *testing.T) {
	turn := &AgentTurn{RequestedTask: taskProposalFixture()}
	turn.RequestedTask.Budget = &BudgetPolicy{MaxTurns: 3}
	copy := cloneAgentTurn(turn)
	copy.RequestedTask.Budget.MaxTurns = 4
	if turn.RequestedTask.Budget.MaxTurns != 3 {
		t.Fatal("durable turn clone shared task budget state")
	}
	outcome := &TurnOutcome{ProposedTask: taskProposalFixture(), NextRunStatus: AgentRunStatusRunning}
	if err := validateTurnOutcome(AgentRunStatusRunning, outcome); err != nil {
		t.Fatal(err)
	}
	outcome.RunOutput = map[string]interface{}{"summary": "Started."}
	if validateTurnOutcome(AgentRunStatusRunning, outcome) == nil {
		t.Fatal("non-hosted turn bypassed commit-before-ack validation")
	}
}

func TestHostedTaskAuthorityRetainsTrustedConversationSourceThroughAdapter(t *testing.T) {
	source := &AgentRun{ID: "foreground", Kind: RunKindConversation, Scope: Scope{Kind: "tenant", ID: "one"},
		Owner:   ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		Context: map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "user-message"}}
	input := TurnExecutionContext{Run: cloneAgentRun(source), ForegroundConversation: source, Turn: &AgentTurn{ID: "turn"}}
	input.Run.Kind = RunKindAgentWork
	host := &recordingTurnHost{response: &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: "turn", ModelProvider: "test", Model: "test",
		ProposedTask: taskProposalFixture(), NextRunStatus: AgentRunStatusRunning}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1",
		ConversationTasks: &HostedConversationTaskContext{ConversationID: "chat", CanStart: true}})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := runner.RunTurn(t.Context(), input); err != nil || outcome == nil || outcome.ProposedTask == nil {
		t.Fatalf("conversation adapter lost source authority: %#v %v", outcome, err)
	}
	input.ForegroundConversation = cloneAgentRun(source)
	input.ForegroundConversation.ID = "another-source"
	if outcome, err := runner.RunTurn(t.Context(), input); err == nil || outcome != nil {
		t.Fatal("unrelated foreground source authorized a task worker")
	}
}

func TestHostedTaskContextBoundsSnapshotCountAndUnicodeGoals(t *testing.T) {
	snapshot := ConversationTaskSnapshot{TaskID: "task", WorkRunID: "work", Goal: strings.Repeat("界", 512), Status: AgentRunStatusRunning,
		Revision: 1, AvailableControls: []string{RunActionCancel}, CreatedAt: time.Now()}
	value := &HostedConversationTaskContext{ConversationID: "chat", Tasks: []ConversationTaskSnapshot{snapshot}}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.Tasks[0].Goal += "界"
	if value.Validate() == nil {
		t.Fatal("task status context accepted an oversized Unicode goal")
	}
	value.Tasks = make([]ConversationTaskSnapshot, 11)
	if value.Validate() == nil {
		t.Fatal("task status context accepted an unbounded task list")
	}
	value.Tasks = []ConversationTaskSnapshot{snapshot}
	cloned := cloneHostedConversationTaskContext(value)
	cloned.Tasks[0].AvailableControls[0] = RunActionPause
	if value.Tasks[0].AvailableControls[0] != RunActionCancel {
		t.Fatal("snapshot controls were aliased across the host boundary")
	}
}
