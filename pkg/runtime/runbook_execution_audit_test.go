package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// runbookAuditRecords is the audit's store: Runs and activations come from a
// real store, and the turns, action calls, approvals, artifacts, Agent
// requests and approval deliveries are records a test states for them. The
// audit only reads, so a test can give it states the workflows that write
// these records never leave together, such as a succeeded call under a
// pending approval. Each list matches only the filter fields the audit sets.
type runbookAuditRecords struct {
	*sqlite.Store
	turns      []*models.AgentTurn
	actions    routineActionCalls
	approvals  []*models.ApprovalCheckpoint
	artifacts  []*models.Artifact
	requests   []*models.AgentRequest
	endpoints  []*models.ExternalConversationEndpoint
	deliveries []*models.ExternalConversationDelivery
}

func (s *runbookAuditRecords) ListAgentTurns(_ context.Context, filter models.AgentTurnFilter) ([]*models.AgentTurn, error) {
	matched := make([]*models.AgentTurn, 0)
	for _, turn := range s.turns {
		if turn.Scope == filter.Scope && turn.RunID == filter.RunID && turn.Sequence > filter.AfterSequence {
			matched = append(matched, models.CloneAgentTurn(turn))
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Sequence < matched[j].Sequence })
	return routineRecordPage(matched, 0, filter.Limit), nil
}

func (s *runbookAuditRecords) ListActionCalls(ctx context.Context, filter models.ActionFilter) ([]*models.ActionCall, error) {
	return s.actions.ListActionCalls(ctx, filter)
}

func (s *runbookAuditRecords) ListApprovals(_ context.Context, filter models.ApprovalFilter) ([]*models.ApprovalCheckpoint, error) {
	matched := make([]*models.ApprovalCheckpoint, 0)
	for _, approval := range s.approvals {
		if approval.Scope == filter.Scope && approval.RunID == filter.RunID {
			matched = append(matched, models.CloneApprovalCheckpoint(approval))
		}
	}
	return routineRecordPage(matched, filter.Offset, filter.Limit), nil
}

func (s *runbookAuditRecords) ListArtifacts(_ context.Context, filter models.ArtifactFilter) ([]*models.Artifact, error) {
	matched := make([]*models.Artifact, 0)
	for _, artifact := range s.artifacts {
		if artifact.Scope == filter.Scope && artifact.Provenance.RunID == filter.ProducerRunID {
			matched = append(matched, models.CloneArtifact(artifact))
		}
	}
	return routineRecordPage(matched, filter.Offset, filter.Limit), nil
}

func (s *runbookAuditRecords) ListAgentRequests(_ context.Context, filter models.AgentRequestFilter) ([]*models.AgentRequest, error) {
	matched := make([]*models.AgentRequest, 0)
	for _, request := range s.requests {
		if request.Scope == filter.Scope && request.SourceRunID == filter.SourceRunID {
			matched = append(matched, models.CloneAgentRequest(request))
		}
	}
	return routineRecordPage(matched, filter.Offset, filter.Limit), nil
}

func (s *runbookAuditRecords) ListExternalConversationDeliveries(_ context.Context, filter models.ExternalConversationDeliveryFilter) ([]*models.ExternalConversationDelivery, error) {
	matched := make([]*models.ExternalConversationDelivery, 0)
	for _, delivery := range s.deliveries {
		if delivery.Scope == filter.Scope && delivery.Correlation != nil &&
			delivery.Correlation.Kind == filter.CorrelationKind && delivery.Correlation.ID == filter.CorrelationID {
			matched = append(matched, models.CloneExternalConversationDelivery(delivery))
		}
	}
	return routineRecordPage(matched, filter.Offset, filter.Limit), nil
}

func (s *runbookAuditRecords) GetExternalConversationEndpoint(_ context.Context, scope models.Scope, id string) (*models.ExternalConversationEndpoint, error) {
	for _, endpoint := range s.endpoints {
		if endpoint.Scope == scope && endpoint.ID == id {
			return models.CloneExternalConversationEndpoint(endpoint), nil
		}
	}
	return nil, models.ErrExternalConversationEndpointNotFound
}

