package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

type channelActionFixture struct {
	store       *SQLiteStore
	agents      *kernelagent.Registry
	endpoints   *ExternalConversationEndpointService
	resolver    *switchableConversationAdapterResolver
	catalog     *skill.Catalog
	scope       Scope
	endpoint    *ExternalConversationEndpoint
	run         *AgentRun
	coordinator *ActionCoordinator
	dispatcher  *AgentBehaviorActionDispatcher
}

func newChannelActionFixture(t *testing.T) *channelActionFixture {
	t.Helper()
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "channels.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, catalog, scope, adapter := externalConversationTestCatalog(t, ctx)
	resolver := &switchableConversationAdapterResolver{ExternalConversationAdapterResolver: catalog}
	endpoints := NewExternalConversationEndpointService(store, resolver)
	endpoint, err := endpoints.Create(ctx, CreateExternalConversationEndpointRequest{
		ID: "slack-channel", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Slack channel", Adapter: adapter,
		Mode: capability.ConversationEndpointChannel, Address: "C012345",
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy:  ExternalConversationPolicy{MessageSelection: ExternalConversationSelectMentions, ReplyMode: ExternalConversationReplyThread, IgnoreBots: true},
		Status:  ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	agents := kernelagent.NewRegistryWithStore(store)
	definition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "slack-definition", Version: "1.0.0", DisplayName: "Slack Agent", Purpose: "Help with Slack", SystemPrompt: "Work carefully.",
		Authority:  kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, MaxConcurrentRuns: 1},
		Amendments: workforce.AmendmentPolicy{AgentMayPropose: true, RequiresApproval: true, ApproverPrincipals: []string{"user:operator"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "slack-agent", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: definition.ID,
		ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive, Environment: "test", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "initial"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(ctx, AgentManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "agents", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "slack-agent",
		SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
		AllowedActions: []string{AgentActionListChannels, AgentActionConfigureChannel}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "slack-agent"}, AssignedAgentID: "slack-agent", Goal: "Configure Slack", Source: RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil {
		t.Fatal(err)
	}
	validator, err := NewAgentBehaviorActionValidator(agents, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	policy := ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionRequireApproval, Reason: "Review routing", EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "operator"}}, ApprovalTTL: time.Hour}, nil
	})
	dispatcher, err := NewAgentBehaviorActionDispatcher(store, agents, nil, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	return &channelActionFixture{store: store, agents: agents, endpoints: endpoints, resolver: resolver, catalog: catalog, scope: scope, endpoint: endpoint, run: run,
		coordinator: NewActionCoordinator(store, store, catalog, policy, validator), dispatcher: dispatcher}
}

func (f *channelActionFixture) propose(t *testing.T, extra map[string]interface{}) (*ActionProposalResult, error) {
	t.Helper()
	arguments := map[string]interface{}{"endpointId": f.endpoint.ID, "messageSelection": "all_messages", "rationale": "Handle messages in the current channel"}
	for key, value := range extra {
		arguments[key] = value
	}
	return f.coordinator.Propose(context.Background(), ProposeActionRequest{
		Scope: f.scope, RunID: f.run.ID, WorkerID: "conversation-worker", DeploymentID: "slack-agent",
		SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion, Action: AgentActionConfigureChannel,
		Arguments: arguments, IdempotencyKey: "configure-channel", Summary: "Configure channel",
	})
}

