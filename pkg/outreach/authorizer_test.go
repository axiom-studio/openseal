package outreach

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/source"
)

type authorizationStateStub struct {
	run      *runtime.AgentRun
	project  *runtime.Project
	thread   *runtime.OutreachThread
	call     *runtime.ActionCall
	approval *runtime.ApprovalCheckpoint
	events   []*runtime.ActivityEvent
}

func (s *authorizationStateStub) GetAgentRun(context.Context, runtime.Scope, string) (*runtime.AgentRun, error) {
	return s.run, nil
}

func (s *authorizationStateStub) GetProject(context.Context, runtime.Scope, string) (*runtime.Project, error) {
	return s.project, nil
}

func (s *authorizationStateStub) GetOutreachThread(context.Context, runtime.Scope, string) (*runtime.OutreachThread, error) {
	return s.thread, nil
}

func (s *authorizationStateStub) GetActionCall(context.Context, runtime.Scope, string) (*runtime.ActionCall, error) {
	return s.call, nil
}

func (s *authorizationStateStub) GetApproval(context.Context, runtime.Scope, string) (*runtime.ApprovalCheckpoint, error) {
	if s.approval == nil {
		return nil, runtime.ErrApprovalNotFound
	}
	return s.approval, nil
}

func (s *authorizationStateStub) AppendActivity(_ context.Context, event *runtime.ActivityEvent) (*runtime.ActivityEvent, error) {
	for _, existing := range s.events {
		if existing.ID == event.ID {
			return nil, errors.New("duplicate")
		}
	}
	s.events = append(s.events, event)
	return event, nil
}

func (s *authorizationStateStub) ListActivity(_ context.Context, filter runtime.ActivityFilter) ([]*runtime.ActivityEvent, error) {
	var result []*runtime.ActivityEvent
	for _, event := range s.events {
		if event.Scope == filter.Scope && event.RunID == filter.RunID {
			result = append(result, event)
		}
	}
	return result, nil
}

