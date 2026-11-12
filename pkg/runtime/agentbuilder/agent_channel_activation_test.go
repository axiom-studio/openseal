package agentbuilder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

type channelActionFixture struct {
	store       *sqlite.Store
	db          *sql.DB
	agents      *kernelagent.Registry
	endpoints   *externalconversation.ExternalConversationEndpointService
	resolver    *switchableAdapterResolver
	catalog     *skill.Catalog
	scope       models.Scope
	endpoint    *models.ExternalConversationEndpoint
	run         *models.AgentRun
	coordinator *action.ActionCoordinator
	dispatcher  *AgentBehaviorActionDispatcher
}

func newChannelActionFixture(t *testing.T) *channelActionFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "channels.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := kerneltest.OpenSQLiteDB(t, path)
	_, catalog, scope, adapter := slackConversationCatalog(t, ctx)
	resolver := &switchableAdapterResolver{ExternalConversationAdapterResolver: catalog}
	endpoints := externalconversation.NewExternalConversationEndpointService(store, resolver)
	endpoint, err := endpoints.Create(ctx, externalconversation.CreateExternalConversationEndpointRequest{
		ID: "slack-channel", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Slack channel", Adapter: adapter,
		Mode: capability.ConversationEndpointChannel, Address: "C012345",
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy:  models.ExternalConversationPolicy{MessageSelection: models.ExternalConversationSelectMentions, ReplyMode: models.ExternalConversationReplyThread, IgnoreBots: true},
		Status:  models.ExternalConversationEndpointActive,
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
	if err := catalog.Register(ctx, models.AgentManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "agents", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "slack-agent",
		SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
		AllowedActions: []string{models.AgentActionListChannels, models.AgentActionConfigureChannel}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"}, AssignedAgentID: "slack-agent", Goal: "Configure Slack", Source: models.RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil {
		t.Fatal(err)
	}
	validator, err := NewAgentBehaviorActionValidator(agents, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	policy := action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{Disposition: action.ActionDispositionRequireApproval, Reason: "Review routing", EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "operator"}}, ApprovalTTL: time.Hour}, nil
	})
	dispatcher, err := NewAgentBehaviorActionDispatcher(store, agents, nil, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	return &channelActionFixture{store: store, db: db, agents: agents, endpoints: endpoints, resolver: resolver, catalog: catalog, scope: scope, endpoint: endpoint, run: run,
		coordinator: action.NewActionCoordinator(store, store, catalog, policy, []action.ActionProposalValidator{validator}), dispatcher: dispatcher}
}

func (f *channelActionFixture) propose(t *testing.T, extra map[string]interface{}) (*models.ActionProposalResult, error) {
	t.Helper()
	arguments := map[string]interface{}{"endpointId": f.endpoint.ID, "messageSelection": "all_messages", "rationale": "Handle messages in the current channel"}
	for key, value := range extra {
		arguments[key] = value
	}
	return f.coordinator.Propose(context.Background(), actiontest.LoopProposeRequest(action.ProposeActionRequest{
		Scope: f.scope, RunID: f.run.ID, WorkerID: "conversation-worker", DeploymentID: "slack-agent",
		SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion, Action: models.AgentActionConfigureChannel,
		Arguments: arguments, IdempotencyKey: "configure-channel", Summary: "Configure channel",
	}, "configure-channel"))
}

