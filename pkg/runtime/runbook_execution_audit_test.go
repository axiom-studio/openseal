package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestRunbookExecutionAuditProjectsNodeLineageWithoutGovernedValues(t *testing.T) {
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "audit"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "research", Version: "1", Name: "Research",
		Entrypoints: map[string]string{"daily": "browse"},
		Steps: map[string]runbook.Step{
			"browse": {Kind: runbook.StepAction, Name: "Browse sources", Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "browse", Arguments: map[string]runbook.Value{"url": runbookLiteral("https://example.test")}, ResultPath: "/steps/browse", Next: "review",
			}},
			"review": {Kind: runbook.StepDelegate, Name: "Review findings", Delegate: &runbook.DelegateStep{
				AgentID: runbookLiteral("reviewer"), Goal: runbook.Value{Template: []runbook.TemplateSegment{{Text: "Review "}, {Ref: "/steps/browse/title"}}}, ResultPath: "/steps/review", Next: "done",
			}},
			"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"report": {Ref: "/steps/review"}}}},
		},
	}
	registry := kernelagent.NewRegistryWithStore(kernelagent.NewMemoryStore())
	if _, err := registry.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "researcher-definition", Version: "1", DisplayName: "Researcher", Purpose: "Research", SystemPrompt: "Research carefully.",
		SkillRequirements: []kernelagent.SkillRequirement{{SkillID: "browser", RequiredActions: []string{"browse"}}},
		Authority:         kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, AllowedSkillIDs: []string{"browser"}, MaxConcurrentRuns: 1}, Runbook: definition,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: owner.ID, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "researcher-definition", ActiveVersion: "1",
		RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "audit"); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Research", Goal: "Find evidence", Status: ObjectiveStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := NewRunbookActivationService(store).Create(ctx, CreateRunbookActivationRequest{
		ID: "daily-research", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID,
		DefinitionID: definition.ID, DefinitionVersion: definition.Version, TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "daily", Schedule: &runbook.Schedule{Cron: "0 0 9 * * *", Timezone: "UTC"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID, Entrypoint: "daily",
		Goal: "Perform daily research", Source: RunSourceSchedule, Context: map[string]interface{}{"runbookActivationId": activation.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := map[string]interface{}{}
	browseSequence, _ := beginRunbookStepTrace(checkpoint, definition, "browse", "turn-browse", now)
	if err := setRunbookPointer(checkpoint, "/steps/browse", map[string]interface{}{"title": "A useful finding"}); err != nil {
		t.Fatal(err)
	}
	_ = updateRunbookStepTrace(checkpoint, browseSequence, RunbookStepTraceSucceeded, "review", "Governed action completed", "", map[string]string{"actionCallId": "action-browse", "approvalId": "approval-browse"}, now.Add(time.Minute))
	reviewSequence, _ := beginRunbookStepTrace(checkpoint, definition, "review", "turn-review", now.Add(2*time.Minute))
	_ = updateRunbookStepTrace(checkpoint, reviewSequence, RunbookStepTraceWaiting, "", "Waiting for delegated Agent work", "", nil, now.Add(2*time.Minute))

	requestKey := strings.Join([]string{"delegation", run.ID, "review"}, ":")
	requestID := stableCollaborationID(scope, requestKey, "request")
	childID := "child-review"
	store.mu.Lock()
	persistedRun := cloneAgentRun(store.agentRuns[portfolioKey(scope, run.ID)])
	persistedRun.Checkpoint = checkpoint
	persistedRun.Status = AgentRunStatusRunning
	persistedRun.UpdatedAt = now.Add(2 * time.Minute)
	store.agentRuns[portfolioKey(scope, run.ID)] = persistedRun
	store.turns[portfolioKey(scope, run.ID)] = map[string]*AgentTurn{
		"turn-browse": {ID: "turn-browse", Scope: scope, RunID: run.ID, Sequence: 1, Status: AgentTurnStatusCompleted, OutputSummary: "Requested browser", Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now},
		"turn-review": {ID: "turn-review", Scope: scope, RunID: run.ID, Sequence: 2, Status: AgentTurnStatusCompleted, OutputSummary: "Delegated review", RequestedDelegation: &TurnDelegationProposal{StepID: "review", AssignedAgentID: "reviewer", Goal: "Review findings"}, Revision: 1, CreatedAt: now.Add(2 * time.Minute), UpdatedAt: now.Add(2 * time.Minute), StartedAt: now.Add(2 * time.Minute)},
	}
	store.actions[portfolioKey(scope, "action-browse")] = &ActionCall{
		ID: "action-browse", Scope: scope, RunID: run.ID, TurnID: "turn-browse", DeploymentID: owner.ID,
		SkillID: "browser", SkillVersion: "1", Action: "browse", Status: ActionCallStatusSucceeded, Risk: skill.RiskLevelRead,
		Arguments: map[string]interface{}{"url": "https://example.test/article", "password": "never-project-this"}, Output: map[string]interface{}{"title": "Useful article", "token": "nor-this"}, ApprovalID: "approval-browse",
		Attempt: 1, MaxAttempts: 2, Revision: 2, CreatedAt: now, UpdatedAt: now.Add(time.Minute),
	}
	store.approvals[portfolioKey(scope, "approval-browse")] = &ApprovalCheckpoint{
		ID: "approval-browse", Scope: scope, RunID: run.ID, ActionCallID: "action-browse", Status: ApprovalStatusPending,
		Risk: skill.RiskLevelRead, Summary: "Approve browsing", ProposedAction: map[string]interface{}{"comment": "A concise reviewed reply", "password": "also-never-project-this"},
		EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "operator"}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	adapter := ExternalConversationAdapterReference{SkillID: "slack", SkillVersion: "1", BindingID: "slack-binding", BindingRevision: 1, AdapterID: "slack"}
	store.externalEndpoints[externalConversationEndpointKey(scope, "slack-approvals")] = &ExternalConversationEndpoint{
		ID: "slack-approvals", IngressRoute: "slack-callback", Scope: scope, Owner: owner, DeploymentID: owner.ID,
		Name: "Reddit approvals", Adapter: adapter, Provider: "slack", Mode: capability.ConversationEndpointChannel,
		Address: "#team-reddit-agent-approvals", Status: ExternalConversationEndpointActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	store.externalDeliveries[externalConversationDeliveryKey(scope, "approval-delivery")] = &ExternalConversationDelivery{
		ID: "approval-delivery", Scope: scope, EndpointID: "slack-approvals", EndpointRevision: 1, Adapter: adapter,
		Operation: capability.ConversationDeliveryMessageSend, ConversationID: "approval-conversation", ChannelMessageID: "approval-message",
		OrderingKey: "approval-conversation", IdempotencyKey: "approval-delivery:approval-browse:slack-approvals",
		Correlation: &ExternalConversationDeliveryCorrelation{Kind: "approval", ID: "approval-browse", Phase: "request"},
		Status:      ExternalConversationDeliveryDelivered, Attempt: 1, MaximumAttempts: 5, AvailableAt: now,
		ProviderMessageID: "slack-message-1", Revision: 2, CreatedAt: now, UpdatedAt: now.Add(time.Minute), DeliveredAt: now.Add(time.Minute),
	}
	store.artifacts[artifactStorageKey(scope, "evidence")] = map[int64]*Artifact{1: {
		ID: "evidence", Version: 1, Scope: scope, Name: "Evidence", ContentRef: "opaque-secret-storage-ref", Digest: strings.Repeat("a", 64), SizeBytes: 12,
		Classification: ArtifactClassificationInternal, Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: owner.ID}, RunID: run.ID, TurnID: "turn-browse", ActionID: "action-browse"}, CreatedAt: now,
	}}
	store.agentRuns[portfolioKey(scope, childID)] = &AgentRun{
		ID: childID, Scope: scope, ObjectiveID: objective.ID, ParentRunID: run.ID, RootRunID: run.ID, Owner: owner, AssignedAgentID: "reviewer",
		Goal: "Review findings", Source: RunSourceRequest, Status: AgentRunStatusRunning, AvailableAt: now.Add(2 * time.Minute), QueueEnteredAt: now.Add(2 * time.Minute), Revision: 1, CreatedAt: now.Add(2 * time.Minute), UpdatedAt: now.Add(2 * time.Minute),
	}
	store.requests[requestStoreKey(scope, requestID)] = &AgentRequest{
		ID: requestID, Scope: scope, Kind: AgentRequestKindRequest, Status: AgentRequestStatusAccepted,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: owner.ID}, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "reviewer"},
		SourceRunID: run.ID, ChildRunID: childID, Goal: "Review findings", AcceptancePolicy: AgentRequestAcceptanceRecipientReview,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	store.mu.Unlock()

	audit, err := NewRunbookExecutionAuditService(store, registry).Get(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Run.ActivationID != activation.ID || audit.Run.PendingApprovals != 1 || audit.Run.PendingChildRuns != 1 || len(audit.Nodes) != 3 || len(audit.Lineage) != 1 {
		t.Fatalf("audit summary=%#v lineage=%#v", audit.Run, audit.Lineage)
	}
	nodes := map[string]RunbookNodeExecutionAudit{}
	for _, node := range audit.Nodes {
		nodes[node.StepID] = node
	}
	browseVisit := nodes["browse"].Visits[0]
	if nodes["browse"].Status != RunbookStepTraceSucceeded || len(browseVisit.Actions) != 1 || len(browseVisit.Approvals) != 1 || len(browseVisit.Artifacts) != 1 || len(browseVisit.Trace.Outputs) != 1 || browseVisit.Trace.Outputs[0].Shape != "object" {
		t.Fatalf("browse node=%#v", nodes["browse"])
	}
	if browseVisit.Actions[0].Arguments["url"] != "https://example.test/article" || browseVisit.Actions[0].Arguments["password"] != "[REDACTED]" ||
		browseVisit.Actions[0].Result["title"] != "Useful article" || browseVisit.Actions[0].Result["token"] != "[REDACTED]" ||
		browseVisit.Approvals[0].ProposedAction["comment"] != "A concise reviewed reply" || browseVisit.Approvals[0].ProposedAction["password"] != "[REDACTED]" {
		t.Fatalf("safe action and approval facts=%#v %#v", browseVisit.Actions[0], browseVisit.Approvals[0])
	}
	if deliveries := browseVisit.Approvals[0].Deliveries; len(deliveries) != 1 || deliveries[0].Provider != "slack" ||
		deliveries[0].Address != "#team-reddit-agent-approvals" || deliveries[0].Status != ExternalConversationDeliveryDelivered {
		t.Fatalf("approval deliveries=%#v", deliveries)
	}
	reviewVisit := nodes["review"].Visits[0]
	if nodes["review"].Status != RunbookStepTraceWaiting || len(reviewVisit.ChildRuns) != 1 || reviewVisit.ChildRuns[0].ID != childID || len(reviewVisit.Trace.Inputs) != 1 || reviewVisit.Trace.Inputs[0].Availability != RunbookDataAvailable {
		t.Fatalf("review node=%#v", nodes["review"])
	}
	if audit.Lineage[0].FromSequence != browseSequence || audit.Lineage[0].ToSequence != reviewSequence || audit.Lineage[0].Ref != "/steps/browse/title" {
		t.Fatalf("actual lineage=%#v", audit.Lineage)
	}
	encoded, _ := json.Marshal(audit)
	for _, forbidden := range []string{"never-project-this", "nor-this", "also-never-project-this", "opaque-secret-storage-ref"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("audit exposed governed value %q: %s", forbidden, encoded)
		}
	}
}