func (f *channelActionFixture) approve(t *testing.T, proposal *ActionProposalResult) *ActionCall {
	t.Helper()
	resolved, err := NewApprovalCoordinator(f.store, f.store, EligibleApprovalAuthorizer{}).Resolve(context.Background(), ResolveApprovalRequest{
		Scope: f.scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "approve-channel", Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "user", ID: "operator"}, Reason: "Reviewed",
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Call
}

func (f *channelActionFixture) assertUnactivated(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
	deployment, err := f.agents.GetDeployment(ctx, scope, "slack-agent")
	if err != nil || deployment.Revision != 1 || deployment.ActiveVersion != "1.0.0" {
		t.Fatalf("partial deployment change: %#v, %v", deployment, err)
	}
	activations, err := f.agents.ListActivations(ctx, scope, deployment.ID)
	if err != nil || len(activations) != 1 {
		t.Fatalf("partial activation: %#v, %v", activations, err)
	}
	versions, err := f.agents.ListDefinitionVersions(ctx, deployment.DefinitionID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("partial definition: %#v, %v", versions, err)
	}
	amendments, err := f.agents.ListAmendments(ctx, scope, deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, amendment := range amendments {
		if amendment.Status == kernelagent.AmendmentActivated {
			t.Fatalf("partial amendment activation: %#v", amendment)
		}
	}
}

func TestAgentChannelRejectsRetiredEndpointsBeforeApproval(t *testing.T) {
	for _, status := range []string{"active", "paused", ""} {
		t.Run(status, func(t *testing.T) {
			f := newChannelActionFixture(t)
			retired := ExternalConversationEndpointRetired
			endpoint, err := f.endpoints.Update(context.Background(), f.scope, f.endpoint.ID, UpdateExternalConversationEndpointRequest{ExpectedRevision: f.endpoint.Revision, Status: &retired})
			if err != nil {
				t.Fatal(err)
			}
			extra := map[string]interface{}{}
			if status != "" {
				extra["status"] = status
			}
			if _, err := f.propose(t, extra); !errors.Is(err, ErrInvalidExternalConversation) || !strings.Contains(err.Error(), "retired") {
				t.Fatalf("retired channel proposal: %v", err)
			}
			approvals, err := f.store.ListApprovals(context.Background(), ApprovalFilter{Scope: f.scope, RunID: f.run.ID})
			if err != nil || len(approvals) != 0 {
				t.Fatalf("invalid proposal created approval: %#v, %v", approvals, err)
			}
			f.assertUnactivated(t)
			unchanged, _ := f.endpoints.Get(context.Background(), f.scope, endpoint.ID)
			if !equalJSON(unchanged, endpoint) {
				t.Fatal("rejected proposal changed endpoint")
			}
			listed, err := f.dispatcher.listChannels(context.Background(), f.scope, "slack-agent")
			if err != nil {
				t.Fatal(err)
			}
			channel := listed["channels"].([]map[string]interface{})[0]
			if channel["configurable"] != false || channel["configurationBlocker"] == nil {
				t.Fatalf("retired channel is offered as configurable: %#v", channel)
			}
		})
	}
}

func TestAgentChannelRechecksEndpointAndAdapterAfterApproval(t *testing.T) {
	for _, change := range []string{"retirement", "adapter-unavailable", "foreign-owner"} {
		t.Run(change, func(t *testing.T) {
			f := newChannelActionFixture(t)
			proposal, err := f.propose(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			call := f.approve(t, proposal)
			ctx := context.Background()
			switch change {
			case "retirement":
				retired := ExternalConversationEndpointRetired
				if _, err := f.endpoints.Update(ctx, f.scope, f.endpoint.ID, UpdateExternalConversationEndpointRequest{ExpectedRevision: f.endpoint.Revision, Status: &retired}); err != nil {
					t.Fatal(err)
				}
			case "adapter-unavailable":
				f.resolver.unavailable = true
			case "foreign-owner":
				foreign := cloneExternalConversationEndpoint(f.endpoint)
				foreign.Owner.ID = "other-agent"
				foreign.DeploymentID = "other-agent"
				payload, err := json.Marshal(foreign)
				if err != nil {
					t.Fatal(err)
				}
				// Simulate a corrupted association without relying on a public
				// endpoint revision change to catch the ownership violation.
				if _, err := f.store.db.ExecContext(ctx, `UPDATE external_conversation_endpoints SET owner_id=?, payload=? WHERE scope_kind=? AND scope_id=? AND id=?`, foreign.Owner.ID, string(payload), f.scope.Kind, f.scope.ID, foreign.ID); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := f.endpoints.Get(ctx, f.scope, f.endpoint.ID)
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := f.dispatcher.configureChannel(ctx, ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); err == nil {
					t.Fatal("accepted stale or unavailable channel")
				}
				f.assertUnactivated(t)
			}
			after, _ := f.endpoints.Get(ctx, f.scope, f.endpoint.ID)
			if !equalJSON(before, after) {
				t.Fatal("rejected execution changed endpoint")
			}
		})
	}
}

func TestAgentChannelActivationRollsBackWhenEndpointWriteFails(t *testing.T) {
	f := newChannelActionFixture(t)
	proposal, err := f.propose(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := f.approve(t, proposal)
	ctx := context.Background()
	if _, err := f.store.db.ExecContext(ctx, `CREATE TRIGGER reject_endpoint_update BEFORE UPDATE ON external_conversation_endpoints BEGIN SELECT RAISE(ABORT, 'endpoint write rejected'); END`); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := f.dispatcher.configureChannel(ctx, ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); err == nil || !strings.Contains(err.Error(), "endpoint write rejected") {
			t.Fatalf("execution error: %v", err)
		}
		f.assertUnactivated(t)
		endpoint, _ := f.endpoints.Get(ctx, f.scope, f.endpoint.ID)
		if !equalJSON(endpoint, f.endpoint) {
			t.Fatal("failed transaction changed endpoint")
		}
	}
	if _, err := f.store.db.ExecContext(ctx, `DROP TRIGGER reject_endpoint_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dispatcher.configureChannel(ctx, ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); err != nil {
		t.Fatalf("retry after storage recovery: %v", err)
	}
}

func TestAgentChannelActivationAndReplayPreserveLaterEndpointChanges(t *testing.T) {
	f := newChannelActionFixture(t)
	proposal, err := f.propose(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := f.approve(t, proposal)
	ctx := context.Background()
	output, err := f.dispatcher.configureChannel(ctx, ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := output["endpoint"].(*ExternalConversationEndpoint)
	if endpoint.Revision != f.endpoint.Revision+1 || endpoint.Policy.MessageSelection != ExternalConversationSelectAllMessages {
		t.Fatalf("endpoint was not committed: %#v", endpoint)
	}
	if replay, err := f.dispatcher.configureChannel(ctx, ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); err != nil || replay["replayed"] != true {
		t.Fatalf("replay: %#v, %v", replay, err)
	}
	paused := ExternalConversationEndpointPaused
	later, err := f.endpoints.Update(ctx, f.scope, endpoint.ID, UpdateExternalConversationEndpointRequest{ExpectedRevision: endpoint.Revision, Status: &paused})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.dispatcher.configureChannel(ctx, ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); !errors.Is(err, ErrExternalConversationConflict) {
		t.Fatalf("replay overwrote newer endpoint: %v", err)
	}
	unchanged, _ := f.endpoints.Get(ctx, f.scope, endpoint.ID)
	if !equalJSON(unchanged, later) {
		t.Fatal("replay changed paused endpoint")
	}
}

func TestAgentChannelRejectsUnavailableAdapterBeforeApproval(t *testing.T) {
	f := newChannelActionFixture(t)
	f.resolver.unavailable = true
	if _, err := f.propose(t, nil); !errors.Is(err, ErrInvalidExternalConversation) {
		t.Fatalf("unavailable adapter proposal: %v", err)
	}
	f.assertUnactivated(t)
	approvals, err := f.store.ListApprovals(context.Background(), ApprovalFilter{Scope: f.scope, RunID: f.run.ID})
	if err != nil || len(approvals) != 0 {
		t.Fatalf("unavailable adapter created approval: %#v, %v", approvals, err)
	}
}

func TestAgentChannelRejectsForeignTenantEndpointBeforeApproval(t *testing.T) {
	f := newChannelActionFixture(t)
	foreign := cloneExternalConversationEndpoint(f.endpoint)
	foreign.ID = "foreign-channel"
	foreign.Scope.ID = "another-tenant"
	foreign.IngressRoute = "another-route"
	if err := f.store.CreateExternalConversationEndpoint(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := f.propose(t, map[string]interface{}{"endpointId": foreign.ID}); !errors.Is(err, ErrExternalConversationEndpointNotFound) {
		t.Fatalf("foreign endpoint proposal: %v", err)
	}
	f.assertUnactivated(t)
}

func TestAgentChannelActivationEndpointCASRollsBackWholeTransaction(t *testing.T) {
	f := newChannelActionFixture(t)
	proposal, err := f.propose(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := f.approve(t, proposal)
	ctx := context.Background()
	// Move the endpoint revision after preflight, inside the activation
	// transaction, to exercise its final CAS rather than only early validation.
	if _, err := f.store.db.ExecContext(ctx, `CREATE TRIGGER change_endpoint_before_activation BEFORE INSERT ON agent_definitions BEGIN UPDATE external_conversation_endpoints SET revision=revision+1; END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dispatcher.configureChannel(ctx, ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); !errors.Is(err, ErrExternalConversationConflict) {
		t.Fatalf("stale transaction commit: %v", err)
	}
	f.assertUnactivated(t)
	endpoint, _ := f.endpoints.Get(ctx, f.scope, f.endpoint.ID)
	if !equalJSON(endpoint, f.endpoint) {
		t.Fatal("endpoint CAS failure did not roll back transaction")
	}
}

func TestAgentChannelCanActivatePausedEndpoint(t *testing.T) {
	f := newChannelActionFixture(t)
	ctx := context.Background()
	paused := ExternalConversationEndpointPaused
	if _, err := f.endpoints.Update(ctx, f.scope, f.endpoint.ID, UpdateExternalConversationEndpointRequest{ExpectedRevision: f.endpoint.Revision, Status: &paused}); err != nil {
		t.Fatal(err)
	}
	proposal, err := f.propose(t, map[string]interface{}{"status": "active"})
	if err != nil {
		t.Fatal(err)
	}
	call := f.approve(t, proposal)
	output, err := f.dispatcher.configureChannel(ctx, ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if output["endpoint"].(*ExternalConversationEndpoint).Status != ExternalConversationEndpointActive {
		t.Fatalf("paused endpoint was not activated: %#v", output)
	}
}