func (f *channelActionFixture) approve(t *testing.T, proposal *models.ActionProposalResult) *models.ActionCall {
	t.Helper()
	resolved, err := action.NewApprovalCoordinator(f.store, f.store, action.EligibleApprovalAuthorizer{}).Resolve(context.Background(), action.ResolveApprovalRequest{
		Scope: f.scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "approve-channel", Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "user", ID: "operator"}, Reason: "Reviewed",
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
			retired := models.ExternalConversationEndpointRetired
			endpoint, err := f.endpoints.Update(context.Background(), f.scope, f.endpoint.ID, externalconversation.UpdateExternalConversationEndpointRequest{ExpectedRevision: f.endpoint.Revision, Status: &retired})
			if err != nil {
				t.Fatal(err)
			}
			extra := map[string]interface{}{}
			if status != "" {
				extra["status"] = status
			}
			if _, err := f.propose(t, extra); !errors.Is(err, models.ErrInvalidExternalConversation) || !strings.Contains(err.Error(), "retired") {
				t.Fatalf("retired channel proposal: %v", err)
			}
			approvals, err := f.store.ListApprovals(context.Background(), models.ApprovalFilter{Scope: f.scope, RunID: f.run.ID})
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
				retired := models.ExternalConversationEndpointRetired
				if _, err := f.endpoints.Update(ctx, f.scope, f.endpoint.ID, externalconversation.UpdateExternalConversationEndpointRequest{ExpectedRevision: f.endpoint.Revision, Status: &retired}); err != nil {
					t.Fatal(err)
				}
			case "adapter-unavailable":
				f.resolver.unavailable = true
			case "foreign-owner":
				foreign := models.CloneExternalConversationEndpoint(f.endpoint)
				foreign.Owner.ID = "other-agent"
				foreign.DeploymentID = "other-agent"
				payload, err := json.Marshal(foreign)
				if err != nil {
					t.Fatal(err)
				}
				// Simulate a corrupted association without relying on a public
				// endpoint revision change to catch the ownership violation.
				if _, err := f.db.ExecContext(ctx, `UPDATE external_conversation_endpoints SET owner_id=?, payload=? WHERE scope_kind=? AND scope_id=? AND id=?`, foreign.Owner.ID, string(payload), f.scope.Kind, f.scope.ID, foreign.ID); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := f.endpoints.Get(ctx, f.scope, f.endpoint.ID)
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := f.dispatcher.configureChannel(ctx, agentrun.ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); err == nil {
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
	if _, err := f.db.ExecContext(ctx, `CREATE TRIGGER reject_endpoint_update BEFORE UPDATE ON external_conversation_endpoints BEGIN SELECT RAISE(ABORT, 'endpoint write rejected'); END`); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := f.dispatcher.configureChannel(ctx, agentrun.ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); err == nil || !strings.Contains(err.Error(), "endpoint write rejected") {
			t.Fatalf("execution error: %v", err)
		}
		f.assertUnactivated(t)
		endpoint, _ := f.endpoints.Get(ctx, f.scope, f.endpoint.ID)
		if !equalJSON(endpoint, f.endpoint) {
			t.Fatal("failed transaction changed endpoint")
		}
	}
	if _, err := f.db.ExecContext(ctx, `DROP TRIGGER reject_endpoint_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dispatcher.configureChannel(ctx, agentrun.ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); err != nil {
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
	output, err := f.dispatcher.configureChannel(ctx, agentrun.ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := output["endpoint"].(*models.ExternalConversationEndpoint)
	if endpoint.Revision != f.endpoint.Revision+1 || endpoint.Policy.MessageSelection != models.ExternalConversationSelectAllMessages {
		t.Fatalf("endpoint was not committed: %#v", endpoint)
	}
	if replay, err := f.dispatcher.configureChannel(ctx, agentrun.ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); err != nil || replay["replayed"] != true {
		t.Fatalf("replay: %#v, %v", replay, err)
	}
	paused := models.ExternalConversationEndpointPaused
	later, err := f.endpoints.Update(ctx, f.scope, endpoint.ID, externalconversation.UpdateExternalConversationEndpointRequest{ExpectedRevision: endpoint.Revision, Status: &paused})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.dispatcher.configureChannel(ctx, agentrun.ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); !errors.Is(err, models.ErrExternalConversationConflict) {
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
	if _, err := f.propose(t, nil); !errors.Is(err, models.ErrInvalidExternalConversation) {
		t.Fatalf("unavailable adapter proposal: %v", err)
	}
	f.assertUnactivated(t)
	approvals, err := f.store.ListApprovals(context.Background(), models.ApprovalFilter{Scope: f.scope, RunID: f.run.ID})
	if err != nil || len(approvals) != 0 {
		t.Fatalf("unavailable adapter created approval: %#v, %v", approvals, err)
	}
}

func TestAgentChannelRejectsForeignTenantEndpointBeforeApproval(t *testing.T) {
	f := newChannelActionFixture(t)
	foreign := models.CloneExternalConversationEndpoint(f.endpoint)
	foreign.ID = "foreign-channel"
	foreign.Scope.ID = "another-tenant"
	foreign.IngressRoute = "another-route"
	if err := f.store.CreateExternalConversationEndpoint(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := f.propose(t, map[string]interface{}{"endpointId": foreign.ID}); !errors.Is(err, models.ErrExternalConversationEndpointNotFound) {
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
	if _, err := f.db.ExecContext(ctx, `CREATE TRIGGER change_endpoint_before_activation BEFORE INSERT ON agent_definitions BEGIN UPDATE external_conversation_endpoints SET revision=revision+1; END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dispatcher.configureChannel(ctx, agentrun.ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run); !errors.Is(err, models.ErrExternalConversationConflict) {
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
	paused := models.ExternalConversationEndpointPaused
	if _, err := f.endpoints.Update(ctx, f.scope, f.endpoint.ID, externalconversation.UpdateExternalConversationEndpointRequest{ExpectedRevision: f.endpoint.Revision, Status: &paused}); err != nil {
		t.Fatal(err)
	}
	proposal, err := f.propose(t, map[string]interface{}{"status": "active"})
	if err != nil {
		t.Fatal(err)
	}
	call := f.approve(t, proposal)
	output, err := f.dispatcher.configureChannel(ctx, agentrun.ActionDispatchInput{Call: call, Arguments: call.Arguments}, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if output["endpoint"].(*models.ExternalConversationEndpoint).Status != models.ExternalConversationEndpointActive {
		t.Fatalf("paused endpoint was not activated: %#v", output)
	}
}