func TestRunExecutionAuditProjectsConversationalRunAsOneGraph(t *testing.T) {
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "conversation-audit"}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "assistant"},
		AssignedAgentID: "assistant", Goal: "Respond to an Agent channel message", Source: RunSourceRequest,
		Context: map[string]interface{}{"conversationId": "conversation-one"},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.turns[portfolioKey(scope, run.ID)] = map[string]*AgentTurn{
		"turn-one": {
			ID: "turn-one", Scope: scope, RunID: run.ID, Sequence: 1, Status: AgentTurnStatusFailed,
			Error: "capability binding unavailable", Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now,
		},
	}
	store.actions[portfolioKey(scope, "action-one")] = &ActionCall{
		ID: "action-one", Scope: scope, RunID: run.ID, TurnID: "turn-one", DeploymentID: "assistant",
		BindingID: "bundled:agents", BindingRevision: 1, SkillID: AgentManagementSkillID,
		SkillVersion: AgentManagementSkillVersion, Action: AgentActionAmendBehavior,
		Status: ActionCallStatusFailed, Error: "binding unavailable", Attempt: 1, MaxAttempts: 1,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	store.mu.Unlock()

	audit, err := NewRunbookExecutionAuditService(store, nil).Get(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Run.ID != run.ID || audit.Run.ActivationID != "" || audit.Runbook != nil ||
		len(audit.Nodes) != 0 || len(audit.UnassignedTurns) != 1 || len(audit.UnassignedActions) != 1 {
		t.Fatalf("conversation execution audit = %#v", audit)
	}
}

func TestRunbookExecutionAuditKeepsRetryRecordsOnExactVisits(t *testing.T) {
	now := time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC)
	definition := &runbook.Definition{
		Steps: map[string]runbook.Step{
			"browse": {Kind: runbook.StepAction, Name: "Browse", Action: &runbook.ActionStep{ResultPath: "/steps/browse"}},
		},
	}
	trace := &RunbookExecutionTrace{Revision: runbookTraceRevision, NextSequence: 3, Entries: []RunbookStepTrace{
		{Sequence: 1, StepID: "browse", StepKind: runbook.StepAction, Visit: 1, Status: RunbookStepTraceFailed, TurnID: "turn-1", ActionCallID: "action-1", StartedAt: now, CompletedAt: timePointer(now.Add(time.Second))},
		{Sequence: 2, StepID: "browse", StepKind: runbook.StepAction, Visit: 2, Status: RunbookStepTraceSucceeded, TurnID: "turn-2", ActionCallID: "action-2", StartedAt: now.Add(time.Minute), CompletedAt: timePointer(now.Add(time.Minute + time.Second))},
	}}
	result := &RunbookExecutionAudit{Nodes: buildRunbookNodeAudits(definition, trace), Trace: trace}
	attachRunbookTurns(result, []*AgentTurn{
		{ID: "turn-1", Sequence: 1, Status: AgentTurnStatusFailed, StartedAt: now},
		{ID: "turn-2", Sequence: 2, Status: AgentTurnStatusCompleted, StartedAt: now.Add(time.Minute)},
		{ID: "turn-unassigned", Sequence: 3, Status: AgentTurnStatusCompleted, StartedAt: now.Add(2 * time.Minute)},
	})
	attachRunbookActions(result, []*ActionCall{
		{ID: "action-1", TurnID: "turn-1", SkillID: "browser", SkillVersion: "1", Action: "browse", Status: ActionCallStatusFailed},
		{ID: "action-2", TurnID: "turn-2", SkillID: "browser", SkillVersion: "1", Action: "browse", Status: ActionCallStatusSucceeded},
		{ID: "action-unassigned", TurnID: "turn-unassigned", SkillID: "browser", SkillVersion: "1", Action: "browse", Status: ActionCallStatusSucceeded},
	}, []*ApprovalCheckpoint{
		{ID: "approval-2", ActionCallID: "action-2", Status: ApprovalStatusApproved, Summary: "Approved"},
		{ID: "approval-unassigned", ActionCallID: "missing", Status: ApprovalStatusPending, Summary: "Unassigned"},
	})

	visits := result.Nodes[0].Visits
	if len(visits) != 2 || visits[0].Actions[0].ID != "action-1" || visits[1].Actions[0].ID != "action-2" || len(visits[0].Approvals) != 0 || visits[1].Approvals[0].ID != "approval-2" {
		t.Fatalf("visit-scoped records=%#v", visits)
	}
	if len(result.UnassignedTurns) != 1 || len(result.UnassignedActions) != 1 || len(result.UnassignedApprovals) != 1 {
		t.Fatalf("unassigned turns=%#v actions=%#v approvals=%#v", result.UnassignedTurns, result.UnassignedActions, result.UnassignedApprovals)
	}
}
