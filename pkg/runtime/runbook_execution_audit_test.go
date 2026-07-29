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
		Arguments: map[string]interface{}{"password": "never-project-this"}, Output: map[string]interface{}{"token": "nor-this"}, ApprovalID: "approval-browse",
		Attempt: 1, MaxAttempts: 2, Revision: 2, CreatedAt: now, UpdatedAt: now.Add(time.Minute),
	}
	store.approvals[portfolioKey(scope, "approval-browse")] = &ApprovalCheckpoint{
		ID: "approval-browse", Scope: scope, RunID: run.ID, ActionCallID: "action-browse", Status: ApprovalStatusPending,
		Risk: skill.RiskLevelRead, Summary: "Approve browsing", ProposedAction: map[string]interface{}{"password": "also-never-project-this"},
		EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "operator"}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	store.artifacts[artifactStorageKey(scope, "evidence")] = map[int64]*Artifact{1: {
		ID: "evidence", Version: 1, Scope: scope, Name: "Evidence", ContentRef: "opaque-secret-storage-ref", Digest: strings.Repeat("a", 64), SizeBytes: 12,
		Classification: ArtifactClassificationInternal, Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: owner.ID}, RunID: run.ID, TurnID: "turn-browse", ActionID: "action-browse"}, CreatedAt: now,
	}}
	store.agentRuns[portfolioKey(scope, childID)] = &AgentRun{
		ID: childID, Scope: scope, ObjectiveID: objective.ID, ParentRunID: run.ID, RootRunID: run.ID, Owner: owner, AssignedAgentID: "reviewer",
		Goal: "Review findings", Source: RunSourceRequest, Status: AgentRunStatusRunning, AvailableAt: now, QueueEnteredAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
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
	if audit.Run.ActivationID != activation.ID || audit.Run.PendingApprovals != 1 || audit.Run.PendingChildRuns != 1 || len(audit.Nodes) != 3 || len(audit.Lineage) != 2 {
		t.Fatalf("audit summary=%#v lineage=%#v", audit.Run, audit.Lineage)
	}
	nodes := map[string]RunbookNodeExecutionAudit{}
	for _, node := range audit.Nodes {
		nodes[node.StepID] = node
	}
	if nodes["browse"].Status != RunbookStepTraceSucceeded || len(nodes["browse"].Actions) != 1 || len(nodes["browse"].Approvals) != 1 || len(nodes["browse"].Artifacts) != 1 {
		t.Fatalf("browse node=%#v", nodes["browse"])
	}
	if nodes["review"].Status != RunbookStepTraceWaiting || len(nodes["review"].ChildRuns) != 1 || nodes["review"].ChildRuns[0].ID != childID {
		t.Fatalf("review node=%#v", nodes["review"])
	}
	encoded, _ := json.Marshal(audit)
	for _, forbidden := range []string{"never-project-this", "nor-this", "also-never-project-this", "opaque-secret-storage-ref"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("audit exposed governed value %q: %s", forbidden, encoded)
		}
	}
}
