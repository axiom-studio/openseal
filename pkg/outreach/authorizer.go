package outreach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/source"
)

// PolicyResolver keeps policy storage and tenancy in the embedding host while
// making the authorization algorithm identical in standalone OpenSeal and
// enterprise runtimes.
type PolicyResolver interface {
	ResolveOutreachPolicy(context.Context, skill.ScopeReference, string) (*source.Policy, error)
}

type PolicyResolverFunc func(context.Context, skill.ScopeReference, string) (*source.Policy, error)

func (f PolicyResolverFunc) ResolveOutreachPolicy(ctx context.Context, scope skill.ScopeReference, reference string) (*source.Policy, error) {
	return f(ctx, scope, reference)
}

type AuthorizationState interface {
	GetAgentRun(context.Context, runtime.Scope, string) (*runtime.AgentRun, error)
	GetInitiative(context.Context, runtime.Scope, string) (*runtime.Initiative, error)
	GetOutreachThread(context.Context, runtime.Scope, string) (*runtime.OutreachThread, error)
	GetActionCall(context.Context, runtime.Scope, string) (*runtime.ActionCall, error)
	GetApproval(context.Context, runtime.Scope, string) (*runtime.ApprovalCheckpoint, error)
	AppendActivity(context.Context, *runtime.ActivityEvent) (*runtime.ActivityEvent, error)
	ListActivity(context.Context, runtime.ActivityFilter) ([]*runtime.ActivityEvent, error)
}

// CanonicalInvocationAuthorizer reloads every trusted fact used by an
// outreach action. Model-visible arguments never grant authority and a worker
// cannot dispatch if the reviewed thread, Run, ActionCall, approval, evidence,
// binding, or source policy has drifted.
type CanonicalInvocationAuthorizer struct {
	state    AuthorizationState
	policies PolicyResolver
}

func NewCanonicalInvocationAuthorizer(state AuthorizationState, policies PolicyResolver) (*CanonicalInvocationAuthorizer, error) {
	if state == nil || policies == nil {
		return nil, errors.New("outreach authorization state and policy resolver are required")
	}
	return &CanonicalInvocationAuthorizer{state: state, policies: policies}, nil
}

func (a *CanonicalInvocationAuthorizer) AuthorizeOutreachInvocation(ctx context.Context, invocation runtime.ToolInvocation) (*InvocationAuthorization, error) {
	if a == nil || a.state == nil || a.policies == nil {
		return nil, errors.New("outreach invocation authorizer is unavailable")
	}
	if invocation.Name != SkillID || invocation.SkillID != SkillID || invocation.SkillVersion != SkillVersion || invocation.Action != PostReply {
		return nil, errors.New("outreach invocation authorizer received an unsupported capability")
	}
	scope := runtime.Scope{Kind: invocation.Scope.Kind, ID: invocation.Scope.ID}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	run, err := a.state.GetAgentRun(ctx, scope, invocation.RunID)
	if err != nil {
		return nil, fmt.Errorf("load outreach Run: %w", err)
	}
	if run == nil {
		return nil, errors.New("load outreach Run: not found")
	}
	invocationContext, ok := run.Context[runtime.OutreachInvocationContextKey].(map[string]interface{})
	threadID, _ := invocationContext["threadId"].(string)
	messageID, _ := invocationContext["messageId"].(string)
	if !ok || len(invocationContext) != 2 || strings.TrimSpace(threadID) == "" || strings.TrimSpace(messageID) == "" || run.AssignedAgentID != invocation.DeploymentID {
		return nil, errors.New("outreach Run provenance is incomplete")
	}
	thread, err := a.state.GetOutreachThread(ctx, scope, threadID)
	if err != nil {
		return nil, fmt.Errorf("load outreach thread: %w", err)
	}
	if thread == nil {
		return nil, errors.New("load outreach thread: not found")
	}
	if thread.InitiativeID == "" || thread.Status != runtime.OutreachThreadOpen || thread.AssignedAgentID != invocation.DeploymentID || thread.Owner != run.Owner {
		return nil, errors.New("outreach thread does not belong to the active Run")
	}
	message := messageByID(thread, messageID)
	if err := validateInvocationMessage(message, thread, invocation); err != nil {
		return nil, err
	}
	call, err := a.state.GetActionCall(ctx, scope, invocation.ActionCallID)
	if err != nil {
		return nil, fmt.Errorf("load outreach ActionCall: %w", err)
	}
	if call == nil {
		return nil, errors.New("load outreach ActionCall: not found")
	}
	if err := validateInvocationAction(call, run, thread, message); err != nil {
		return nil, err
	}
	if message.Status == runtime.OutreachMessagePendingApproval {
		approval, approvalErr := a.state.GetApproval(ctx, scope, message.ApprovalID)
		if approvalErr != nil || approval == nil || approval.Status != runtime.ApprovalStatusApproved || approval.ActionCallID != call.ID || approval.RunID != run.ID {
			return nil, errors.New("outreach action has not received its required approval")
		}
	} else if message.Status != runtime.OutreachMessageReady || message.ApprovalID != "" {
		return nil, errors.New("outreach message is not ready for delivery")
	}
	policyReference, _ := run.Policy["outreachApprovalPolicyRef"].(string)
	if strings.TrimSpace(thread.SourcePolicyRef) == "" || policyReference != thread.ApprovalPolicyRef {
		return nil, errors.New("outreach approval policy drifted from the reviewed thread")
	}
	policy, err := a.policies.ResolveOutreachPolicy(ctx, invocation.Scope, thread.SourcePolicyRef)
	if err != nil {
		return nil, fmt.Errorf("resolve outreach source policy %q: %w", thread.SourcePolicyRef, err)
	}
	if policy == nil {
		return nil, fmt.Errorf("resolve outreach source policy %q: not found", thread.SourcePolicyRef)
	}
	decision, err := policy.AuthorizeOutreach(thread.TargetURI, len([]byte(message.Body)), thread.ApprovalPolicyRef)
	if err != nil {
		return nil, fmt.Errorf("source policy %s denied outreach: %w", thread.SourcePolicyRef, err)
	}
	initiative, err := a.state.GetInitiative(ctx, scope, thread.InitiativeID)
	if err != nil || initiative == nil || initiative.Owner != thread.Owner {
		return nil, errors.New("outreach Initiative ownership is unavailable or inconsistent")
	}
	if err := a.recordDecision(ctx, run, initiative, thread, message, decision, call.ID); err != nil {
		return nil, err
	}
	return &InvocationAuthorization{Decision: *decision, ApprovalPolicy: thread.ApprovalPolicyRef}, nil
}