func TestCanonicalInvocationAuthorizerReloadsEveryGovernedFact(t *testing.T) {
	state, invocation, policy := authorizedFixture(false)
	authorizer, err := NewCanonicalInvocationAuthorizer(state, PolicyResolverFunc(func(_ context.Context, scope skill.ScopeReference, reference string) (*source.Policy, error) {
		if scope.Kind != "local" || scope.ID != "research" || reference != "community@1" {
			t.Fatalf("policy lookup = %#v %q", scope, reference)
		}
		return policy, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := authorizer.AuthorizeOutreachInvocation(t.Context(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	if authorization.ApprovalPolicy != "human-review" || authorization.Decision.PolicyID != "community" || len(state.events) != 1 || state.events[0].ID != "outreach-policy-action-1" {
		t.Fatalf("authorization=%#v events=%#v", authorization, state.events)
	}
	if _, err := authorizer.AuthorizeOutreachInvocation(t.Context(), invocation); err != nil || len(state.events) != 1 {
		t.Fatalf("idempotent authorization err=%v events=%d", err, len(state.events))
	}
}

func TestCanonicalInvocationAuthorizerRequiresApprovedCheckpointAndExactAction(t *testing.T) {
	state, invocation, policy := authorizedFixture(true)
	authorizer, err := NewCanonicalInvocationAuthorizer(state, PolicyResolverFunc(func(context.Context, skill.ScopeReference, string) (*source.Policy, error) {
		return policy, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	state.approval.Status = runtime.ApprovalStatusPending
	if _, err := authorizer.AuthorizeOutreachInvocation(t.Context(), invocation); err == nil {
		t.Fatal("pending approval authorized delivery")
	}
	state.approval.Status = runtime.ApprovalStatusApproved
	state.call.Arguments["body"] = "unreviewed replacement"
	if _, err := authorizer.AuthorizeOutreachInvocation(t.Context(), invocation); err == nil {
		t.Fatal("drifted ActionCall authorized delivery")
	}
	state.call.Arguments["body"] = state.thread.Messages[0].Body
	if _, err := authorizer.AuthorizeOutreachInvocation(t.Context(), invocation); err != nil {
		t.Fatalf("approved exact action rejected: %v", err)
	}
}

func TestTransportInvocationAuthorizerRequiresExactTrustedHandoff(t *testing.T) {
	decision := source.OutreachPolicyDecision{PolicyID: "community", PolicyVersion: "1", SourceHost: "hooks.example.com", PathPrefix: "/replies", ApprovalPolicy: "human-review", MaximumBytes: 1000}
	invocation := runtime.ToolInvocation{Name: SkillID, DeploymentID: "researcher", SkillID: SkillID, SkillVersion: SkillVersion, Action: PostReply, ActionCallID: "action-1", RunID: "run-1",
		Arguments: map[string]interface{}{PolicyDecisionTransportKey: decision, ApprovalPolicyTransportKey: "human-review", ActionCallIDTransportKey: "action-1", RunIDTransportKey: "run-1", DeploymentIDTransportKey: "researcher"}}
	authorization, err := (TransportInvocationAuthorizer{}).AuthorizeOutreachInvocation(t.Context(), invocation)
	if err != nil || authorization.Decision.PolicyID != "community" || authorization.ApprovalPolicy != "human-review" {
		t.Fatalf("authorization=%#v err=%v", authorization, err)
	}
	invocation.Arguments[RunIDTransportKey] = "another-run"
	if _, err := (TransportInvocationAuthorizer{}).AuthorizeOutreachInvocation(t.Context(), invocation); err == nil {
		t.Fatal("drifted trusted transport identity was accepted")
	}
	invocation.Arguments[RunIDTransportKey] = "run-1"
	invocation.Arguments[PolicyDecisionTransportKey] = map[string]interface{}{"policyId": "community", "policyVersion": "1", "sourceHost": "hooks.example.com", "pathPrefix": "/replies", "approvalPolicy": "human-review", "maximumBytes": 1000, "unexpected": true}
	if _, err := (TransportInvocationAuthorizer{}).AuthorizeOutreachInvocation(t.Context(), invocation); err == nil {
		t.Fatal("unknown trusted policy field was accepted")
	}
}

func authorizedFixture(withApproval bool) (*authorizationStateStub, runtime.ToolInvocation, *source.Policy) {
	scope := runtime.Scope{Kind: "local", ID: "research"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "team-1"}
	target, body := "https://hooks.example.com/community/thread-1", "I am part of OpenSeal. Which step was hardest?"
	arguments := map[string]interface{}{"targetUri": target, "body": body}
	capability := &runtime.OutreachCapability{BindingID: "community-account", BindingRevision: 3, SkillID: SkillID, SkillVersion: SkillVersion, Action: PostReply, Arguments: arguments, TargetArgument: "targetUri", BodyArgument: "body"}
	message := runtime.OutreachMessage{ID: "message-1", Direction: runtime.OutreachMessageOutbound, Intent: runtime.OutreachIntentRequestFeedback, Body: body, Status: runtime.OutreachMessageReady, Capability: capability, RunID: "run-1", ActionCallID: "action-1"}
	call := &runtime.ActionCall{ID: "action-1", Scope: scope, RunID: "run-1", DeploymentID: "researcher", BindingID: "community-account", BindingRevision: 3,
		SkillID: SkillID, SkillVersion: SkillVersion, Action: PostReply, Status: runtime.ActionCallStatusRunning, SideEffect: skill.SideEffectExternal,
		Arguments: map[string]interface{}{"targetUri": target, "body": body}, EvidenceRefs: []string{"evidence-1"}, IdempotencyKey: "outreach:thread-1:message-1"}
	state := &authorizationStateStub{
		run: &runtime.AgentRun{ID: "run-1", Scope: scope, Owner: owner, ObjectiveID: "objective-1", AssignedAgentID: "researcher",
			Context: map[string]interface{}{runtime.OutreachInvocationContextKey: map[string]interface{}{"threadId": "thread-1", "messageId": "message-1"}},
			Policy:  map[string]interface{}{"outreachApprovalPolicyRef": "human-review"}},
		project: &runtime.Project{ID: "project-1", Scope: scope, Owner: owner},
		thread: &runtime.OutreachThread{ID: "thread-1", Scope: scope, ProjectID: "project-1", SourceObservationID: "evidence-1", TargetURI: target,
			Owner: owner, AssignedAgentID: "researcher", SourcePolicyRef: "community@1", ApprovalPolicyRef: "human-review", Status: runtime.OutreachThreadOpen, Messages: []runtime.OutreachMessage{message}},
		call: call,
	}
	if withApproval {
		state.thread.Messages[0].Status, state.thread.Messages[0].ApprovalID, state.call.ApprovalID = runtime.OutreachMessagePendingApproval, "approval-1", "approval-1"
		state.approval = &runtime.ApprovalCheckpoint{ID: "approval-1", Scope: scope, RunID: "run-1", ActionCallID: "action-1", Status: runtime.ApprovalStatusApproved}
	}
	policy := &source.Policy{ID: "community", Version: "1", Enabled: true, MaximumItems: 10, Sources: []source.PolicySource{{Host: "hooks.example.com", PathPrefixes: []string{"/community"}}},
		Outreach: &source.OutreachPolicy{Enabled: true, ApprovalPolicy: "human-review", MaximumBytes: 1000}}
	invocation := runtime.ToolInvocation{Name: SkillID, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", SkillID: SkillID, SkillVersion: SkillVersion,
		Action: PostReply, ActionCallID: "action-1", RunID: "run-1", Arguments: map[string]interface{}{"targetUri": target, "body": body}}
	return state, invocation, policy
}