func TestRunbookExecutionAuditProjectsNodeLineageWithoutGovernedValues(t *testing.T) {
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "audit"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "research", Version: "1", Name: "Research",
		Entrypoints: map[string]string{"daily": "browse"},
		Steps: map[string]runbook.Step{
			"browse": {Kind: runbook.StepAction, Name: "Browse sources", Action: &runbook.ActionStep{
				SkillID: "browser", SkillVersion: "1", Action: "browse", Arguments: map[string]runbook.Value{"url": routineRunbookLiteral("https://example.test")}, ResultPath: "/steps/browse", Next: "review",
			}},
			"review": {Kind: runbook.StepDelegate, Name: "Review findings", Delegate: &runbook.DelegateStep{
				AgentID: routineRunbookLiteral("reviewer"), Goal: runbook.Value{Template: []runbook.TemplateSegment{{Text: "Review "}, {Ref: "/steps/browse/title"}}}, ResultPath: "/steps/review", Next: "done",
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
	store := &runbookAuditRecords{Store: conversationtest.NewStore(t)}
	objective, err := portfolio.NewPortfolioService(store).CreateObjective(ctx, portfolio.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Research", Goal: "Find evidence", Status: models.ObjectiveStatusActive,
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
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID, Entrypoint: "daily",
		Goal: "Perform daily research", Source: models.RunSourceSchedule, Context: map[string]interface{}{"runbookActivationId": activation.ID},
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
	requestID := collaboration.StableCollaborationID(scope, requestKey, "request")
	childID := "child-review"
	persistedRun, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedRun.Checkpoint = checkpoint
	persistedRun.Status = models.AgentRunStatusRunning
	persistedRun.UpdatedAt = now.Add(2 * time.Minute)
	putRoutineRun(t, store.Store, persistedRun)
	store.turns = append(store.turns, &models.AgentTurn{ID: "turn-browse", Scope: scope, RunID: run.ID, Sequence: 1, Status: models.AgentTurnStatusCompleted, OutputSummary: "Requested browser", Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now})
	store.turns = append(store.turns, &models.AgentTurn{ID: "turn-review", Scope: scope, RunID: run.ID, Sequence: 2, Status: models.AgentTurnStatusCompleted, OutputSummary: "Delegated review", Revision: 1, CreatedAt: now.Add(2 * time.Minute), UpdatedAt: now.Add(2 * time.Minute), StartedAt: now.Add(2 * time.Minute)})
	store.actions.put(&models.ActionCall{
		ID: "action-browse", Scope: scope, RunID: run.ID, TurnID: "turn-browse", DeploymentID: owner.ID,
		SkillID: "browser", SkillVersion: "1", Action: "browse", Status: models.ActionCallStatusSucceeded, Risk: skill.RiskLevelRead,
		Arguments: map[string]interface{}{"url": "https://example.test/article", "password": "never-project-this"}, Output: map[string]interface{}{"title": "Useful article", "token": "nor-this"}, ApprovalID: "approval-browse",
		Attempt: 1, MaxAttempts: 2, Revision: 2, CreatedAt: now, UpdatedAt: now.Add(time.Minute),
	})
	store.approvals = append(store.approvals, &models.ApprovalCheckpoint{
		ID: "approval-browse", Scope: scope, RunID: run.ID, ActionCallID: "action-browse", Status: models.ApprovalStatusPending,
		Risk: skill.RiskLevelRead, Summary: "Approve browsing", ProposedAction: map[string]interface{}{"comment": "A concise reviewed reply", "password": "also-never-project-this"},
		EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "operator"}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	adapter := models.ExternalConversationAdapterReference{SkillID: "slack", SkillVersion: "1", BindingID: "slack-binding", BindingRevision: 1, AdapterID: "slack"}
	store.endpoints = append(store.endpoints, &models.ExternalConversationEndpoint{
		ID: "slack-approvals", IngressRoute: "slack-callback", Scope: scope, Owner: owner, DeploymentID: owner.ID,
		Name: "Reddit approvals", Adapter: adapter, Provider: "slack", Mode: capability.ConversationEndpointChannel,
		Address: "#team-reddit-agent-approvals", Status: models.ExternalConversationEndpointActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	store.deliveries = append(store.deliveries, &models.ExternalConversationDelivery{
		ID: "approval-delivery", Scope: scope, EndpointID: "slack-approvals", EndpointRevision: 1, Adapter: adapter,
		Operation: capability.ConversationDeliveryMessageSend, ConversationID: "approval-conversation", ChannelMessageID: "approval-message",
		OrderingKey: "approval-conversation", IdempotencyKey: "approval-delivery:approval-browse:slack-approvals",
		Correlation: &models.ExternalConversationDeliveryCorrelation{Kind: "approval", ID: "approval-browse", Phase: "request"},
		Status:      models.ExternalConversationDeliveryDelivered, Attempt: 1, MaximumAttempts: 5, AvailableAt: now,
		ProviderMessageID: "slack-message-1", Revision: 2, CreatedAt: now, UpdatedAt: now.Add(time.Minute), DeliveredAt: now.Add(time.Minute),
	})
	store.artifacts = append(store.artifacts, &models.Artifact{
		ID: "evidence", Version: 1, Scope: scope, Name: "Evidence", ContentRef: "opaque-secret-storage-ref", Digest: strings.Repeat("a", 64), SizeBytes: 12,
		Classification: models.ArtifactClassificationInternal, Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "agent", ID: owner.ID}, RunID: run.ID, TurnID: "turn-browse", ActionID: "action-browse"}, CreatedAt: now,
	})
	if err := store.CreateAgentRun(ctx, &models.AgentRun{
		ID: childID, Scope: scope, ObjectiveID: objective.ID, ParentRunID: run.ID, RootRunID: run.ID, Owner: owner, AssignedAgentID: "reviewer",
		Goal: "Review findings", Source: models.RunSourceRequest, Status: models.AgentRunStatusRunning, AvailableAt: now.Add(2 * time.Minute), QueueEnteredAt: now.Add(2 * time.Minute), Revision: 1, CreatedAt: now.Add(2 * time.Minute), UpdatedAt: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	store.requests = append(store.requests, &models.AgentRequest{
		ID: requestID, Scope: scope, Kind: models.AgentRequestKindRequest, Status: models.AgentRequestStatusAccepted,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: owner.ID}, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "reviewer"},
		SourceRunID: run.ID, ChildRunID: childID, Goal: "Review findings", AcceptancePolicy: models.AgentRequestAcceptanceRecipientReview,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	})

	audit, err := NewRunbookExecutionAuditService(store, registry).Get(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Run.ActivationID != activation.ID || audit.Run.PendingApprovals != 1 || audit.Run.PendingChildRuns != 1 || len(audit.Nodes) != 3 || len(audit.Lineage) != 1 {
		t.Fatalf("audit summary=%#v lineage=%#v", audit.Run, audit.Lineage)
	}
	if len(audit.Children) != 1 || audit.Children[0].Run.ID != childID || audit.Children[0].Run.Status != models.AgentRunStatusRunning {
		t.Fatalf("child execution audits=%#v", audit.Children)
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
		deliveries[0].Address != "#team-reddit-agent-approvals" || deliveries[0].Status != models.ExternalConversationDeliveryDelivered {
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
	scope := models.Scope{Kind: "tenant", ID: "conversation-audit"}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	store := &runbookAuditRecords{Store: conversationtest.NewStore(t)}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "assistant"},
		AssignedAgentID: "assistant", Goal: "Respond to an Agent channel message", Source: models.RunSourceRequest,
		Context: map[string]interface{}{"conversationId": "conversation-one"},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.turns = append(store.turns, &models.AgentTurn{
		ID: "turn-one", Scope: scope, RunID: run.ID, Sequence: 1, Status: models.AgentTurnStatusFailed,
		Error: "capability binding unavailable", Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now,
	})
	store.actions.put(&models.ActionCall{
		ID: "action-one", Scope: scope, RunID: run.ID, TurnID: "turn-one", DeploymentID: "assistant",
		BindingID: "bundled:agents", BindingRevision: 1, SkillID: models.AgentManagementSkillID,
		SkillVersion: models.AgentManagementSkillVersion, Action: models.AgentActionAmendBehavior,
		Status: models.ActionCallStatusFailed, Error: "binding unavailable", Attempt: 1, MaxAttempts: 1,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	})

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
	attachRunbookTurns(result, []*models.AgentTurn{
		{ID: "turn-1", Sequence: 1, Status: models.AgentTurnStatusFailed, StartedAt: now},
		{ID: "turn-2", Sequence: 2, Status: models.AgentTurnStatusCompleted, StartedAt: now.Add(time.Minute)},
		{ID: "turn-unassigned", Sequence: 3, Status: models.AgentTurnStatusCompleted, StartedAt: now.Add(2 * time.Minute)},
	})
	attachRunbookActions(result, []*models.ActionCall{
		{ID: "action-1", TurnID: "turn-1", SkillID: "browser", SkillVersion: "1", Action: "browse", Status: models.ActionCallStatusFailed},
		{ID: "action-2", TurnID: "turn-2", SkillID: "browser", SkillVersion: "1", Action: "browse", Status: models.ActionCallStatusSucceeded},
		{ID: "action-unassigned", TurnID: "turn-unassigned", SkillID: "browser", SkillVersion: "1", Action: "browse", Status: models.ActionCallStatusSucceeded},
	}, []*models.ApprovalCheckpoint{
		{ID: "approval-2", ActionCallID: "action-2", Status: models.ApprovalStatusApproved, Summary: "Approved"},
		{ID: "approval-unassigned", ActionCallID: "missing", Status: models.ApprovalStatusPending, Summary: "Unassigned"},
	})

	visits := result.Nodes[0].Visits
	if len(visits) != 2 || visits[0].Actions[0].ID != "action-1" || visits[1].Actions[0].ID != "action-2" || len(visits[0].Approvals) != 0 || visits[1].Approvals[0].ID != "approval-2" {
		t.Fatalf("visit-scoped records=%#v", visits)
	}
	if len(result.UnassignedTurns) != 1 || len(result.UnassignedActions) != 1 || len(result.UnassignedApprovals) != 1 {
		t.Fatalf("unassigned turns=%#v actions=%#v approvals=%#v", result.UnassignedTurns, result.UnassignedActions, result.UnassignedApprovals)
	}
}

func TestRunbookAuditPlacesDelegatedChildUnderVisit(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "audit"}
	parent := &models.AgentRun{ID: "run-parent", Scope: scope}
	now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	result := &RunbookExecutionAudit{Nodes: []RunbookNodeExecutionAudit{
		{StepID: "browse", Visits: []RunbookVisitExecutionAudit{{Trace: RunbookStepTrace{Sequence: 1, StepID: "browse", StartedAt: now}}}},
		{StepID: "review", Visits: []RunbookVisitExecutionAudit{
			{Trace: RunbookStepTrace{Sequence: 2, StepID: "review", StartedAt: now.Add(time.Minute)}},
			{Trace: RunbookStepTrace{Sequence: 4, StepID: "review", StartedAt: now.Add(10 * time.Minute)}},
		}},
	}}
	requestID := collaboration.StableCollaborationID(scope, control.DelegationIdempotencyKey(parent.ID, "review"), "request")
	children := []*models.AgentRun{
		{ID: "child-review", Scope: scope, ParentRunID: parent.ID, AssignedAgentID: "reviewer", CreatedAt: now.Add(2 * time.Minute)},
		{ID: "child-unrelated", Scope: scope, ParentRunID: parent.ID, AssignedAgentID: "other", CreatedAt: now.Add(3 * time.Minute)},
	}
	requests := []*models.AgentRequest{
		{ID: requestID, Scope: scope, SourceRunID: parent.ID, ChildRunID: "child-review"},
		// A request whose id is not derived from (parent, step) is not mapped.
		{ID: "request-other", Scope: scope, SourceRunID: parent.ID, ChildRunID: "child-unrelated"},
	}
	attachRunbookChildren(result, children, requests, parent)

	review := result.Nodes[1]
	if len(review.Visits[0].ChildRuns) != 1 || review.Visits[0].ChildRuns[0].ID != "child-review" || review.Visits[0].ChildRuns[0].StepID != "review" {
		t.Fatalf("review visit children=%#v", review.Visits[0].ChildRuns)
	}
	if len(review.Visits[1].ChildRuns) != 0 || len(result.Nodes[0].Visits[0].ChildRuns) != 0 {
		t.Fatalf("child placed on wrong visit: %#v", result.Nodes)
	}
	if len(result.UnassignedChildren) != 1 || result.UnassignedChildren[0].ID != "child-unrelated" {
		t.Fatalf("unassigned children=%#v", result.UnassignedChildren)
	}
}

func timePointer(value time.Time) *time.Time { return &value }