func validateInvocationMessage(message *runtime.OutreachMessage, thread *runtime.OutreachThread, invocation runtime.ToolInvocation) error {
	if message == nil || message.Direction != runtime.OutreachMessageOutbound || message.Capability == nil ||
		message.ActionCallID != invocation.ActionCallID || message.Capability.SkillID != invocation.SkillID ||
		message.Capability.SkillVersion != invocation.SkillVersion || message.Capability.Action != invocation.Action ||
		message.Capability.Arguments[message.Capability.TargetArgument] != thread.TargetURI ||
		message.Capability.Arguments[message.Capability.BodyArgument] != message.Body ||
		invocation.Arguments[message.Capability.TargetArgument] != thread.TargetURI ||
		invocation.Arguments[message.Capability.BodyArgument] != message.Body {
		return errors.New("outreach invocation drifted from its reviewed evidence-bound message")
	}
	return nil
}

func validateInvocationAction(call *runtime.ActionCall, run *runtime.AgentRun, thread *runtime.OutreachThread, message *runtime.OutreachMessage) error {
	capability := message.Capability
	if call.Status != runtime.ActionCallStatusRunning || call.RunID != run.ID || call.DeploymentID != thread.AssignedAgentID ||
		call.SkillID != capability.SkillID || call.SkillVersion != capability.SkillVersion || call.Action != capability.Action ||
		call.BindingID != capability.BindingID || call.BindingRevision != capability.BindingRevision ||
		call.SideEffect != skill.SideEffectExternal || call.IdempotencyKey == "" ||
		!sameMap(call.Arguments, capability.Arguments) || !contains(call.EvidenceRefs, thread.SourceObservationID) ||
		call.ApprovalID != message.ApprovalID {
		return errors.New("outreach ActionCall does not match its reviewed message and active worker lease")
	}
	return nil
}

func (a *CanonicalInvocationAuthorizer) recordDecision(ctx context.Context, run *runtime.AgentRun, initiative *runtime.Initiative, thread *runtime.OutreachThread, message *runtime.OutreachMessage, decision *source.OutreachPolicyDecision, actionCallID string) error {
	eventID := "outreach-policy-" + strings.TrimSpace(actionCallID)
	existing, err := a.state.ListActivity(ctx, runtime.ActivityFilter{Scope: run.Scope, RunID: run.ID, EventTypes: []string{"outreach.policy_authorized"}, Limit: 100})
	if err != nil {
		return fmt.Errorf("list outreach policy audit: %w", err)
	}
	if activityContains(existing, eventID) {
		return nil
	}
	visibility, teamID := runtime.ActivityVisibilityScope, ""
	if initiative.Owner.Type == runtime.OwnerTypeTeam {
		visibility, teamID = runtime.ActivityVisibilityTeam, initiative.Owner.ID
	}
	event := &runtime.ActivityEvent{
		ID: eventID, Scope: run.Scope, EventType: "outreach.policy_authorized", Severity: runtime.ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, InitiativeID: initiative.ID, RunID: run.ID, TeamID: teamID,
		Actor: runtime.ActivityActor{Type: "system", ID: "outreach-policy"}, Visibility: visibility,
		Summary: "Reviewed outreach authorized by source policy",
		Payload: map[string]interface{}{
			"threadId": thread.ID, "messageId": message.ID, "actionCallId": actionCallID,
			"policyId": decision.PolicyID, "policyVersion": decision.PolicyVersion,
			"sourceHost": decision.SourceHost, "pathPrefix": decision.PathPrefix,
			"approvalPolicy": decision.ApprovalPolicy, "maximumBytes": decision.MaximumBytes,
		},
	}
	if _, err := a.state.AppendActivity(ctx, event); err == nil {
		return nil
	}
	existing, listErr := a.state.ListActivity(ctx, runtime.ActivityFilter{Scope: run.Scope, RunID: run.ID, EventTypes: []string{"outreach.policy_authorized"}, Limit: 100})
	if listErr == nil && activityContains(existing, eventID) {
		return nil
	}
	return errors.New("persist outreach policy authorization audit")
}

func messageByID(thread *runtime.OutreachThread, id string) *runtime.OutreachMessage {
	if thread == nil {
		return nil
	}
	for index := range thread.Messages {
		if thread.Messages[index].ID == id {
			return &thread.Messages[index]
		}
	}
	return nil
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func sameMap(left, right map[string]interface{}) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func activityContains(events []*runtime.ActivityEvent, id string) bool {
	for _, event := range events {
		if event != nil && event.ID == id {
			return true
		}
	}
	return false
}
