package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/axiom-studio/openseal/internal/server"
	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	kernelbundle "github.com/axiom-studio/openseal/pkg/bundle"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
	tea "github.com/charmbracelet/bubbletea"
	"go.uber.org/zap"
)

type fakeKernelClient struct {
	document             kernelapi.CapabilityDocument
	agentDocument        *kernelapi.CapabilityDocument
	agentCapabilityCalls []struct {
		scope        capability.ScopeReference
		deploymentID string
	}
	agentDeployments           []kernelapi.AgentDeploymentCatalogEntry
	agentUpdates               []kernelapi.UpdateAgentDeploymentRequest
	agentAmendments            []*kernelagent.DefinitionAmendment
	agentProposals             []kernelagent.ProposeAmendmentRequest
	agentEvaluations           []kernelagent.SubmitAmendmentEvaluationRequest
	agentDecisions             []kernelagent.ResolveAmendmentRequest
	agentActivations           []kernelapi.ActivateAgentDefinitionAmendmentRequest
	teamDeployments            []kernelapi.TeamDeploymentCatalogEntry
	teamUpdates                []kernelapi.UpdateTeamDeploymentRequest
	teamAmendments             []*kernelteam.DefinitionAmendment
	teamProposals              []kernelteam.ProposeAmendmentRequest
	teamEvaluations            []kernelteam.SubmitAmendmentEvaluationRequest
	teamDecisions              []kernelteam.ResolveAmendmentRequest
	teamActivations            []kernelapi.ActivateTeamDefinitionAmendmentRequest
	runs                       []*runtime.AgentRun
	agentRequests              []*runtime.AgentRequest
	agentRequestFilters        []runtime.AgentRequestFilter
	agentRequestCreates        []kernelapi.CreateAgentRequestRequest
	agentRequestKeys           []string
	agentResponses             []kernelapi.RespondAgentRequestRequest
	agentCompletions           []kernelapi.CompleteAgentRequestRequest
	agentCompletionKeys        []string
	actionApprovals            []*runtime.ApprovalCheckpoint
	approvalFilters            []runtime.ApprovalFilter
	agentTurns                 []*kernelapi.AgentTurnRecord
	agentTurnFilters           []runtime.AgentTurnFilter
	actionCalls                []*runtime.ActionCall
	actionCallFilters          []runtime.ActionFilter
	actionDecisions            []kernelapi.ResolveActionApprovalRequest
	actionDecisionKeys         []string
	compilations               []*kernelagent.DefinitionCompilation
	compilationErr             error
	createErrors               []error
	createKeys                 []string
	createRequests             []kernelapi.CreateAgentRunRequest
	objectives                 []*runtime.Objective
	runbooks                   []*runtime.RunbookActivation
	runbookDefinitions         map[string]*runbook.Definition
	runbookVerifications       map[string]*runbook.VerificationReport
	runbookUpdates             []runtime.UpdateRunbookActivationRequest
	runbookStarts              []runtime.StartRunbookActivationRequest
	runbookStartResult         *runtime.AgentRunCommandResult
	objectiveKeys              []string
	objectiveCreates           []kernelapi.CreateObjectiveRequest
	objectiveUpdates           []kernelapi.UpdateObjectiveRequest
	scheduleRequests           []kernelapi.ReconcileRunbookSchedulesRequest
	scheduleResult             *kernelapi.RunbookScheduleReconciliation
	activityPages              map[string]*runtime.ActivityFeedPage
	activityRequests           []runtime.ActivityFeedRequest
	commands                   []kernelapi.AgentRunCommandRequest
	artifacts                  []*runtime.Artifact
	downloadBody               string
	downloadCalls              int
	skillActions               []capability.ModelAction
	skillActionCalls           int
	skillBindings              []*capability.Binding
	skillBindingUpserts        []skill.UpsertBindingRequest
	skillBindingDisables       []skill.DisableBindingRequest
	skillUpgradePlans          []runtime.PlanSkillReferenceUpgradeRequest
	skillUpgradeApplies        []runtime.ApplySkillReferenceUpgradeRequest
	skillUpgradePlan           *runtime.SkillReferenceUpgradePlan
	skillBindingError          error
	conversationGateways       []*runtime.ExternalConversationGatewayRegistration
	conversationGatewayCreates []kernelapi.CreateExternalConversationGatewayRequest
	conversationGatewayUpdates []kernelapi.UpdateExternalConversationGatewayRequest
}

type fakeWorkforceBundleKernelClient struct {
	*fakeKernelClient
	inspected int
}

func (f *fakeWorkforceBundleKernelClient) ValidateWorkforceBundle(context.Context, *kernelbundle.Bundle) error {
	return nil
}

func (f *fakeWorkforceBundleKernelClient) InspectWorkforceBundle(_ context.Context, artifact *kernelbundle.Bundle) (*kernelbundle.Inspection, error) {
	f.inspected++
	return kernelbundle.Inspect(artifact)
}

func (f *fakeWorkforceBundleKernelClient) CompareWorkforceBundles(context.Context, *kernelbundle.Bundle, *kernelbundle.Bundle) (*kernelbundle.Diff, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeWorkforceBundleKernelClient) PreviewWorkforceBundleInstallation(context.Context, *kernelbundle.Bundle, kernelbundle.Placement) (*kernelbundle.InstallationPreview, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeWorkforceBundleKernelClient) PlanWorkforceBundleUpgrade(context.Context, *kernelbundle.Bundle, *kernelbundle.Bundle) (*kernelbundle.UpgradePlan, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeWorkforceBundleKernelClient) InstallWorkforceBundle(context.Context, *kernelbundle.Bundle, capability.ScopeReference, kernelbundle.Placement, string, string) (*kernelbundle.InstallationReceipt, error) {
	return nil, errors.New("not implemented by test client")
}

var (
	_ client.KernelClient                      = (*fakeKernelClient)(nil)
	_ client.AgentDefinitionCapabilityClient   = (*fakeKernelClient)(nil)
	_ client.AgentDefinitionLifecycleClient    = (*fakeKernelClient)(nil)
	_ client.ExternalConversationGatewayClient = (*fakeKernelClient)(nil)
	_ client.KernelClient                      = (*fakeChannelKernelClient)(nil)
	_ client.KernelClient                      = (*fakeClawHubKernelClient)(nil)
	_ client.SkillBindingClient                = (*fakeKernelClient)(nil)
)

func (f *fakeKernelClient) InstallAgentManifest(context.Context, kernelagent.ManifestInstallationRequest, string) (*kernelagent.ManifestInstallationResult, error) {
	return nil, errors.New("agent manifest installation is not configured in this TUI test")
}

func (f *fakeKernelClient) CreateExternalConversationGateway(_ context.Context, request kernelapi.CreateExternalConversationGatewayRequest) (*runtime.ExternalConversationGatewayRegistration, error) {
	f.conversationGatewayCreates = append(f.conversationGatewayCreates, request)
	now := time.Now().UTC()
	value := &runtime.ExternalConversationGatewayRegistration{
		ID: "gateway-created", IngressRoute: "route-created", Name: request.Name, Gateway: request.Gateway,
		Status: runtime.ExternalConversationGatewayPaused, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	f.conversationGateways = append(f.conversationGateways, value)
	return value, nil
}

func (f *fakeKernelClient) ListExternalConversationGateways(context.Context, runtime.ExternalConversationGatewayFilter) ([]*runtime.ExternalConversationGatewayRegistration, error) {
	return f.conversationGateways, nil
}

func (f *fakeKernelClient) GetExternalConversationGateway(_ context.Context, _ runtime.Scope, id string) (*runtime.ExternalConversationGatewayRegistration, error) {
	for _, item := range f.conversationGateways {
		if item.ID == id {
			return item, nil
		}
	}
	return nil, runtime.ErrExternalConversationGatewayNotFound
}

func (f *fakeKernelClient) UpdateExternalConversationGateway(_ context.Context, _ runtime.Scope, id string, request kernelapi.UpdateExternalConversationGatewayRequest) (*runtime.ExternalConversationGatewayRegistration, error) {
	f.conversationGatewayUpdates = append(f.conversationGatewayUpdates, request)
	for _, item := range f.conversationGateways {
		if item.ID == id {
			if request.ExpectedRevision != item.Revision {
				return nil, runtime.ErrExternalConversationConflict
			}
			if request.Status != nil {
				item.Status = *request.Status
			}
			item.Revision++
			item.UpdatedAt = time.Now().UTC()
			return item, nil
		}
	}
	return nil, runtime.ErrExternalConversationGatewayNotFound
}

func (f *fakeKernelClient) ListSkillBindings(context.Context, capability.ScopeReference, client.SkillBindingOwner) (*kernelapi.SkillBindingList, error) {
	return &kernelapi.SkillBindingList{Items: f.skillBindings}, nil
}
func (f *fakeKernelClient) UpsertSkillBinding(_ context.Context, _ client.SkillBindingOwner, request skill.UpsertBindingRequest) (*kernelapi.SkillBindingMutationResult, error) {
	f.skillBindingUpserts = append(f.skillBindingUpserts, request)
	if f.skillBindingError != nil {
		return nil, f.skillBindingError
	}
	value := *request.Binding
	value.Revision = request.ExpectedRevision + 1
	f.skillBindings = []*capability.Binding{&value}
	return &kernelapi.SkillBindingMutationResult{Binding: &value}, nil
}
func (f *fakeKernelClient) DisableSkillBinding(_ context.Context, _ client.SkillBindingOwner, request skill.DisableBindingRequest) (*kernelapi.SkillBindingMutationResult, error) {
	f.skillBindingDisables = append(f.skillBindingDisables, request)
	if f.skillBindingError != nil {
		return nil, f.skillBindingError
	}
	value := *f.skillBindings[0]
	value.Disabled = true
	value.Revision++
	return &kernelapi.SkillBindingMutationResult{Binding: &value}, nil
}
func (f *fakeKernelClient) PlanSkillReferenceUpgrade(_ context.Context, _ client.SkillBindingOwner, request runtime.PlanSkillReferenceUpgradeRequest) (*runtime.SkillReferenceUpgradePlan, error) {
	f.skillUpgradePlans = append(f.skillUpgradePlans, request)
	if f.skillBindingError != nil {
		return nil, f.skillBindingError
	}
	if f.skillUpgradePlan != nil {
		return f.skillUpgradePlan, nil
	}
	return &runtime.SkillReferenceUpgradePlan{
		APIVersion: runtime.SkillReferenceUpgradeAPIVersion, Scope: request.Scope, DeploymentID: request.DeploymentID,
		BindingID: request.BindingID, ExpectedBindingRevision: 1,
		From:   runtime.SkillReferenceIdentity{ID: "reader", Version: "1"},
		To:     runtime.SkillReferenceIdentity{ID: "reader", Version: request.ToVersion, SourceIdentity: request.ToSourceIdentity},
		Digest: "sha256:reviewed",
	}, nil
}
func (f *fakeKernelClient) ApplySkillReferenceUpgrade(_ context.Context, _ client.SkillBindingOwner, request runtime.ApplySkillReferenceUpgradeRequest) (*runtime.SkillReferenceUpgradeReceipt, error) {
	f.skillUpgradeApplies = append(f.skillUpgradeApplies, request)
	if f.skillBindingError != nil {
		return nil, f.skillBindingError
	}
	value := *f.skillBindings[0]
	value.SkillVersion = request.Plan.To.Version
	value.SourceIdentity = request.Plan.To.SourceIdentity
	value.Revision++
	f.skillBindings = []*capability.Binding{&value}
	return &runtime.SkillReferenceUpgradeReceipt{
		APIVersion: runtime.SkillReferenceUpgradeAPIVersion, PlanDigest: request.Plan.Digest,
		Scope: request.Plan.Scope, DeploymentID: request.Plan.DeploymentID, BindingID: request.Plan.BindingID,
		BindingRevision: value.Revision, From: request.Plan.From, To: request.Plan.To, Actor: request.Actor, Reason: request.Reason,
	}, nil
}

func (f *fakeKernelClient) ReconcileRunbookSchedules(_ context.Context, request kernelapi.ReconcileRunbookSchedulesRequest) (*kernelapi.RunbookScheduleReconciliation, error) {
	f.scheduleRequests = append(f.scheduleRequests, request)
	if f.scheduleResult != nil {
		return f.scheduleResult, nil
	}
	return &kernelapi.RunbookScheduleReconciliation{Scope: request.Scope, Result: &runtime.RunbookScheduleResult{}}, nil
}

var _ client.ConversationClient = (*fakeChannelKernelClient)(nil)

type fakeChannelKernelClient struct {
	*fakeKernelClient
	conversations              []*runtime.Conversation
	messages                   []*runtime.ChannelMessage
	rounds                     []*runtime.ParticipationRoundResult
	presence                   []*runtime.ConversationPresence
	createConversationErrors   []error
	createConversationKeys     []string
	createConversationRequests []kernelapi.CreateConversationRequest
	postErrors                 []error
	postKeys                   []string
	postRequests               []kernelapi.PostChannelMessageRequest
	cursor                     *runtime.ConversationCursor
	cursorAdvances             []kernelapi.AdvanceConversationCursorRequest
}

type fakeClawHubKernelClient struct {
	*fakeKernelClient
	states     []clawhub.InstalledState
	installs   []clawhub.SkillReference
	pins       []string
	updates    []string
	uninstalls []string
	updateAll  int
}

func (f *fakeClawHubKernelClient) InspectClawHubSkill(context.Context, clawhub.SkillReference) (*clawhub.SkillDetail, error) {
	return &clawhub.SkillDetail{}, nil
}
func (f *fakeClawHubKernelClient) ListClawHubSkillVersions(context.Context, clawhub.SkillReference, int, string) (*clawhub.VersionPage, error) {
	return &clawhub.VersionPage{}, nil
}
func (f *fakeClawHubKernelClient) VerifyClawHubSkill(context.Context, clawhub.SkillReference, kernelapi.ClawHubVersionRequest) (*clawhub.Verification, error) {
	return &clawhub.Verification{OK: true}, nil
}
func (f *fakeClawHubKernelClient) ListInstalledClawHubSkills(context.Context) ([]clawhub.InstalledState, error) {
	return f.states, nil
}
func (f *fakeClawHubKernelClient) InstallClawHubSkill(_ context.Context, reference clawhub.SkillReference, _ kernelapi.ClawHubVersionRequest) (*clawhub.LifecycleResult, error) {
	f.installs = append(f.installs, reference)
	return &clawhub.LifecycleResult{APIVersion: clawhub.LifecycleAPIVersion, Operation: clawhub.LifecycleInstall, SourceIdentity: "source", Reference: reference, Version: "1.0.0", Outcome: clawhub.LifecycleOutcomeInstalled, Changed: true}, nil
}
func (f *fakeClawHubKernelClient) VerifyInstalledClawHubSkill(context.Context, string) (*clawhub.Verification, error) {
	return &clawhub.Verification{OK: true}, nil
}
func (f *fakeClawHubKernelClient) PinClawHubSkill(_ context.Context, reference, reason string) (*clawhub.LifecycleResult, error) {
	f.pins = append(f.pins, reason)
	return f.result(reference, clawhub.LifecyclePin, clawhub.LifecycleOutcomePinned), nil
}
func (f *fakeClawHubKernelClient) UnpinClawHubSkill(_ context.Context, reference string) (*clawhub.LifecycleResult, error) {
	return f.result(reference, clawhub.LifecycleUnpin, clawhub.LifecycleOutcomeUnpinned), nil
}
func (f *fakeClawHubKernelClient) UpdateClawHubSkill(_ context.Context, reference string) (*clawhub.LifecycleResult, error) {
	f.updates = append(f.updates, reference)
	return f.result(reference, clawhub.LifecycleUpdate, clawhub.LifecycleOutcomeUpdated), nil
}
func (f *fakeClawHubKernelClient) UpdateAllClawHubSkills(context.Context) (*clawhub.LifecycleBatchResult, error) {
	f.updateAll++
	return &clawhub.LifecycleBatchResult{APIVersion: clawhub.LifecycleAPIVersion, Operation: clawhub.LifecycleUpdateAll}, nil
}
func (f *fakeClawHubKernelClient) UninstallClawHubSkill(_ context.Context, reference string) (*clawhub.LifecycleResult, error) {
	f.uninstalls = append(f.uninstalls, reference)
	return f.result(reference, clawhub.LifecycleUninstall, clawhub.LifecycleOutcomeRemoved), nil
}
func (f *fakeClawHubKernelClient) result(reference string, operation clawhub.LifecycleOperation, outcome clawhub.LifecycleOutcome) *clawhub.LifecycleResult {
	ref, _ := clawhub.ParseSkillReference(reference)
	return &clawhub.LifecycleResult{APIVersion: clawhub.LifecycleAPIVersion, Operation: operation, SourceIdentity: "source", Reference: ref, Version: "1.0.0", Outcome: outcome, Changed: true}
}

func (f *fakeChannelKernelClient) CreateConversation(_ context.Context, request kernelapi.CreateConversationRequest, key string) (*runtime.Conversation, error) {
	f.createConversationKeys = append(f.createConversationKeys, key)
	f.createConversationRequests = append(f.createConversationRequests, request)
	if len(f.createConversationErrors) > 0 {
		err := f.createConversationErrors[0]
		f.createConversationErrors = f.createConversationErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	conversation := testConversation("created-channel", request.Title, 0, 1)
	f.conversations = append([]*runtime.Conversation{conversation}, f.conversations...)
	return conversation, nil
}

func (f *fakeChannelKernelClient) ListConversations(context.Context, runtime.ConversationFilter) ([]*runtime.Conversation, error) {
	return f.conversations, nil
}

func (f *fakeChannelKernelClient) GetConversation(_ context.Context, _ runtime.Scope, id string) (*runtime.Conversation, error) {
	for _, conversation := range f.conversations {
		if conversation.ID == id {
			return conversation, nil
		}
	}
	return nil, runtime.ErrConversationNotFound
}

func (f *fakeChannelKernelClient) UpdateConversation(context.Context, string, kernelapi.UpdateConversationRequest) (*runtime.Conversation, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeChannelKernelClient) PostChannelMessage(_ context.Context, conversationID string, request kernelapi.PostChannelMessageRequest, key string) (*runtime.ChannelMessageCommitResult, error) {
	f.postKeys = append(f.postKeys, key)
	f.postRequests = append(f.postRequests, request)
	if len(f.postErrors) > 0 {
		err := f.postErrors[0]
		f.postErrors = f.postErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	conversation, err := f.GetConversation(context.Background(), request.Scope, conversationID)
	if err != nil {
		return nil, err
	}
	conversation.Revision++
	conversation.LastSequence++
	message := &runtime.ChannelMessage{
		ID: "posted-message", Scope: request.Scope, ConversationID: conversationID,
		Sequence: conversation.LastSequence, Sender: request.Sender, Intent: request.Intent,
		Content: request.Content, Audience: request.Audience, RequiresResponse: request.RequiresResponse,
		CreatedAt: time.Now(),
	}
	f.messages = append([]*runtime.ChannelMessage{message}, f.messages...)
	return &runtime.ChannelMessageCommitResult{Conversation: conversation, Message: message}, nil
}

func (f *fakeChannelKernelClient) ListChannelMessages(context.Context, runtime.ChannelMessageFilter) ([]*runtime.ChannelMessage, error) {
	return f.messages, nil
}

func (f *fakeChannelKernelClient) GetChannelMessage(context.Context, runtime.Scope, string, string) (*runtime.ChannelMessage, error) {
	return nil, runtime.ErrChannelMessageNotFound
}

func (f *fakeChannelKernelClient) GetParticipationRound(context.Context, runtime.Scope, string, string) (*runtime.ParticipationRoundResult, error) {
	return nil, runtime.ErrParticipationRoundNotFound
}

func (f *fakeChannelKernelClient) ListParticipationRounds(context.Context, runtime.ParticipationRoundFilter) ([]*runtime.ParticipationRoundResult, error) {
	return f.rounds, nil
}

func (f *fakeChannelKernelClient) AdvanceConversationCursor(_ context.Context, conversationID string, request kernelapi.AdvanceConversationCursorRequest) (*runtime.ConversationCursor, bool, error) {
	f.cursorAdvances = append(f.cursorAdvances, request)
	f.cursor = &runtime.ConversationCursor{
		Scope: request.Scope, ConversationID: conversationID, Participant: request.Participant,
		DeliveredSequence: request.DeliveredSequence, ReadSequence: request.ReadSequence,
		Revision: request.ExpectedRevision + 1, UpdatedAt: time.Now(),
	}
	return f.cursor, false, nil
}

func (f *fakeChannelKernelClient) GetConversationCursor(context.Context, runtime.Scope, string, runtime.ConversationParticipant) (*runtime.ConversationCursor, error) {
	if f.cursor == nil {
		return nil, &client.APIError{StatusCode: 404, Message: "conversation cursor not found"}
	}
	return f.cursor, nil
}

func (f *fakeChannelKernelClient) SetConversationPresence(context.Context, string, kernelapi.SetConversationPresenceRequest) (*runtime.ConversationPresence, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeChannelKernelClient) ReleaseConversationPresence(context.Context, string, kernelapi.ReleaseConversationPresenceRequest) error {
	return errors.New("not implemented by test client")
}

func (f *fakeChannelKernelClient) ListConversationPresence(context.Context, runtime.Scope, string) ([]*runtime.ConversationPresence, error) {
	return f.presence, nil
}

func (f *fakeKernelClient) Capabilities(context.Context) (kernelapi.CapabilityDocument, error) {
	return f.document, nil
}

func (f *fakeKernelClient) AgentDefinitionCapabilities(_ context.Context, scope capability.ScopeReference, deploymentID string) (kernelapi.CapabilityDocument, error) {
	f.agentCapabilityCalls = append(f.agentCapabilityCalls, struct {
		scope        capability.ScopeReference
		deploymentID string
	}{scope: scope, deploymentID: deploymentID})
	if f.agentDocument != nil {
		return *f.agentDocument, nil
	}
	return f.document, nil
}

func (f *fakeKernelClient) ListAgentSkillActions(context.Context, capability.ScopeReference, string, []string, capability.SideEffect) (*kernelapi.SkillActionList, error) {
	f.skillActionCalls++
	return &kernelapi.SkillActionList{DeploymentID: "researcher", Actions: f.skillActions}, nil
}

func (f *fakeKernelClient) CreateAgentRequest(_ context.Context, request kernelapi.CreateAgentRequestRequest, key string) (*runtime.AgentRequestResult, error) {
	f.agentRequestCreates = append(f.agentRequestCreates, request)
	f.agentRequestKeys = append(f.agentRequestKeys, key)
	created := &runtime.AgentRequest{
		ID: "created-request", Scope: request.Scope, Kind: request.Kind, Status: runtime.AgentRequestStatusPending,
		Requester: request.Requester, Recipient: request.Recipient, SourceRunID: request.SourceRunID, Goal: request.Goal,
		IdempotencyKey: key, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	f.agentRequests = append([]*runtime.AgentRequest{created}, f.agentRequests...)
	return &runtime.AgentRequestResult{Request: created}, nil
}

func (f *fakeKernelClient) ListAgentRequests(_ context.Context, filter runtime.AgentRequestFilter) ([]*runtime.AgentRequest, error) {
	f.agentRequestFilters = append(f.agentRequestFilters, filter)
	result := make([]*runtime.AgentRequest, 0, len(f.agentRequests))
	for _, request := range f.agentRequests {
		if filter.Requester != nil && request.Requester != *filter.Requester {
			continue
		}
		if filter.Recipient != nil && request.Recipient != *filter.Recipient {
			continue
		}
		result = append(result, request)
	}
	return result, nil
}

func (f *fakeKernelClient) GetAgentRequest(_ context.Context, _ runtime.Scope, id string) (*runtime.AgentRequest, error) {
	for _, request := range f.agentRequests {
		if request.ID == id {
			return request, nil
		}
	}
	return nil, runtime.ErrAgentRequestNotFound
}

func (f *fakeKernelClient) RespondAgentRequest(_ context.Context, _ runtime.Scope, id string, response kernelapi.RespondAgentRequestRequest) (*runtime.AgentRequestResult, error) {
	f.agentResponses = append(f.agentResponses, response)
	request, err := f.GetAgentRequest(context.Background(), runtime.Scope{}, id)
	if err != nil {
		return nil, err
	}
	request.Revision++
	request.Response = response.Message
	switch response.Decision {
	case runtime.AgentRequestDecisionAccept:
		request.Status = runtime.AgentRequestStatusAccepted
		request.AssignedAgentID = response.AssignedAgentID
		request.ChildRunID = "child-run"
	case runtime.AgentRequestDecisionReject:
		request.Status = runtime.AgentRequestStatusRejected
	case runtime.AgentRequestDecisionRequestClarification:
		request.Status = runtime.AgentRequestStatusClarificationRequested
		request.Clarification = response.Message
	case runtime.AgentRequestDecisionProvideClarification:
		request.Status = runtime.AgentRequestStatusPending
	}
	return &runtime.AgentRequestResult{Request: request}, nil
}

func (f *fakeKernelClient) CompleteAgentRequest(_ context.Context, _ runtime.Scope, id string, completion kernelapi.CompleteAgentRequestRequest, key string) (*runtime.AgentRequestResult, error) {
	f.agentCompletions = append(f.agentCompletions, completion)
	f.agentCompletionKeys = append(f.agentCompletionKeys, key)
	request, err := f.GetAgentRequest(context.Background(), runtime.Scope{}, id)
	if err != nil {
		return nil, err
	}
	request.Revision++
	request.Status = runtime.AgentRequestStatusCompleted
	request.CompletionSummary = completion.Summary
	return &runtime.AgentRequestResult{Request: request}, nil
}

func (f *fakeKernelClient) ReviewAgentRequestCompletion(_ context.Context, _ runtime.Scope, id string, review kernelapi.ReviewAgentRequestCompletionRequest, _ string) (*runtime.AgentRequestResult, error) {
	request, err := f.GetAgentRequest(context.Background(), runtime.Scope{}, id)
	if err != nil {
		return nil, err
	}
	request.Revision++
	request.CompletionReviewSummary = review.Summary
	request.Status = runtime.AgentRequestStatusFailed
	if review.Approve {
		request.Status = runtime.AgentRequestStatusCompleted
	}
	return &runtime.AgentRequestResult{Request: request}, nil
}

func (f *fakeKernelClient) ListActionCalls(_ context.Context, filter runtime.ActionFilter) ([]*runtime.ActionCall, error) {
	f.actionCallFilters = append(f.actionCallFilters, filter)
	calls := make([]*runtime.ActionCall, 0, len(f.actionCalls))
	for _, call := range f.actionCalls {
		if call != nil && (filter.RunID == "" || call.RunID == filter.RunID) {
			calls = append(calls, call)
		}
	}
	return calls, nil
}

func (f *fakeKernelClient) GetActionCall(context.Context, runtime.Scope, string) (*runtime.ActionCall, error) {
	return nil, runtime.ErrActionNotFound
}

func (f *fakeKernelClient) ListAgentTurns(_ context.Context, filter runtime.AgentTurnFilter) ([]*kernelapi.AgentTurnRecord, error) {
	f.agentTurnFilters = append(f.agentTurnFilters, filter)
	turns := make([]*kernelapi.AgentTurnRecord, 0, len(f.agentTurns))
	for _, turn := range f.agentTurns {
		if turn != nil && (filter.RunID == "" || turn.RunID == filter.RunID) {
			turns = append(turns, turn)
		}
	}
	return turns, nil
}

func (f *fakeKernelClient) GetAgentTurn(context.Context, runtime.Scope, string) (*kernelapi.AgentTurnRecord, error) {
	return nil, runtime.ErrTurnNotFound
}

func (f *fakeKernelClient) ListActionApprovals(_ context.Context, filter runtime.ApprovalFilter) ([]*runtime.ApprovalCheckpoint, error) {
	f.approvalFilters = append(f.approvalFilters, filter)
	return f.actionApprovals, nil
}

func (f *fakeKernelClient) GetActionApproval(_ context.Context, _ runtime.Scope, id string) (*runtime.ApprovalCheckpoint, error) {
	for _, approval := range f.actionApprovals {
		if approval.ID == id {
			return approval, nil
		}
	}
	return nil, runtime.ErrApprovalNotFound
}

func (f *fakeKernelClient) ResolveActionApproval(_ context.Context, _ runtime.Scope, id string, decision kernelapi.ResolveActionApprovalRequest, key string) (*runtime.ApprovalResolutionResult, error) {
	f.actionDecisions = append(f.actionDecisions, decision)
	f.actionDecisionKeys = append(f.actionDecisionKeys, key)
	approval, err := f.GetActionApproval(context.Background(), runtime.Scope{}, id)
	if err != nil {
		return nil, err
	}
	approval.Revision++
	approval.DecisionReason = decision.Reason
	approval.DecisionBy = &decision.Principal
	approval.Status = runtime.ApprovalStatusRejected
	if decision.Decision == runtime.ApprovalDecisionApprove {
		approval.Status = runtime.ApprovalStatusApproved
	} else if decision.Decision == runtime.ApprovalDecisionRequestChanges {
		approval.Status = runtime.ApprovalStatusChangesRequested
	}
	return &runtime.ApprovalResolutionResult{Approval: approval, Resolved: true}, nil
}

func (f *fakeKernelClient) CreateObjective(_ context.Context, request kernelapi.CreateObjectiveRequest, key string) (*runtime.Objective, error) {
	f.objectiveKeys = append(f.objectiveKeys, key)
	f.objectiveCreates = append(f.objectiveCreates, request)
	objective := &runtime.Objective{
		ID: "objective-created", Scope: request.Scope, Owner: request.Owner, Title: request.Title, Goal: request.Goal,
		Status: request.Status, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	f.objectives = append([]*runtime.Objective{objective}, f.objectives...)
	return objective, nil
}

func (f *fakeKernelClient) ListObjectives(context.Context, runtime.ObjectiveFilter) ([]*runtime.Objective, error) {
	return f.objectives, nil
}

func (f *fakeKernelClient) GetObjective(_ context.Context, _ runtime.Scope, id string) (*kernelapi.ObjectiveDetail, error) {
	for _, objective := range f.objectives {
		if objective.ID == id {
			return &kernelapi.ObjectiveDetail{Objective: objective, Runbooks: f.runbooks, Runs: f.runs}, nil
		}
	}
	return nil, runtime.ErrObjectiveNotFound
}

func (f *fakeKernelClient) ListRunbooks(context.Context, runtime.RunbookActivationFilter) ([]*runtime.RunbookActivation, error) {
	return f.runbooks, nil
}

func (f *fakeKernelClient) GetRunbook(_ context.Context, _ runtime.Scope, id string) (*runtime.RunbookDetail, error) {
	for _, value := range f.runbooks {
		if value.ID == id {
			return &runtime.RunbookDetail{Activation: value, Definition: f.runbookDefinitions[id], Verification: f.runbookVerifications[id]}, nil
		}
	}
	return nil, runtime.ErrRunbookActivationNotFound
}

func (f *fakeKernelClient) UpdateRunbook(_ context.Context, _ runtime.Scope, id string, request runtime.UpdateRunbookActivationRequest) (*runtime.RunbookActivation, error) {
	f.runbookUpdates = append(f.runbookUpdates, request)
	for _, value := range f.runbooks {
		if value.ID == id {
			value.Status = request.Status
			value.Revision++
			return value, nil
		}
	}
	return nil, runtime.ErrRunbookActivationNotFound
}

func (f *fakeKernelClient) StartRunbook(_ context.Context, _ runtime.Scope, _ string, request runtime.StartRunbookActivationRequest) (*runtime.AgentRunCommandResult, error) {
	f.runbookStarts = append(f.runbookStarts, request)
	if f.runbookStartResult != nil {
		return f.runbookStartResult, nil
	}
	return nil, errors.New("starting Runbooks is not configured in this TUI test")
}

func (f *fakeKernelClient) UpdateObjective(_ context.Context, _ runtime.Scope, id string, request kernelapi.UpdateObjectiveRequest) (*runtime.Objective, error) {
	f.objectiveUpdates = append(f.objectiveUpdates, request)
	for _, objective := range f.objectives {
		if objective.ID == id {
			if request.Goal != nil {
				objective.Goal = *request.Goal
			}
			objective.Revision++
			return objective, nil
		}
	}
	return nil, runtime.ErrObjectiveNotFound
}

func (f *fakeKernelClient) ListActivity(_ context.Context, request runtime.ActivityFeedRequest) (*runtime.ActivityFeedPage, error) {
	f.activityRequests = append(f.activityRequests, request)
	keys := []string{activityTestRequestKey(request), request.RunID, request.AgentID, request.TeamID}
	for _, key := range keys {
		if page := f.activityPages[key]; page != nil {
			return page, nil
		}
	}
	if page := f.activityPages[request.RunID]; page != nil {
		return page, nil
	}
	return &runtime.ActivityFeedPage{}, nil
}

func activityTestRequestKey(request runtime.ActivityFeedRequest) string {
	selector := "agent:" + request.AgentID
	if request.TeamID != "" {
		selector = "team:" + request.TeamID
	} else if request.ObjectiveID != "" {
		selector = "objective:" + request.ObjectiveID
	} else if request.RunID != "" {
		selector = "run:" + request.RunID
	}
	if request.Cursor != "" {
		selector += "|cursor:" + request.Cursor
	}
	if request.IncludeDetails {
		selector += "|details"
	}
	return selector
}
func (f *fakeKernelClient) CreateAgentRun(_ context.Context, request kernelapi.CreateAgentRunRequest, key string) (*runtime.AgentRunCommandResult, error) {
	f.createKeys = append(f.createKeys, key)
	f.createRequests = append(f.createRequests, request)
	if len(f.createErrors) > 0 {
		err := f.createErrors[0]
		f.createErrors = f.createErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	run := testRun("created", runtime.AgentRunStatusQueued, 1)
	f.runs = append([]*runtime.AgentRun{run}, f.runs...)
	return &runtime.AgentRunCommandResult{Run: run}, nil
}

func (f *fakeKernelClient) ListAgentRuns(context.Context, runtime.AgentRunFilter) ([]*runtime.AgentRun, error) {
	return f.runs, nil
}

func (f *fakeKernelClient) ListAgentDefinitionCompilations(context.Context, capability.ScopeReference, string) ([]*kernelagent.DefinitionCompilation, error) {
	return f.compilations, f.compilationErr
}

func (f *fakeKernelClient) GetAgentDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelapi.AgentDeploymentCatalogEntry, error) {
	if f.compilationErr != nil {
		return nil, f.compilationErr
	}
	for i := range f.agentDeployments {
		entry := &f.agentDeployments[i]
		if entry.Deployment != nil && entry.Deployment.ID == id && entry.Deployment.Scope == scope {
			return entry, nil
		}
	}
	return nil, kernelagent.ErrDeploymentNotFound
}

func (f *fakeKernelClient) ListAgentDeployments(_ context.Context, filter kernelagent.AgentDeploymentFilter) (*kernelapi.AgentDeploymentList, error) {
	items := make([]kernelapi.AgentDeploymentCatalogEntry, 0, len(f.agentDeployments))
	for _, entry := range f.agentDeployments {
		if entry.Deployment != nil && entry.Deployment.Scope == filter.Scope && !slices.Contains(filter.ExcludeStatuses, entry.Deployment.RolloutStatus) {
			items = append(items, entry)
		}
	}
	return &kernelapi.AgentDeploymentList{Items: items}, nil
}

func (f *fakeKernelClient) UpdateAgentDeployment(_ context.Context, id string, request kernelapi.UpdateAgentDeploymentRequest) (*kernelapi.AgentDeploymentUpdateResult, error) {
	f.agentUpdates = append(f.agentUpdates, request)
	for index := range f.agentDeployments {
		entry := &f.agentDeployments[index]
		if entry.Deployment == nil || entry.Deployment.ID != id {
			continue
		}
		updated := *request.Deployment
		updated.Revision++
		entry.Deployment = &updated
		return &kernelapi.AgentDeploymentUpdateResult{Deployment: &updated, Audit: &workforce.DefinitionActivation{
			DeploymentID: id, DefinitionID: updated.DefinitionID, ToVersion: updated.ActiveVersion,
			DeploymentRevision: updated.Revision, ChangeKind: workforce.DeploymentChangeConfigurationUpdated,
		}}, nil
	}
	return nil, kernelagent.ErrDeploymentNotFound
}

func (f *fakeKernelClient) ActivateAgentDefinition(context.Context, string, kernelapi.ActivateAgentDefinitionRequest) (*kernelapi.AgentDefinitionActivationResult, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) RollbackAgentDefinition(context.Context, string, kernelapi.RollbackAgentDefinitionRequest) (*kernelapi.AgentDefinitionActivationResult, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) ListAgentDefinitionActivations(context.Context, capability.ScopeReference, string) ([]workforce.DefinitionActivation, error) {
	return nil, nil
}

func (f *fakeKernelClient) ProposeAgentDefinitionAmendment(_ context.Context, request kernelagent.ProposeAmendmentRequest) (*kernelagent.DefinitionAmendment, error) {
	f.agentProposals = append(f.agentProposals, request)
	amendment := &kernelagent.DefinitionAmendment{ID: "agent-amendment-new", Scope: request.Scope, DeploymentID: request.DeploymentID, DefinitionID: request.Candidate.ID, BaseVersion: "1", BaseDigest: "sha256:base", Candidate: *request.Candidate, Changes: []workforce.DefinitionFieldChange{{Field: "systemPrompt"}}, ProposerType: request.ProposerType, ProposerID: request.ProposerID, Rationale: request.Rationale, Status: kernelagent.AmendmentReady, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.agentAmendments = append([]*kernelagent.DefinitionAmendment{amendment}, f.agentAmendments...)
	return amendment, nil
}

func (f *fakeKernelClient) GetAgentDefinitionAmendment(_ context.Context, scope capability.ScopeReference, deploymentID, amendmentID string) (*kernelagent.DefinitionAmendment, error) {
	for _, amendment := range f.agentAmendments {
		if amendment.Scope == scope && amendment.DeploymentID == deploymentID && amendment.ID == amendmentID {
			return amendment, nil
		}
	}
	return nil, kernelagent.ErrAmendmentNotFound
}

func (f *fakeKernelClient) ListAgentDefinitionAmendments(_ context.Context, scope capability.ScopeReference, deploymentID string) (*kernelapi.AgentDefinitionAmendmentList, error) {
	result := &kernelapi.AgentDefinitionAmendmentList{}
	for _, amendment := range f.agentAmendments {
		if amendment.Scope == scope && amendment.DeploymentID == deploymentID {
			result.Items = append(result.Items, amendment)
		}
	}
	return result, nil
}

func (f *fakeKernelClient) SubmitAgentDefinitionAmendmentEvaluation(_ context.Context, _ string, request kernelagent.SubmitAmendmentEvaluationRequest) (*kernelagent.DefinitionAmendment, error) {
	f.agentEvaluations = append(f.agentEvaluations, request)
	amendment, err := f.GetAgentDefinitionAmendment(context.Background(), request.Scope, f.agentAmendments[0].DeploymentID, request.AmendmentID)
	if err != nil {
		return nil, err
	}
	amendment.Evaluations, amendment.Status, amendment.Revision = request.Evaluations, kernelagent.AmendmentAwaitingApproval, request.ExpectedRevision+1
	return amendment, nil
}

func (f *fakeKernelClient) ResolveAgentDefinitionAmendment(_ context.Context, _ string, request kernelagent.ResolveAmendmentRequest) (*kernelagent.DefinitionAmendment, error) {
	f.agentDecisions = append(f.agentDecisions, request)
	amendment := f.agentAmendments[0]
	amendment.Decision = &kernelagent.AmendmentDecision{Approved: request.Approved, ActorType: request.ActorType, ActorID: request.ActorID, Reason: request.Reason}
	amendment.Revision = request.ExpectedRevision + 1
	if request.Approved {
		amendment.Status = kernelagent.AmendmentApproved
	} else {
		amendment.Status = kernelagent.AmendmentRejected
	}
	return amendment, nil
}

func (f *fakeKernelClient) ActivateAgentDefinitionAmendment(_ context.Context, _ string, _ string, request kernelapi.ActivateAgentDefinitionAmendmentRequest) (*kernelapi.AgentDefinitionAmendmentActivationResult, error) {
	f.agentActivations = append(f.agentActivations, request)
	amendment := f.agentAmendments[0]
	amendment.Status, amendment.Revision, amendment.ActivationID = kernelagent.AmendmentActivated, request.ExpectedRevision+1, "agent-activation-amendment"
	deployment := *f.agentDeployments[0].Deployment
	deployment.ActiveVersion, deployment.Revision = amendment.Candidate.Version, deployment.Revision+1
	f.agentDeployments[0].Deployment, f.agentDeployments[0].Definition = &deployment, &amendment.Candidate
	return &kernelapi.AgentDefinitionAmendmentActivationResult{Amendment: amendment, Deployment: &deployment, Activation: &workforce.DefinitionActivation{ID: amendment.ActivationID}}, nil
}

func (f *fakeKernelClient) GetAgentRun(_ context.Context, _ runtime.Scope, id string) (*runtime.AgentRun, error) {
	for _, run := range f.runs {
		if run != nil && run.ID == id {
			return run, nil
		}
	}
	return nil, runtime.ErrRunNotFound
}

func (f *fakeKernelClient) CommandAgentRun(_ context.Context, _ runtime.Scope, id string, request kernelapi.AgentRunCommandRequest) (*runtime.AgentRunCommandResult, error) {
	f.commands = append(f.commands, request)
	status := runtime.AgentRunStatusPaused
	if request.Kind == runtime.AgentRunCommandResume {
		status = runtime.AgentRunStatusQueued
	} else if request.Kind == runtime.AgentRunCommandCancel {
		status = runtime.AgentRunStatusCanceled
	}
	run := testRun(id, status, request.ExpectedRevision+1)
	return &runtime.AgentRunCommandResult{Run: run}, nil
}

func (f *fakeKernelClient) RegisterTeamDefinition(context.Context, *kernelteam.Definition) (*kernelteam.Definition, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) ListTeamDefinitionVersions(context.Context, string) ([]*kernelteam.Definition, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) CreateTeamDeployment(context.Context, kernelapi.CreateTeamDeploymentRequest) (*kernelapi.TeamDeploymentResult, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) GetTeamDeployment(context.Context, capability.ScopeReference, string) (*kernelteam.Deployment, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) ListTeamDeployments(_ context.Context, scope capability.ScopeReference) (*kernelapi.TeamDeploymentList, error) {
	items := make([]kernelapi.TeamDeploymentCatalogEntry, 0, len(f.teamDeployments))
	for _, entry := range f.teamDeployments {
		if entry.Deployment != nil && entry.Deployment.Scope == scope {
			items = append(items, entry)
		}
	}
	return &kernelapi.TeamDeploymentList{Items: items}, nil
}

func (f *fakeKernelClient) ActivateTeamDefinition(context.Context, string, kernelapi.ActivateTeamDefinitionRequest) (*kernelapi.TeamDeploymentResult, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) UpdateTeamDeployment(_ context.Context, id string, request kernelapi.UpdateTeamDeploymentRequest) (*kernelapi.TeamDeploymentResult, error) {
	f.teamUpdates = append(f.teamUpdates, request)
	if request.Deployment == nil || request.Deployment.ID != id {
		return nil, errors.New("team deployment id mismatch")
	}
	updated := *request.Deployment
	updated.Revision = request.ExpectedRevision + 1
	updated.UpdatedAt = time.Now()
	for index := range f.teamDeployments {
		if f.teamDeployments[index].Deployment != nil && f.teamDeployments[index].Deployment.ID == id && f.teamDeployments[index].Deployment.Scope == updated.Scope {
			f.teamDeployments[index].Deployment = &updated
			break
		}
	}
	return &kernelapi.TeamDeploymentResult{Deployment: &updated}, nil
}

func (f *fakeKernelClient) ListTeamDefinitionActivations(context.Context, capability.ScopeReference, string) ([]workforce.DefinitionActivation, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) ProposeTeamDefinitionAmendment(_ context.Context, request kernelteam.ProposeAmendmentRequest) (*kernelteam.DefinitionAmendment, error) {
	f.teamProposals = append(f.teamProposals, request)
	amendment := &kernelteam.DefinitionAmendment{ID: "amendment-new", Scope: request.Scope, DeploymentID: request.DeploymentID, DefinitionID: request.Candidate.ID, BaseVersion: "1", BaseDigest: "sha256:base", Candidate: *request.Candidate, Changes: []workforce.DefinitionFieldChange{{Field: "purpose"}}, ProposerType: request.ProposerType, ProposerID: request.ProposerID, Rationale: request.Rationale, Status: kernelteam.AmendmentReady, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.teamAmendments = append([]*kernelteam.DefinitionAmendment{amendment}, f.teamAmendments...)
	return amendment, nil
}

func (f *fakeKernelClient) GetTeamDefinitionAmendment(_ context.Context, scope capability.ScopeReference, deploymentID, amendmentID string) (*kernelteam.DefinitionAmendment, error) {
	for _, amendment := range f.teamAmendments {
		if amendment.Scope == scope && amendment.DeploymentID == deploymentID && amendment.ID == amendmentID {
			return amendment, nil
		}
	}
	return nil, kernelteam.ErrAmendmentNotFound
}

func (f *fakeKernelClient) ListTeamDefinitionAmendments(_ context.Context, scope capability.ScopeReference, deploymentID string) (*kernelapi.TeamDefinitionAmendmentList, error) {
	result := &kernelapi.TeamDefinitionAmendmentList{}
	for _, amendment := range f.teamAmendments {
		if amendment.Scope == scope && amendment.DeploymentID == deploymentID {
			result.Items = append(result.Items, amendment)
		}
	}
	return result, nil
}

func (f *fakeKernelClient) SubmitTeamDefinitionAmendmentEvaluation(_ context.Context, _ string, request kernelteam.SubmitAmendmentEvaluationRequest) (*kernelteam.DefinitionAmendment, error) {
	f.teamEvaluations = append(f.teamEvaluations, request)
	amendment, err := f.GetTeamDefinitionAmendment(context.Background(), request.Scope, f.teamAmendments[0].DeploymentID, request.AmendmentID)
	if err != nil {
		return nil, err
	}
	amendment.Evaluations, amendment.Status, amendment.Revision = request.Evaluations, kernelteam.AmendmentAwaitingApproval, request.ExpectedRevision+1
	return amendment, nil
}

func (f *fakeKernelClient) ResolveTeamDefinitionAmendment(_ context.Context, _ string, request kernelteam.ResolveAmendmentRequest) (*kernelteam.DefinitionAmendment, error) {
	f.teamDecisions = append(f.teamDecisions, request)
	amendment := f.teamAmendments[0]
	amendment.Decision = &kernelteam.AmendmentDecision{Approved: request.Approved, ActorType: request.ActorType, ActorID: request.ActorID, Reason: request.Reason}
	amendment.Revision = request.ExpectedRevision + 1
	if request.Approved {
		amendment.Status = kernelteam.AmendmentApproved
	} else {
		amendment.Status = kernelteam.AmendmentRejected
	}
	return amendment, nil
}

func (f *fakeKernelClient) ActivateTeamDefinitionAmendment(_ context.Context, _ string, _ string, request kernelapi.ActivateTeamDefinitionAmendmentRequest) (*kernelapi.TeamDefinitionAmendmentActivationResult, error) {
	f.teamActivations = append(f.teamActivations, request)
	amendment := f.teamAmendments[0]
	amendment.Status, amendment.Revision, amendment.ActivationID = kernelteam.AmendmentActivated, request.ExpectedRevision+1, "activation-amendment"
	deployment := *f.teamDeployments[0].Deployment
	deployment.ActiveVersion, deployment.Revision = amendment.Candidate.Version, deployment.Revision+1
	f.teamDeployments[0].Deployment, f.teamDeployments[0].Definition = &deployment, &amendment.Candidate
	return &kernelapi.TeamDefinitionAmendmentActivationResult{Amendment: amendment, Deployment: &deployment, Activation: &workforce.DefinitionActivation{ID: amendment.ActivationID}}, nil
}

func (f *fakeKernelClient) RegisterArtifact(context.Context, runtime.RegisterArtifactRequest) (*runtime.ArtifactRegistrationResult, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) GetArtifact(_ context.Context, _ runtime.Scope, id string, version int64) (*runtime.Artifact, error) {
	for _, artifact := range f.artifacts {
		if artifact.ID == id && (version == 0 || artifact.Version == version) {
			return artifact, nil
		}
	}
	return nil, runtime.ErrArtifactNotFound
}

func (f *fakeKernelClient) ListArtifacts(context.Context, runtime.ArtifactFilter) ([]*runtime.Artifact, error) {
	return f.artifacts, nil
}

func (f *fakeKernelClient) UploadArtifactContent(context.Context, runtime.Scope, string, string, int64, io.Reader) (*runtime.ArtifactStoredContent, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) DownloadArtifactContent(context.Context, runtime.Scope, string, int64) (*client.ArtifactDownload, error) {
	f.downloadCalls++
	return &client.ArtifactDownload{
		Body:   io.NopCloser(strings.NewReader(f.downloadBody)),
		Digest: testDigest(f.downloadBody), SizeBytes: int64(len(f.downloadBody)),
	}, nil
}

func (f *fakeKernelClient) ResolveArtifactContent(context.Context, runtime.Scope, string, int64, kernelapi.ResolveArtifactContentRequest) (*runtime.ArtifactContentResolution, error) {
	return nil, errors.New("not implemented by test client")
}

func TestFakeKernelClientAgentDeploymentsRespectScope(t *testing.T) {
	tenantOne := capability.ScopeReference{Kind: "tenant", ID: "one"}
	tenantTwo := capability.ScopeReference{Kind: "tenant", ID: "two"}
	fake := &fakeKernelClient{agentDeployments: []kernelapi.AgentDeploymentCatalogEntry{
		{Deployment: &kernelagent.AgentDeployment{ID: "shared", Scope: tenantOne}},
		{Deployment: &kernelagent.AgentDeployment{ID: "shared", Scope: tenantTwo}},
	}}

	entry, err := fake.GetAgentDeployment(context.Background(), tenantTwo, "shared")
	if err != nil || entry.Deployment.Scope != tenantTwo {
		t.Fatalf("get exact tenant deployment: entry=%+v err=%v", entry, err)
	}
	if _, err := fake.GetAgentDeployment(context.Background(), capability.ScopeReference{Kind: "tenant", ID: "other"}, "shared"); !errors.Is(err, kernelagent.ErrDeploymentNotFound) {
		t.Fatalf("cross-scope get error = %v, want deployment not found", err)
	}
	list, err := fake.ListAgentDeployments(context.Background(), kernelagent.AgentDeploymentFilter{Scope: tenantOne})
	if err != nil || len(list.Items) != 1 || list.Items[0].Deployment.Scope != tenantOne {
		t.Fatalf("list tenant deployments: list=%+v err=%v", list, err)
	}
}

func TestModelDiscoversCapabilitiesBeforeRenderingActions(t *testing.T) {
	limited := kernelapi.Capability{
		ID: kernelapi.AgentRunsCapabilityID, Version: kernelapi.AgentRunsCapabilityVersion,
		Available: true, Operations: []string{kernelapi.OperationCreate, kernelapi.OperationList},
	}
	fake := &fakeKernelClient{
		document: kernelapi.CapabilityDocument{APIVersion: kernelapi.APIVersion, Capabilities: []kernelapi.Capability{limited}},
		runs:     []*runtime.AgentRun{testRun("run-1", runtime.AgentRunStatusQueued, 1)},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	view := model.View()
	if !strings.Contains(view, "Current work") || !strings.Contains(view, "First durable outcome") {
		t.Fatalf("work view missing canonical run:\n%s", view)
	}
	for _, action := range []string{"p pause", "g guide", "x stop"} {
		if strings.Contains(view, action) {
			t.Fatalf("unadvertised action %q was rendered:\n%s", action, view)
		}
	}
}

func TestTUIInspectsPortableWorkforceBundleThroughAdvertisedKernelCapability(t *testing.T) {
	definition := &kernelagent.AgentDefinition{
		ID: "portable/researcher", Version: "1.0.0", DisplayName: "Researcher", Purpose: "Research questions", SystemPrompt: "Research carefully.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	}
	deployment := &kernelagent.AgentDeployment{
		ID: "agent:source", Scope: capability.ScopeReference{Kind: "tenant", ID: "source"}, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1,
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
	agentArtifact, err := kernelagent.ExportBundle(kernelagent.BundleExportRequest{
		Definition: definition, Deployment: deployment,
		Metadata: kernelagent.BundleMetadata{ID: "researcher", Version: "1.0.0", DisplayName: "Researcher"},
		Manifest: kernelagent.ManifestMetadata{ID: "researcher", Version: "1.0.0", DisplayName: "Researcher"},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := kernelbundle.New(kernelbundle.Metadata{ID: "research", Version: "1.0.0", DisplayName: "Research"})
	artifact.Agents = []kernelbundle.Agent{{Key: "researcher", Artifact: agentArtifact}}
	if err := artifact.Seal(); err != nil {
		t.Fatal(err)
	}
	encoded, err := kernelbundle.EncodeYAML(artifact)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/research.yaml"
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeWorkforceBundleKernelClient{fakeKernelClient: &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(kernelapi.WorkforceBundlesCapability(false)),
	}}
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.section != sectionBundles || !strings.Contains(model.View(), "B Imports") {
		t.Fatalf("bundle capability not projected: section=%v view=%s", model.section, model.View())
	}
	model.editor.SetValue(path)
	applyCommand(t, model, model.submitWorkforceBundleInspection())
	if fake.inspected != 1 || model.workforceBundleInspection == nil {
		t.Fatalf("inspection calls=%d result=%#v", fake.inspected, model.workforceBundleInspection)
	}
	for _, expected := range []string{"research@1.0.0", "Agents 1", artifact.Digest[:32]} {
		if !strings.Contains(model.View(), expected) {
			t.Fatalf("bundle view missing %q: %s", expected, model.View())
		}
	}
}

func TestFullStandaloneWorkspaceStartsAtGuidedHomeWithDiscoverableNavigationAndHelp(t *testing.T) {
	fake := newAgentBuilderTestClient()
	fake.document = kernelapi.NewCapabilityDocument(
		kernelapi.AgentBuilderCapability(runtime.AgentBuilderDeploymentID),
		kernelapi.AgentDefinitionsCapability(),
		kernelapi.TeamDefinitionsCapability(kernelapi.TeamDefinitionCapabilityFeatures{}),
		kernelapi.AgentRunsCapability(kernelapi.OperationList),
		kernelapi.ObjectivesCapability(),
		kernelapi.AgentRequestsCapability(),
	)
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	view := model.View()
	for _, expected := range []string{"Welcome to OpenSeal", "Describe the agent you need", "Approve the card", "Start here", "How it works", "Home", "f Create", "h Agents", "T Teams", "w Work", "o Goals", "R Inbox", "← / → pages", "? help"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("Home missing %q:\n%s", expected, view)
		}
	}
	if model.section != sectionOverview {
		t.Fatalf("full workspace section = %v", model.section)
	}
	model.focusPanelList()
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if !model.showHelp || !strings.Contains(model.View(), "OpenSeal help") || !strings.Contains(model.View(), "Workspace destinations") {
		t.Fatalf("global help unavailable:\n%s", model.View())
	}
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if model.showHelp {
		t.Fatal("Escape did not close help")
	}
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRight})
	if model.section != sectionAuthoring {
		t.Fatalf("right navigation section = %v", model.section)
	}
	view = model.View()
	for _, expected := range []string{"New agent", "Try an example", "Type on the left and press Enter"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("Create page missing %q:\n%s", expected, view)
		}
	}
}

func TestHomeEnterOpensNewAgentAndStartsBuilderChat(t *testing.T) {
	fake := newAgentBuilderTestClient()
	fake.document = kernelapi.NewCapabilityDocument(
		kernelapi.AgentBuilderCapability(runtime.AgentBuilderDeploymentID),
		kernelapi.ChannelsCapability(kernelapi.ChannelCapabilityFeatures{}),
		kernelapi.AgentDefinitionsCapability(),
		kernelapi.TeamDefinitionsCapability(kernelapi.TeamDefinitionCapabilityFeatures{}),
		kernelapi.AgentRunsCapability(kernelapi.OperationList),
		kernelapi.ObjectivesCapability(),
	)
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.section != sectionOverview || model.focus != focusPanel {
		t.Fatalf("home section=%v focus=%v", model.section, model.focus)
	}
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if model.section != sectionAuthoring || model.mode != modeAgentBuilderPrompt || model.focus != focusComposer {
		t.Fatalf("Enter on Home section=%v mode=%v focus=%v", model.section, model.mode, model.focus)
	}
	model.editor.SetValue("An agent that books my travel")
	_, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	applyCommand(t, model, command)
	if len(fake.builderStarts) != 1 || fake.builderStarts[0].Prompt != "An agent that books my travel" ||
		fake.builderStarts[0].Parameters.Kind != runtime.AgentBuilderKindAgent {
		t.Fatalf("builder starts = %#v", fake.builderStarts)
	}
	if model.section != sectionChannels || model.mode != modeChannelPost || model.selectedConversation != "builder-chat" {
		t.Fatalf("builder chat not open section=%v mode=%v selected=%q", model.section, model.mode, model.selectedConversation)
	}
}

func TestTUIRunbookScheduleReconciliationIsCapabilityGated(t *testing.T) {
	scope := runtime.Scope{Kind: "local", ID: "default"}
	fake := &fakeKernelClient{
		document:       kernelapi.NewCapabilityDocument(kernelapi.ObjectivesCapability(), kernelapi.RunbooksCapability()),
		scheduleResult: &kernelapi.RunbookScheduleReconciliation{Scope: scope, Result: &runtime.RunbookScheduleResult{Examined: 3, Scheduled: 2}},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	applyCommand(t, model, model.reconcileRunbookSchedules())
	if len(fake.scheduleRequests) != 1 || fake.scheduleRequests[0].Scope != scope || fake.scheduleRequests[0].Limit != 100 {
		t.Fatalf("schedule requests = %#v", fake.scheduleRequests)
	}
	if !strings.Contains(model.status, "3 examined · 2 Runs scheduled") || !strings.Contains(model.View(), "g reconcile schedules") {
		t.Fatalf("schedule projection missing: status=%q view=%s", model.status, model.View())
	}

	drifted := kernelapi.RunbooksCapability()
	drifted.Version = "future"
	closedFake := &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.ObjectivesCapability(), drifted)}
	closed := newTestModel(t, closedFake)
	applyCommand(t, closed, closed.loadCapabilities())
	if closed.runbookCapability.Available || closed.reconcileRunbookSchedules() != nil || len(closedFake.scheduleRequests) != 0 || strings.Contains(closed.View(), "reconcile schedules") {
		t.Fatalf("mismatched Runbook capability did not fail closed: %#v", closed.runbookCapability)
	}
}

func TestTeamsAreFirstClassInspectableAndRevisionSafeInTUI(t *testing.T) {
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	definition := &kernelteam.Definition{
		ID: "gtm-research", Version: "3", DisplayName: "GTM Research", Purpose: "Find evidence, synthesize it, and coordinate approved outreach.",
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "monitor", Title: "Monitor source communities"}, {ID: "report", Title: "Deliver cited report"}},
		Roles:              []kernelteam.RoleSlot{{ID: "research", DisplayName: "Researcher", Purpose: "Find evidence"}},
	}
	deployment := &kernelteam.Deployment{
		ID: "gtm-live", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		Status: kernelteam.DeploymentActive, Revision: 7,
		Roster:       []kernelteam.RosterAssignment{{ID: "researcher", RoleID: "research", AgentDeploymentID: "agent-research", DisplayName: "Researcher"}},
		Restrictions: kernelteam.DeploymentRestrictions{MaximumConcurrency: 3, MaximumRisk: capability.RiskLevelExternal},
	}
	fake := &fakeKernelClient{
		document:        kernelapi.NewCapabilityDocument(kernelapi.TeamDefinitionsCapability(kernelapi.TeamDefinitionCapabilityFeatures{})),
		teamDeployments: []kernelapi.TeamDeploymentCatalogEntry{{Deployment: deployment, Definition: definition}},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.section != sectionTeams || model.selectedTeamDeployment != deployment.ID {
		t.Fatalf("team projection section=%v selected=%q", model.section, model.selectedTeamDeployment)
	}
	view := model.View()
	for _, expected := range []string{"T Teams", "GTM Research", "Definition gtm-research@3", "relevance-arbitrated", "Researcher · research · Agent agent-research", "2 template(s)", "p pause/resume"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("Team view missing %q:\n%s", expected, view)
		}
	}

	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if updated != model || command == nil {
		t.Fatal("pause did not create a Team lifecycle command")
	}
	applyCommand(t, model, command)
	if len(fake.teamUpdates) != 1 {
		t.Fatalf("team updates=%d, want 1", len(fake.teamUpdates))
	}
	request := fake.teamUpdates[0]
	if request.ExpectedRevision != 7 || request.Deployment.Status != kernelteam.DeploymentPaused || request.ActorType != "user" || request.ActorID != "local" || request.Reason == "" {
		t.Fatalf("unsafe Team lifecycle request: %#v", request)
	}
	if selected := model.selectedTeamDeploymentRecord(); selected == nil || selected.Deployment == nil || selected.Deployment.Revision != 8 || selected.Deployment.Status != kernelteam.DeploymentPaused {
		t.Fatalf("updated Team was not reloaded: %#v", selected)
	}
}

func TestAgentAmendmentGovernanceIsInspectableRevisionSafeAndCapabilityGated(t *testing.T) {
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	definition := &kernelagent.AgentDefinition{
		ID: "operator", Version: "1", DisplayName: "Operator", Purpose: "Operate safely", SystemPrompt: "Verify evidence before acting.", Personality: "Calm and precise.",
		Authority:   kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		Evaluations: []workforce.EvaluationCriterion{{ID: "safety", Description: "Behavior remains bounded", Required: true}},
		Amendments:  workforce.AmendmentPolicy{AllowedFields: []string{"systemPrompt", "personality"}, RequiresApproval: true, ApproverPrincipals: []string{"user:local"}},
		Digest:      "sha256:base",
	}
	candidate := *definition
	candidate.Version, candidate.SystemPrompt = "2", "Verify attributable evidence and state uncertainty before acting."
	deployment := &kernelagent.AgentDeployment{ID: "operator", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive, Revision: 9}
	amendment := &kernelagent.DefinitionAmendment{
		ID: "agent-amend-1", Scope: scope, DeploymentID: deployment.ID, DefinitionID: definition.ID, BaseVersion: definition.Version, BaseDigest: definition.Digest,
		Candidate: candidate, Changes: []workforce.DefinitionFieldChange{{Field: "systemPrompt"}}, ProposerType: "agent", ProposerID: "operator", Rationale: "Require attributable evidence",
		Status: kernelagent.AmendmentEvaluating, Revision: 4, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	globalDocument := kernelapi.NewCapabilityDocument(kernelapi.Capability{
		ID: kernelapi.AgentDefinitionsCapabilityID, Version: kernelapi.AgentDefinitionsCapabilityVersion,
		Available: true, Operations: []string{kernelapi.OperationGet, kernelapi.OperationList, kernelapi.OperationListCompilations},
	})
	contextualDocument := kernelapi.NewCapabilityDocument(kernelapi.AgentDefinitionsCapability(kernelapi.AgentDefinitionCapabilityFeatures{Lifecycle: true, Amendments: true}))
	fake := &fakeKernelClient{
		document:         globalDocument,
		agentDocument:    &contextualDocument,
		agentDeployments: []kernelapi.AgentDeploymentCatalogEntry{{Deployment: deployment, Definition: definition}}, agentAmendments: []*kernelagent.DefinitionAmendment{amendment},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if len(fake.agentCapabilityCalls) != 1 || fake.agentCapabilityCalls[0].scope != scope || fake.agentCapabilityCalls[0].deploymentID != deployment.ID {
		t.Fatalf("contextual Agent capability calls = %#v", fake.agentCapabilityCalls)
	}
	view := model.View()
	for _, expected := range []string{"Agent runtime & placement", "Governance history · 1 amendment(s)", "evaluating · 1 → 2 · r4", "Proposed by agent:operator", "Changed · systemPrompt", "evaluate"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("Agent governance view missing %q:\n%s", expected, view)
		}
	}

	model.section, model.focus = sectionReadiness, focusPanel
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	model.editor.SetValue("safety=pass: Prompt remains bounded to attributable evidence")
	_, command := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)
	if len(fake.agentEvaluations) != 1 || fake.agentEvaluations[0].ExpectedRevision != 4 || len(fake.agentEvaluations[0].Evaluations) != 1 || !fake.agentEvaluations[0].Evaluations[0].Passed {
		t.Fatalf("unsafe Agent evaluation request: %#v", fake.agentEvaluations)
	}

	model.section, model.focus = sectionReadiness, focusPanel
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model.editor.SetValue("Required safety evidence passed")
	_, command = model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)
	if len(fake.agentDecisions) != 1 || fake.agentDecisions[0].ExpectedRevision != 5 || !fake.agentDecisions[0].Approved || fake.agentDecisions[0].ActorType != "user" || fake.agentDecisions[0].ActorID != "local" {
		t.Fatalf("unsafe Agent decision request: %#v", fake.agentDecisions)
	}

	model.section, model.focus = sectionReadiness, focusPanel
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	model.editor.SetValue("Reviewed safety evidence supports activation")
	_, command = model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)
	if len(fake.agentActivations) != 1 || fake.agentActivations[0].ExpectedRevision != 6 || fake.agentActivations[0].ActorType != "user" || fake.agentActivations[0].ActorID != "local" || fake.agentDeployments[0].Deployment.ActiveVersion != "2" {
		t.Fatalf("unsafe Agent activation request: requests=%#v deployment=%#v", fake.agentActivations, fake.agentDeployments[0].Deployment)
	}
	if !strings.Contains(model.View(), "activated · 1 → 2 · r7") || !strings.Contains(model.View(), "Activation · agent-activation-amendment") {
		t.Fatalf("authoritative Agent activation was not rendered:\n%s", model.View())
	}

	proposalFake := &fakeKernelClient{
		document:         kernelapi.NewCapabilityDocument(kernelapi.AgentDefinitionsCapability(kernelapi.AgentDefinitionCapabilityFeatures{Lifecycle: true, Amendments: true})),
		agentDeployments: []kernelapi.AgentDeploymentCatalogEntry{{Deployment: deployment, Definition: definition}},
	}
	proposal := newTestModel(t, proposalFake)
	applyCommand(t, proposal, proposal.loadCapabilities())
	proposal.section, proposal.focus = sectionReadiness, focusPanel
	_, _ = proposal.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	proposal.editor.SetValue("personality\nMake collaboration more concise\nConcise, calm, and direct.")
	_, command = proposal.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, proposal, command)
	if len(proposalFake.agentProposals) != 1 || proposalFake.agentProposals[0].ExpectedDeploymentRevision != 9 || proposalFake.agentProposals[0].Candidate.Personality != "Concise, calm, and direct." || proposalFake.agentProposals[0].Candidate.SystemPrompt != definition.SystemPrompt {
		t.Fatalf("Agent proposal did not preserve immutable behavior: %#v", proposalFake.agentProposals)
	}

	readOnlyFake := &fakeKernelClient{
		document:         kernelapi.NewCapabilityDocument(kernelapi.AgentDefinitionsCapability(kernelapi.AgentDefinitionCapabilityFeatures{Lifecycle: true})),
		agentDeployments: []kernelapi.AgentDeploymentCatalogEntry{{Deployment: deployment, Definition: definition}}, agentAmendments: []*kernelagent.DefinitionAmendment{amendment},
	}
	readOnly := newTestModel(t, readOnlyFake)
	applyCommand(t, readOnly, readOnly.loadCapabilities())
	for _, forbidden := range []string{"Governance history", "m propose behavior", "Enter evaluate", "y approve", "v activate"} {
		if strings.Contains(readOnly.View(), forbidden) {
			t.Fatalf("unadvertised Agent governance control %q rendered:\n%s", forbidden, readOnly.View())
		}
	}
}

func TestTeamAmendmentGovernanceIsInspectableRevisionSafeAndCapabilityGated(t *testing.T) {
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	definition := &kernelteam.Definition{
		ID: "research", Version: "1", DisplayName: "Research", Purpose: "Monitor evidence",
		Roles:       []kernelteam.RoleSlot{{ID: "analyst", DisplayName: "Analyst", Purpose: "Synthesize evidence"}},
		Approvals:   kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
		Evaluations: []workforce.EvaluationCriterion{{ID: "evidence", Description: "Evidence remains attributable", Required: true}},
		Amendments:  workforce.AmendmentPolicy{AllowedFields: []string{"purpose"}, RequiresApproval: true, ApproverPrincipals: []string{"user:local"}},
	}
	candidate := *definition
	candidate.Version, candidate.Purpose = "2", "Monitor and synthesize attributable evidence"
	deployment := &kernelteam.Deployment{ID: "research-live", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version, Status: kernelteam.DeploymentActive, Revision: 9}
	amendment := &kernelteam.DefinitionAmendment{
		ID: "amend-1", Scope: scope, DeploymentID: deployment.ID, DefinitionID: definition.ID, BaseVersion: definition.Version, BaseDigest: "sha256:base",
		Candidate: candidate, Changes: []workforce.DefinitionFieldChange{{Field: "purpose"}}, ProposerType: "agent", ProposerID: "researcher", Rationale: "Synthesis is now part of the mandate",
		Status: kernelteam.AmendmentEvaluating, Revision: 4, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	fake := &fakeKernelClient{
		document:        kernelapi.NewCapabilityDocument(kernelapi.TeamDefinitionsCapability(kernelapi.TeamDefinitionCapabilityFeatures{Amendments: true})),
		teamDeployments: []kernelapi.TeamDeploymentCatalogEntry{{Deployment: deployment, Definition: definition}}, teamAmendments: []*kernelteam.DefinitionAmendment{amendment},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	view := model.View()
	for _, expected := range []string{"Governance history · 1 amendment(s)", "evaluating · 1 → 2 · r4", "Proposed by agent:researcher", "Changed · purpose", "evaluate"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("Team governance view missing %q:\n%s", expected, view)
		}
	}

	model.section, model.focus = sectionTeams, focusPanel
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	model.editor.SetValue("evidence=pass: Sources remain linked to immutable observations")
	_, command := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)
	if len(fake.teamEvaluations) != 1 || fake.teamEvaluations[0].ExpectedRevision != 4 || len(fake.teamEvaluations[0].Evaluations) != 1 || !fake.teamEvaluations[0].Evaluations[0].Passed {
		t.Fatalf("unsafe evaluation request: %#v", fake.teamEvaluations)
	}

	model.section, model.focus = sectionTeams, focusPanel
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model.editor.SetValue("Required evidence gate passed")
	_, command = model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)
	if len(fake.teamDecisions) != 1 || fake.teamDecisions[0].ExpectedRevision != 5 || !fake.teamDecisions[0].Approved || fake.teamDecisions[0].ActorType != "user" || fake.teamDecisions[0].ActorID != "local" {
		t.Fatalf("unsafe decision request: %#v", fake.teamDecisions)
	}

	model.section, model.focus = sectionTeams, focusPanel
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	model.editor.SetValue("Reviewed evidence supports activation")
	_, command = model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)
	if len(fake.teamActivations) != 1 || fake.teamActivations[0].ExpectedRevision != 6 || fake.teamActivations[0].ActorType != "user" || fake.teamActivations[0].ActorID != "local" || fake.teamDeployments[0].Deployment.ActiveVersion != "2" {
		t.Fatalf("unsafe activation request: requests=%#v deployment=%#v", fake.teamActivations, fake.teamDeployments[0].Deployment)
	}
	if !strings.Contains(model.View(), "activated · 1 → 2 · r7") || !strings.Contains(model.View(), "Activation · activation-amendment") {
		t.Fatalf("authoritative activation was not rendered:\n%s", model.View())
	}

	readOnly := newTestModel(t, &fakeKernelClient{
		document:        kernelapi.NewCapabilityDocument(kernelapi.TeamDefinitionsCapability(kernelapi.TeamDefinitionCapabilityFeatures{})),
		teamDeployments: []kernelapi.TeamDeploymentCatalogEntry{{Deployment: deployment, Definition: definition}}, teamAmendments: []*kernelteam.DefinitionAmendment{amendment},
	})
	applyCommand(t, readOnly, readOnly.loadCapabilities())
	for _, forbidden := range []string{"Governance history", "m propose purpose", "Enter evaluate", "y approve", "v activate"} {
		if strings.Contains(readOnly.View(), forbidden) {
			t.Fatalf("unadvertised Team governance control %q rendered:\n%s", forbidden, readOnly.View())
		}
	}
}

func TestAgentRequestsAreAFirstClassCapabilityGatedProjection(t *testing.T) {
	now := time.Now()
	local := runtime.CollaborationParty{Type: runtime.OwnerTypeAgent, ID: "operator"}
	peer := runtime.CollaborationParty{Type: runtime.OwnerTypeAgent, ID: "researcher"}
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(kernelapi.AgentRequestsCapability()),
		agentRequests: []*runtime.AgentRequest{
			{ID: "incoming", Scope: runtime.Scope{Kind: "local", ID: "default"}, Kind: runtime.AgentRequestKindRequest, Status: runtime.AgentRequestStatusPending, Requester: peer, Recipient: local, SourceRunID: "source-in", Goal: "Review the cited report", Revision: 1, CreatedAt: now, UpdatedAt: now},
			{ID: "outgoing", Scope: runtime.Scope{Kind: "local", ID: "default"}, Kind: runtime.AgentRequestKindHandoff, Status: runtime.AgentRequestStatusClarificationRequested, Requester: local, Recipient: peer, SourceRunID: "source-out", Goal: "Publish the release brief", Clarification: "Which audience?", Revision: 2, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)},
			{ID: "unrelated", Scope: runtime.Scope{Kind: "local", ID: "default"}, Kind: runtime.AgentRequestKindRequest, Status: runtime.AgentRequestStatusPending, Requester: peer, Recipient: runtime.CollaborationParty{Type: runtime.OwnerTypeAgent, ID: "other"}, SourceRunID: "source-other", Goal: "Not visible", Revision: 1, CreatedAt: now, UpdatedAt: now},
		},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.section != sectionRequests || len(model.agentRequests) != 2 || len(fake.agentRequestFilters) != 2 {
		t.Fatalf("request projection section=%v requests=%d filters=%#v", model.section, len(model.agentRequests), fake.agentRequestFilters)
	}
	view := model.View()
	for _, expected := range []string{"R Inbox", "Collaboration requests", "Review the cited report", "from agent:researcher", "y accept", "q clarify", "x reject"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("request view missing %q:\n%s", expected, view)
		}
	}
}

func TestPromptFirstAgentRequestCreationUsesSelectedRunAndIdempotency(t *testing.T) {
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(kernelapi.AgentRunsCapability(), kernelapi.AgentRequestsCapability()),
		runs:     []*runtime.AgentRun{testRun("source-run", runtime.AgentRunStatusRunning, 4)},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.section, model.focus = sectionRequests, focusPanel
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if model.mode != modeRequestCreate || model.focus != focusComposer {
		t.Fatalf("request composer not prepared: mode=%v focus=%v", model.mode, model.focus)
	}
	model.editor.SetValue("handoff team:marketing\nPublish a cited release brief and coordinate approved follow-up")
	_, command := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)
	if len(fake.agentRequestCreates) != 1 || len(fake.agentRequestKeys) != 1 || fake.agentRequestKeys[0] == "" {
		t.Fatalf("creates=%#v keys=%#v", fake.agentRequestCreates, fake.agentRequestKeys)
	}
	created := fake.agentRequestCreates[0]
	if created.Kind != runtime.AgentRequestKindHandoff || created.SourceRunID != "source-run" || created.Requester != (runtime.CollaborationParty{Type: runtime.OwnerTypeAgent, ID: "operator"}) || created.Recipient != (runtime.CollaborationParty{Type: runtime.OwnerTypeTeam, ID: "marketing"}) || created.IdempotencyKey != fake.agentRequestKeys[0] || !strings.Contains(created.Goal, "cited release brief") {
		t.Fatalf("create request=%#v", created)
	}
	if model.selectedAgentRequest != "created-request" || !strings.Contains(model.View(), "Publish a cited release brief") {
		t.Fatalf("created request was not selected:\n%s", model.View())
	}
}

func TestAgentRequestPromptParsesEscalation(t *testing.T) {
	kind, recipient, goal, err := parseAgentRequestPrompt("escalation team:incident-command\nResolve the production blocker")
	if err != nil {
		t.Fatal(err)
	}
	if kind != runtime.AgentRequestKindEscalation ||
		recipient != (runtime.CollaborationParty{Type: runtime.OwnerTypeTeam, ID: "incident-command"}) ||
		goal != "Resolve the production blocker" {
		t.Fatalf("parsed escalation = %q / %#v / %q", kind, recipient, goal)
	}
}

func TestAgentRequestComposerRejectsTerminalSourceWork(t *testing.T) {
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(kernelapi.AgentRunsCapability(), kernelapi.AgentRequestsCapability()),
		runs:     []*runtime.AgentRun{testRun("finished-source", runtime.AgentRunStatusCompleted, 5)},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.section, model.focus = sectionRequests, focusPanel
	view := model.View()
	if strings.Contains(view, "n request from selected Work") {
		t.Fatalf("terminal source advertised request creation:\n%s", view)
	}
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if model.mode == modeRequestCreate {
		t.Fatalf("terminal source opened request composer")
	}
	model.mode = modeRequestCreate
	model.editor.SetValue("agent:marketing\nPrepare a follow-up")
	if command := model.submitAgentRequestCreation(); command != nil || len(fake.agentRequestCreates) != 0 || !strings.Contains(model.status, "Start or resume active work") {
		t.Fatalf("terminal submission command=%v creates=%#v status=%q", command, fake.agentRequestCreates, model.status)
	}
}

func TestAgentRequestPromptRejectsAmbiguousRecipients(t *testing.T) {
	for _, prompt := range []string{"researcher\nDo work", "user:alice\nDo work", "agent:researcher", "agent:\nDo work"} {
		if _, _, _, err := parseAgentRequestPrompt(prompt); err == nil {
			t.Fatalf("parseAgentRequestPrompt(%q) unexpectedly succeeded", prompt)
		}
	}
}

func TestAgentRequestResponsesAndCompletionUseExactDurableRevisions(t *testing.T) {
	now := time.Now()
	local := runtime.CollaborationParty{Type: runtime.OwnerTypeAgent, ID: "operator"}
	peer := runtime.CollaborationParty{Type: runtime.OwnerTypeAgent, ID: "developer"}
	request := &runtime.AgentRequest{ID: "request-1", Scope: runtime.Scope{Kind: "local", ID: "default"}, Kind: runtime.AgentRequestKindRequest, Status: runtime.AgentRequestStatusPending, Requester: peer, Recipient: local, SourceRunID: "source", Goal: "Review release evidence", Revision: 4, CreatedAt: now, UpdatedAt: now}
	fake := &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.AgentRequestsCapability()), agentRequests: []*runtime.AgentRequest{request}, runs: []*runtime.AgentRun{testRun("child-run", runtime.AgentRunStatusRunning, 7)}}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.section, model.focus = sectionRequests, focusPanel
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if model.mode != modeRequestAccept || model.focus != focusComposer {
		t.Fatalf("accept composer not prepared: mode=%v focus=%v", model.mode, model.focus)
	}
	model.editor.SetValue("Ready to review")
	_, command := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)
	if len(fake.agentResponses) != 1 || fake.agentResponses[0].ExpectedRevision != 4 || fake.agentResponses[0].Principal != local || fake.agentResponses[0].Decision != runtime.AgentRequestDecisionAccept {
		t.Fatalf("response=%#v", fake.agentResponses)
	}
	if request.Status != runtime.AgentRequestStatusAccepted || request.ChildRunID == "" {
		t.Fatalf("accepted request=%#v", request)
	}
	model.section, model.focus = sectionRequests, focusPanel
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if model.mode != modeRequestComplete {
		t.Fatalf("completion composer mode=%v", model.mode)
	}
	model.editor.SetValue("Release evidence is complete")
	_, command = model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)
	if len(fake.agentCompletions) != 1 || fake.agentCompletions[0].ExpectedRevision != 5 || fake.agentCompletions[0].ExpectedChildRevision != 7 || fake.agentCompletions[0].Principal != local || fake.agentCompletionKeys[0] == "" || fake.agentCompletions[0].IdempotencyKey != fake.agentCompletionKeys[0] {
		t.Fatalf("completion=%#v keys=%#v", fake.agentCompletions, fake.agentCompletionKeys)
	}
}

func TestActionApprovalResolutionRequiresAdvertisedOperationAndEligiblePrincipal(t *testing.T) {
	now := time.Now()
	approval := &runtime.ApprovalCheckpoint{
		ID: "approval-1", Scope: runtime.Scope{Kind: "local", ID: "default"}, RunID: "run-1", ActionCallID: "call-1",
		Status: runtime.ApprovalStatusPending, Risk: "production", Summary: "Publish the release announcement", PolicyReason: "External side effect",
		ProposedAction: map[string]interface{}{"skillId": "publisher", "skillVersion": "1.0.0", "action": "publish"}, EvidenceRefs: []string{"artifact:brief@1"},
		EligibleApprovers: []runtime.ApprovalPrincipal{{Type: "user", ID: "local"}}, ExpiresAt: now.Add(time.Hour), Revision: 3, CreatedAt: now, UpdatedAt: now,
	}
	fake := &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.ActionApprovalsCapability(kernelapi.ActionApprovalCapabilityFeatures{Resolution: true})), actionApprovals: []*runtime.ApprovalCheckpoint{approval}}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.section != sectionApprovals || len(fake.approvalFilters) != 1 || fake.approvalFilters[0].Owner == nil || *fake.approvalFilters[0].Owner != model.config.Owner {
		t.Fatalf("approval projection section=%v filters=%#v", model.section, fake.approvalFilters)
	}
	view := model.View()
	for _, expected := range []string{"A Approvals", "Action approvals", "External side effect", "publisher@1.0.0 / publish", "y approve", "x reject"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("approval view missing %q:\n%s", expected, view)
		}
	}
	model.focus = focusPanel
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model.editor.SetValue("Evidence confirms the authorized release window")
	_, command := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)
	if len(fake.actionDecisions) != 1 || fake.actionDecisions[0].Decision != runtime.ApprovalDecisionApprove || fake.actionDecisions[0].ExpectedRevision != 3 || fake.actionDecisions[0].Principal != (runtime.ApprovalPrincipal{Type: "user", ID: "local"}) || fake.actionDecisionKeys[0] == "" || fake.actionDecisions[0].DecisionID != fake.actionDecisionKeys[0] {
		t.Fatalf("decision=%#v keys=%#v", fake.actionDecisions, fake.actionDecisionKeys)
	}

	readOnly := &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.ActionApprovalsCapability(kernelapi.ActionApprovalCapabilityFeatures{})), actionApprovals: []*runtime.ApprovalCheckpoint{approval}}
	readOnlyModel := newTestModel(t, readOnly)
	applyCommand(t, readOnlyModel, readOnlyModel.loadCapabilities())
	if strings.Contains(readOnlyModel.View(), "y approve") || strings.Contains(readOnlyModel.View(), "x reject") {
		t.Fatalf("resolution controls rendered without advertised operation:\n%s", readOnlyModel.View())
	}
}

func TestActivityWorkspacePollsPaginatesAndExpandsWithoutLosingSelection(t *testing.T) {
	now := time.Now().UTC()
	summary := runtime.ActivityProjection{
		ID: "event-2", EventType: "approval.resolved", Category: "approval", Severity: runtime.ActivitySeverityInfo,
		Visibility: runtime.ActivityVisibilityScope, AgentID: "operator", RunID: "run-2",
		Actor: runtime.ActivityActor{Type: "user", ID: "alice"}, Summary: "Production restart approved", CreatedAt: now,
		DetailAvailable: true,
	}
	older := runtime.ActivityProjection{
		ID: "event-1", EventType: "run.created", Category: "run", Severity: runtime.ActivitySeverityInfo,
		Visibility: runtime.ActivityVisibilityScope, AgentID: "operator", RunID: "run-1",
		Actor: runtime.ActivityActor{Type: "system", ID: "scheduler"}, Summary: "Investigation started", CreatedAt: now.Add(-time.Minute),
	}
	detail := summary
	detail.ParentRunID = "root-run"
	detail.CorrelationID = "incident-42"
	detail.ConversationRefs = []string{"conversation:sre"}
	detail.UsageDelta = &runtime.BudgetUsage{Turns: 1, InputTokens: 120, OutputTokens: 30, CostMicros: 125_000, DurationMS: 500}
	if got := activityUsageLine(detail.UsageDelta); got != "Usage · 1 turn · 150 tokens (120 in, 30 out) · $0.125 · 500ms" {
		t.Fatalf("usage line = %q", got)
	}
	detail.Payload = map[string]interface{}{"approvalId": "approval-7", "decisionReason": "Exact workload and namespace reviewed"}
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(kernelapi.ActivityCapability()),
		activityPages: map[string]*runtime.ActivityFeedPage{
			"agent:operator":                 {Items: []runtime.ActivityProjection{summary}, HasMore: true, NextCursor: "cursor-1"},
			"agent:operator|cursor:cursor-1": {Items: []runtime.ActivityProjection{summary, older}},
			"run:run-2|details":              {Items: []runtime.ActivityProjection{detail}},
		},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.section != sectionActivity || len(model.activity) != 1 || fake.activityRequests[0].AgentID != "operator" || fake.activityRequests[0].TeamID != "" || fake.activityRequests[0].IncludeDetails {
		t.Fatalf("initial Activity state section=%v activity=%#v requests=%#v", model.section, model.activity, fake.activityRequests)
	}

	applyCommand(t, model, model.loadSelectedActivityDetail())
	if !model.activityExpanded || model.selectedActivity != "event-2" {
		t.Fatalf("expanded=%t selected=%q", model.activityExpanded, model.selectedActivity)
	}
	view := model.View()
	for _, expected := range []string{"t Activity", "Production restart approved", "Usage · 1 turn · 150 tokens", "Evidence · redacted canonical projection", "approval-7", "incident-42", "conversation:sre", "Parent Run root-run"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("Activity view missing %q:\n%s", expected, view)
		}
	}

	applyCommand(t, model, model.loadActivity(true))
	if len(model.activity) != 2 || model.selectedActivity != "event-2" || !model.activityExpanded {
		t.Fatalf("pagination lost context: count=%d selected=%q expanded=%t", len(model.activity), model.selectedActivity, model.activityExpanded)
	}
	newest := runtime.ActivityProjection{
		ID: "event-3", EventType: "action.completed", Category: "action", Severity: runtime.ActivitySeverityInfo,
		Visibility: runtime.ActivityVisibilityScope, AgentID: "operator", RunID: "run-2",
		Actor: runtime.ActivityActor{Type: "system", ID: "worker"}, Summary: "Restart completed", CreatedAt: now.Add(time.Second),
	}
	fake.activityPages["agent:operator"] = &runtime.ActivityFeedPage{Items: []runtime.ActivityProjection{newest, summary}, HasMore: true, NextCursor: "new-first-page-cursor"}
	applyCommand(t, model, model.loadActivity(false))
	if len(model.activity) != 3 || model.selectedActivity != "event-2" || !model.activityExpanded || model.activityNextCursor != "" {
		t.Fatalf("poll reconciliation count=%d selected=%q expanded=%t cursor=%q", len(model.activity), model.selectedActivity, model.activityExpanded, model.activityNextCursor)
	}
	ids := map[string]int{}
	for _, item := range model.activity {
		ids[item.ID]++
	}
	if ids["event-2"] != 1 || model.activity[0].ID != "event-3" || model.selectedActivityRecord().Payload["approvalId"] != "approval-7" {
		t.Fatalf("poll merge activity=%#v ids=%#v", model.activity, ids)
	}
}

func TestActivityWorkspaceUsesExactTeamSelectorAndFailsClosedOnVersionMismatch(t *testing.T) {
	teamFake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(kernelapi.ActivityCapability()),
		activityPages: map[string]*runtime.ActivityFeedPage{"team:marketing": {Items: []runtime.ActivityProjection{{
			ID: "team-event", EventType: "handoff.completed", TeamID: "marketing", Summary: "Launch brief returned", CreatedAt: time.Now().UTC(),
		}}}},
	}
	config := DefaultConfig()
	config.Owner = runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "marketing"}
	config.PollInterval = -1
	model, err := NewModel(t.Context(), teamFake, config)
	if err != nil {
		t.Fatal(err)
	}
	model.width, model.height = 120, 36
	applyCommand(t, model, model.loadCapabilities())
	if len(teamFake.activityRequests) != 1 || teamFake.activityRequests[0].TeamID != "marketing" || teamFake.activityRequests[0].AgentID != "" || !strings.Contains(model.View(), "Launch brief returned") {
		t.Fatalf("Team Activity request=%#v view=%s", teamFake.activityRequests, model.View())
	}

	mismatched := kernelapi.ActivityCapability()
	mismatched.Version = "future"
	closed := newTestModel(t, &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.AgentRunsCapability(), mismatched)})
	applyCommand(t, closed, closed.loadCapabilities())
	if closed.activityCapability.Available || strings.Contains(closed.View(), "t Activity") {
		t.Fatalf("mismatched Activity capability leaked into TUI:\n%s", closed.View())
	}

	missingDeployment := &fakeKernelClient{
		document:       kernelapi.NewCapabilityDocument(kernelapi.ActivityCapability(), kernelapi.AgentDefinitionsCapability()),
		compilationErr: &client.APIError{StatusCode: 404, Message: "Agent deployment was not found in this scope"},
		activityPages: map[string]*runtime.ActivityFeedPage{"agent:operator": {Items: []runtime.ActivityProjection{{
			ID: "available-activity", EventType: "run.created", AgentID: "operator", Summary: "Standalone work remains observable", CreatedAt: time.Now().UTC(),
		}}}},
	}
	standalone := newTestModel(t, missingDeployment)
	applyCommand(t, standalone, standalone.loadCapabilities())
	if standalone.err != nil || standalone.agentDefinitionCapability.Available || !strings.Contains(standalone.View(), "Standalone work remains observable") || strings.Contains(standalone.View(), "h Agents") {
		t.Fatalf("optional deployment readiness polluted standalone Activity: err=%v\n%s", standalone.err, standalone.View())
	}
}

func TestMismatchedRequestCapabilityFailsClosedWithoutHidingMatchingWork(t *testing.T) {
	document := kernelapi.NewCapabilityDocument(
		kernelapi.AgentRunsCapability(),
		kernelapi.Capability{ID: kernelapi.AgentRequestsCapabilityID, Version: "future", Available: true, Operations: []string{kernelapi.OperationList, kernelapi.OperationRespond}},
	)
	model := newTestModel(t, &fakeKernelClient{document: document})
	applyCommand(t, model, model.loadCapabilities())
	if !model.ready || model.runCapability.Available == false || model.requestCapability.Available || strings.Contains(model.View(), "R Inbox") {
		t.Fatalf("capability drift was not isolated:\n%s", model.View())
	}
}

func TestCreateRetryPreservesIdempotencyAndPrompt(t *testing.T) {
	fake := &fakeKernelClient{
		document:     kernelapi.Capabilities(),
		createErrors: []error{errors.New("temporary disconnect"), nil},
	}
	model := newTestModel(t, fake)
	model.ready = true
	model.runCapability = kernelapi.AgentRunsCapability()
	model.editor.SetValue("Monitor Kubernetes events and remediate safely")

	applyCommand(t, model, model.submitRun())
	if model.editor.Value() == "" || model.pendingKey == "" {
		t.Fatal("failed creation did not preserve prompt and idempotency key")
	}
	applyCommand(t, model, model.submitRun())
	if len(fake.createKeys) != 2 || fake.createKeys[0] == "" || fake.createKeys[0] != fake.createKeys[1] {
		t.Fatalf("idempotency keys = %#v", fake.createKeys)
	}
	if model.editor.Value() != "" || model.pendingKey != "" {
		t.Fatal("successful creation did not clear composer state")
	}
	if fake.createRequests[0].AssignedAgentID != "operator" || fake.createRequests[0].Source != runtime.RunSourceManual {
		t.Fatalf("create request = %#v", fake.createRequests[0])
	}
}

// assertSemanticTextContains ignores presentation whitespace so responsive
// line wrapping cannot turn exact capability facts into brittle test fragments.
func assertSemanticTextContains(t *testing.T, rendered string, expected ...string) {
	t.Helper()
	normalize := func(value string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, value)
	}
	semantic := normalize(rendered)
	for _, value := range expected {
		if !strings.Contains(semantic, normalize(value)) {
			t.Fatalf("rendered view missing semantic text %q:\n%s", value, rendered)
		}
	}
}

func TestObjectivePortfolioCreateAndAmendUsePublicCapability(t *testing.T) {
	fake := &fakeKernelClient{
		document: kernelapi.Capabilities(),
		runbooks: []*runtime.RunbookActivation{{
			ID: "daily-scan", Scope: runtime.Scope{Kind: "local", ID: "default"},
			Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"}, ObjectiveID: "objective-created",
			DefinitionID: "community-scan", DefinitionVersion: "1.0.0", AssignedAgentID: "researcher",
			TriggerID: "daily", Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "scan", Schedule: &runbook.Schedule{Cron: "0 0 9 * * *", Timezone: "UTC"}},
			Status: runtime.RunbookActivationActive, Revision: 1,
		}},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if !model.objectiveCapability.Available || model.section != sectionOverview {
		t.Fatalf("full workspace did not start at Home:\n%s", model.View())
	}
	model.section = sectionObjectives
	model.mode = modeObjectiveCreate
	if !strings.Contains(model.View(), "Objective portfolio") {
		t.Fatalf("objective capability was not rendered:\n%s", model.View())
	}
	model.mode = modeObjectiveCreate
	model.focusComposerEditor()
	model.editor.SetValue("Monitor competitor pain points\nContinuously synthesize cited findings.")
	applyCommand(t, model, model.submitObjective())
	if len(fake.objectiveCreates) != 1 || fake.objectiveKeys[0] == "" || fake.objectiveCreates[0].Title != "Monitor competitor pain points" ||
		fake.objectiveCreates[0].Status != runtime.ObjectiveStatusActive {
		t.Fatalf("objective create = %#v keys=%#v", fake.objectiveCreates, fake.objectiveKeys)
	}
	if model.selectedObjectiveRecord() == nil || model.selectedObjectiveRecord().ID != "objective-created" {
		t.Fatalf("selected objective = %#v", model.selectedObjectiveRecord())
	}
	view := model.View()
	if !strings.Contains(view, "Runbooks · 1") || !strings.Contains(view, "community-scan@1.0.0") ||
		!strings.Contains(view, "Schedule · 0 0 9 * * * · UTC") {
		t.Fatalf("Objective-owned Runbook was not rendered:\n%s", view)
	}
	model.mode = modeObjectiveEdit
	model.focusComposerEditor()
	model.editor.SetValue("Monitor competitor pain points and publish a weekly cited report.")
	applyCommand(t, model, model.submitObjectiveAmendment())
	if len(fake.objectiveUpdates) != 1 || fake.objectiveUpdates[0].ExpectedRevision != 1 ||
		fake.objectiveUpdates[0].Goal == nil || !strings.Contains(*fake.objectiveUpdates[0].Goal, "weekly") {
		t.Fatalf("objective update = %#v", fake.objectiveUpdates)
	}
}

func TestObjectiveRunbookInspectionAndLifecycleUseExactPublicCapability(t *testing.T) {
	scope := runtime.Scope{Kind: "local", ID: "default"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"}
	now := time.Now().UTC()
	objective := &runtime.Objective{
		ID: "objective:research", Scope: scope, Owner: owner, Title: "Research communities", Goal: "Find cited pain points",
		Status: runtime.ObjectiveStatusActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	activation := &runtime.RunbookActivation{
		ID: "runbook:daily-scan", Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: "researcher",
		DefinitionID: "community-scan", DefinitionVersion: "1.0.0", TriggerID: "daily",
		Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "scan", Schedule: &runbook.Schedule{Cron: "0 0 9 * * *", Timezone: "UTC"}},
		Status:  runtime.RunbookActivationActive, Revision: 3, CreatedAt: now, UpdatedAt: now,
	}
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: activation.DefinitionID, Version: activation.DefinitionVersion,
		Name: "Daily community scan", Description: "Collect and summarize relevant posts.",
		Entrypoints: map[string]string{"scan": "search"},
		Steps: map[string]runbook.Step{
			"search": {Kind: runbook.StepAction, Name: "Search posts", Action: &runbook.ActionStep{SkillID: "reddit-search", SkillVersion: "1.2.0", Action: "search", ResultPath: "/posts", Next: "done"}},
			"done":   {Kind: runbook.StepEnd, Name: "Finish", End: &runbook.EndStep{}},
		},
	}
	fake := &fakeKernelClient{
		document:   kernelapi.NewCapabilityDocument(kernelapi.ObjectivesCapability(), kernelapi.RunbooksCapability()),
		objectives: []*runtime.Objective{objective}, runbooks: []*runtime.RunbookActivation{activation},
		runbookDefinitions:   map[string]*runbook.Definition{activation.ID: definition},
		runbookVerifications: map[string]*runbook.VerificationReport{activation.ID: {Valid: true}},
		runbookStartResult:   &runtime.AgentRunCommandResult{Run: &runtime.AgentRun{ID: "run:manual"}},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.focusPanelList()

	_, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	applyCommand(t, model, command)
	view := model.View()
	for _, expected := range []string{"Daily community scan · reviewed 1.0.0", "Verified for activation", "Search posts · reddit-search@1.2.0 · search", "u run now", "p pause"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("Runbook detail missing %q:\n%s", expected, view)
		}
	}

	_, command = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	applyCommand(t, model, command)
	if len(fake.runbookStarts) != 1 || fake.runbookStarts[0].IdempotencyKey == "" || fake.runbookStarts[0].Actor != model.config.Actor || !strings.Contains(model.status, "run:manual") {
		t.Fatalf("Runbook start = %#v status=%q", fake.runbookStarts, model.status)
	}

	_, command = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	applyCommand(t, model, command)
	if len(fake.runbookUpdates) != 1 || fake.runbookUpdates[0].ExpectedRevision != 3 || fake.runbookUpdates[0].Status != runtime.RunbookActivationPaused {
		t.Fatalf("Runbook lifecycle update = %#v", fake.runbookUpdates)
	}
}

func TestObjectiveAndRunViewsProjectAttemptAndDurationBudgets(t *testing.T) {
	scope := runtime.Scope{Kind: "local", ID: "default"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"}
	policy := &runtime.BudgetPolicy{MaxAttempts: 3, MaxTurns: 8, MaxDurationMS: 60000}
	fake := &fakeKernelClient{document: kernelapi.Capabilities(), objectives: []*runtime.Objective{{
		ID: "objective-budget", Scope: scope, Owner: owner, Title: "Bounded research", Goal: "Finish safely",
		Status: runtime.ObjectiveStatusActive, Budget: policy, Revision: 1,
	}}, runs: []*runtime.AgentRun{{
		ID: "run-budget", Scope: scope, Owner: owner, Goal: "Research within policy", Status: runtime.AgentRunStatusPaused,
		Budget: policy, BudgetUsage: runtime.BudgetUsage{Attempts: 2, Turns: 4, DurationMS: 30000}, BudgetState: runtime.BudgetStateWarning, Revision: 2,
	}}}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.section = sectionObjectives
	if view := model.View(); !strings.Contains(view, "attempts 3") || !strings.Contains(view, "duration ms 60000") {
		t.Fatalf("objective budget missing:\n%s", view)
	}
	model.section = sectionRuns
	if view := model.View(); !strings.Contains(view, "Autonomy budget · warning") || !strings.Contains(view, "attempts 2/3") || !strings.Contains(view, "duration ms 30000/60000") {
		t.Fatalf("run budget missing:\n%s", view)
	}
}

func TestClawHubSkillsUseAdvertisedLifecycleAndTypedConfirmation(t *testing.T) {
	capability := kernelapi.ClawHubLifecycleCapability(clawhub.CanonicalLifecycleCapability())
	base := &fakeKernelClient{document: kernelapi.NewCapabilityDocument(capability)}
	fake := &fakeClawHubKernelClient{fakeKernelClient: base, states: []clawhub.InstalledState{{APIVersion: clawhub.LifecycleAPIVersion, SourceIdentity: "source", Reference: clawhub.SkillReference{Owner: "acme", Slug: "research"}, Version: "1.0.0", Verified: true}}}
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.section != sectionSkills || !strings.Contains(model.View(), "Skills and authorized actions") || !strings.Contains(model.View(), "@acme/research@1.0.0") {
		t.Fatalf("Skills surface not rendered:\n%s", model.View())
	}
	model.mode = modeSkillInstall
	model.focusComposerEditor()
	model.editor.SetValue("@other/publisher")
	applyCommand(t, model, model.submitClawHubInstall())
	if len(fake.installs) != 1 || fake.installs[0].Owner != "other" {
		t.Fatalf("installs=%#v", fake.installs)
	}
	model.clawHubSkills = fake.states
	model.restoreClawHubSelection()
	model.mode = modeSkillPin
	model.editor.SetValue("Reviewed production version")
	applyCommand(t, model, model.submitClawHubPin())
	if len(fake.pins) != 1 || fake.pins[0] != "Reviewed production version" {
		t.Fatalf("pins=%#v", fake.pins)
	}
	model.mode = modeSkillRemove
	model.editor.SetValue("remove")
	if command := model.submitClawHubRemoval(); command != nil {
		t.Fatal("case-insensitive destructive confirmation was accepted")
	}
	model.editor.SetValue("REMOVE")
	applyCommand(t, model, model.submitClawHubRemoval())
	if len(fake.uninstalls) != 1 {
		t.Fatalf("uninstalls=%#v", fake.uninstalls)
	}
}

func TestSkillActionsUseOwnerScopedTypedDiscovery(t *testing.T) {
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(kernelapi.SkillActionsCapability()),
		skillActions: []capability.ModelAction{{
			Name: "community.reply", Description: "Reply to a community thread", BindingID: "community-account", BindingRevision: 7,
			SkillID: "community", Version: "1", Action: "reply", Risk: capability.RiskLevelExternal, SideEffect: capability.SideEffectExternal,
			SemanticArguments: map[string]string{"target": "thread", "body": "message"},
		}},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.section != sectionSkills || fake.skillActionCalls != 1 {
		t.Fatalf("section=%v calls=%d", model.section, fake.skillActionCalls)
	}
	view := model.View()
	for _, expected := range []string{"Authorized for this Agent", "community@1/reply", "community-account@7", "target → thread", "body → message"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("authorized action missing %q:\n%s", expected, view)
		}
	}
}

func TestSkillActionsRenderTruthfulEmptyState(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.SkillActionsCapability())}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	view := model.View()
	if !strings.Contains(view, "No executable action is bound to this Agent") || strings.Contains(view, "Press n") {
		t.Fatalf("empty action state is not truthful:\n%s", view)
	}
}

func TestClawHubTUIHidesUnadvertisedMutations(t *testing.T) {
	lifecycle := clawhub.CanonicalLifecycleCapability()
	lifecycle.Operations = []clawhub.LifecycleOperation{clawhub.LifecycleInspectInstalled}
	fake := &fakeClawHubKernelClient{fakeKernelClient: &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.ClawHubLifecycleCapability(lifecycle))}, states: []clawhub.InstalledState{{SourceIdentity: "source", Reference: clawhub.SkillReference{Slug: "read-only"}, Version: "1"}}}
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	view := model.View()
	for _, control := range []string{"u update", "p pin", "x remove", "n install"} {
		if strings.Contains(view, control) {
			t.Fatalf("rendered unadvertised %q:\n%s", control, view)
		}
	}
}

func TestLifecycleCommandUsesSelectedRevisionAndAdvertisedOperation(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.Capabilities()}
	model := newTestModel(t, fake)
	model.ready = true
	model.runCapability = kernelapi.AgentRunsCapability()
	model.runs = []*runtime.AgentRun{testRun("run-7", runtime.AgentRunStatusRunning, 7)}
	model.selectedID = "run-7"

	applyCommand(t, model, model.pauseOrResume())
	if len(fake.commands) != 1 {
		t.Fatalf("commands = %#v", fake.commands)
	}
	command := fake.commands[0]
	if command.Kind != runtime.AgentRunCommandPause || command.ExpectedRevision != 7 {
		t.Fatalf("command = %#v", command)
	}

	model.runCapability.Operations = []string{kernelapi.OperationList}
	if command := model.commandSelected(runtime.AgentRunCommandCancel, ""); command != nil {
		t.Fatal("TUI dispatched an unadvertised cancel operation")
	}
}

func TestRunViewProjectsCanonicalDeliveryReceipt(t *testing.T) {
	model := newTestModel(t, &fakeKernelClient{document: kernelapi.Capabilities()})
	model.ready = true
	model.runCapability = kernelapi.AgentRunsCapability()
	run := testRun("delivery-run", runtime.AgentRunStatusCompleted, 4)
	run.Output = map[string]interface{}{
		"receiptId": "smtp:action-1", "status": "accepted", "recipientCount": float64(1),
		"deliveredAt":  "2026-07-13T00:19:22Z",
		"artifactRefs": []interface{}{map[string]interface{}{"id": "pdf-report", "version": float64(3)}},
	}
	run.Context = map[string]interface{}{"capabilityInvocation": map[string]interface{}{"inputs": map[string]interface{}{"to": []interface{}{"reviewer@example.com", "operator@example.com"}}}}
	model.runs = []*runtime.AgentRun{run}
	model.section = sectionRuns

	view := model.renderRunsContent(100)
	for _, expected := range []string{"DELIVERY ACCEPTED", "1 recipient · 1 artifact · example.com", "smtp:action-1", "pdf-report · version 3"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("delivery receipt missing %q:\n%s", expected, view)
		}
	}
}

func TestRunViewLoadsAndProjectsCanonicalActionCalls(t *testing.T) {
	run := testRun("run-actions", runtime.AgentRunStatusCompleted, 4)
	fake := &fakeKernelClient{
		document: kernelapi.Capabilities(),
		runs:     []*runtime.AgentRun{run},
		actionCalls: []*runtime.ActionCall{{
			ID: "call-1", RunID: run.ID, SkillID: "openseal.source", SkillVersion: "1.0.2", Action: "observe_feed",
			BindingID: "source-prod", BindingRevision: 9, Status: runtime.ActionCallStatusSucceeded,
			Attempt: 1, MaxAttempts: 3, Risk: "read", SideEffect: "none",
			Arguments:      map[string]interface{}{"token": "must-not-render"},
			CredentialRefs: map[string]skill.CredentialReference{"token": {Kind: "opaque", ID: "must-not-render"}},
		}},
	}
	model := newTestModel(t, fake)
	model.ready = true
	model.section = sectionRuns
	model.runCapability = kernelapi.AgentRunsCapability()
	model.actionCallCapability = kernelapi.ActionCallsCapability()
	model.runs = fake.runs
	model.selectedID = run.ID

	applyCommand(t, model, model.loadSelectedActionCalls())

	if len(fake.actionCallFilters) != 1 || fake.actionCallFilters[0].RunID != run.ID || fake.actionCallFilters[0].Limit != 100 {
		t.Fatalf("action call filters = %#v", fake.actionCallFilters)
	}
	view := model.renderRunsContent(140)
	for _, expected := range []string{"Skill executions", "succeeded", "openseal.source@1.0.2/observe_feed", "source-prod@9", "attempt 1/3", "read/none", "call call-1"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("ActionCall projection missing %q:\n%s", expected, view)
		}
	}
	if strings.Contains(view, "must-not-render") || strings.Contains(strings.ToLower(view), "credential") {
		t.Fatalf("ActionCall projection exposed private invocation state:\n%s", view)
	}
}

func TestRunViewLoadsAndProjectsAgentSteps(t *testing.T) {
	run := testRun("run-steps", runtime.AgentRunStatusCompleted, 4)
	fake := &fakeKernelClient{
		document: kernelapi.Capabilities(),
		runs:     []*runtime.AgentRun{run},
		agentTurns: []*kernelapi.AgentTurnRecord{
			{
				ID: "step-1", RunID: run.ID, Sequence: 1, Status: runtime.AgentTurnStatusCompleted,
				ModelProvider: "openai-compatible", Model: "deepseek-v4-flash",
				OutputSummary: "Asking the reviewer to check the evidence.",
				ToolCallCount: 1, StopReason: "tool_calls",
				Usage: runtime.TurnUsage{InputTokens: 1840, OutputTokens: 64, DurationMS: 910},
			},
			{
				ID: "step-2", RunID: run.ID, Sequence: 2, Status: runtime.AgentTurnStatusFailed,
				ModelProvider: "openai-compatible", Model: "deepseek-v4-flash",
				FailureCode: "model_context_exceeded", Error: "the conversation no longer fits the model",
			},
		},
		actionCalls: []*runtime.ActionCall{
			{ID: "call-1", RunID: run.ID, TurnID: "step-1", SkillID: "openseal.source", SkillVersion: "3", Action: "observe_feed", Status: runtime.ActionCallStatusSucceeded},
			{ID: "call-other", RunID: run.ID, TurnID: "step-other", SkillID: "openseal.source", SkillVersion: "3", Action: "must_not_join", Status: runtime.ActionCallStatusSucceeded},
		},
	}
	model := newTestModel(t, fake)
	model.ready = true
	model.section = sectionRuns
	model.runCapability = kernelapi.AgentRunsCapability()
	model.agentTurnCapability = kernelapi.AgentTurnsCapability()
	model.actionCallCapability = kernelapi.ActionCallsCapability()
	model.runs = fake.runs
	model.selectedID = run.ID

	applyCommand(t, model, model.loadSelectedAgentTurns())
	applyCommand(t, model, model.loadSelectedActionCalls())

	if len(fake.agentTurnFilters) != 1 || fake.agentTurnFilters[0].RunID != run.ID || fake.agentTurnFilters[0].Limit != 100 {
		t.Fatalf("agent turn filters = %#v", fake.agentTurnFilters)
	}
	steps := strings.Join(model.renderSelectedAgentTurns(160), "\n")
	for _, expected := range []string{
		"Agent steps",
		"step 1", "completed", "openai-compatible/deepseek-v4-flash · 1 tool call",
		"Asking the reviewer to check the evidence.",
		"→ openseal.source@3/observe_feed · succeeded",
		"1840 input · 64 output tokens · 910ms",
		"step 2", "failed", "model_context_exceeded · the conversation no longer fits the model",
	} {
		if !strings.Contains(steps, expected) {
			t.Fatalf("step projection missing %q:\n%s", expected, steps)
		}
	}
	for _, unexpected := range []string{"Agent turns", "turn 1", "must_not_join", "0 tool call", "tool calls"} {
		if strings.Contains(steps, unexpected) {
			t.Fatalf("step projection contains %q:\n%s", unexpected, steps)
		}
	}
	if strings.Count(steps, "→") != 1 {
		t.Fatalf("step projection joined calls of other steps:\n%s", steps)
	}

	model.agentTurns = nil
	if view := strings.Join(model.renderSelectedAgentTurns(160), "\n"); !strings.Contains(view, "No agent steps recorded for this work.") {
		t.Fatalf("empty step history copy missing:\n%s", view)
	}
	model.agentTurnsRunID = "another-run"
	if view := strings.Join(model.renderSelectedAgentTurns(160), "\n"); !strings.Contains(view, "Step history has not loaded yet.") {
		t.Fatalf("unloaded step history copy missing:\n%s", view)
	}
}

func TestBudgetLinesLabelModelSteps(t *testing.T) {
	policy := &runtime.BudgetPolicy{MaxTurns: 64, MaxActions: 10}
	lines := strings.Join(renderBudgetLines(policy, &runtime.BudgetUsage{Turns: 3, Actions: 1}, 120), "\n")
	if !strings.Contains(lines, "steps 3/64") || strings.Contains(lines, "turns") {
		t.Fatalf("budget lines do not label model steps: %s", lines)
	}
	if lines = strings.Join(renderBudgetLines(policy, nil, 120), "\n"); !strings.Contains(lines, "steps 64") {
		t.Fatalf("budget limits do not label model steps: %s", lines)
	}
}

func TestRunViewDoesNotLoadAgentTurnsWithoutCapability(t *testing.T) {
	fake := &fakeKernelClient{runs: []*runtime.AgentRun{testRun("run-no-turns", runtime.AgentRunStatusRunning, 1)}}
	model := newTestModel(t, fake)
	model.ready = true
	model.runCapability = kernelapi.AgentRunsCapability()
	model.runs = fake.runs

	if command := model.loadSelectedAgentTurns(); command != nil {
		t.Fatal("TUI loaded Agent turns without an advertised capability")
	}
	if len(fake.agentTurnFilters) != 0 || strings.Contains(model.renderRunsContent(100), "Agent steps") {
		t.Fatalf("unsupported Agent turn surface was not hidden: filters=%#v\n%s", fake.agentTurnFilters, model.renderRunsContent(100))
	}
}

func TestRunViewDoesNotLoadActionCallsWithoutCapability(t *testing.T) {
	fake := &fakeKernelClient{runs: []*runtime.AgentRun{testRun("run-no-actions", runtime.AgentRunStatusRunning, 1)}}
	model := newTestModel(t, fake)
	model.ready = true
	model.runCapability = kernelapi.AgentRunsCapability()
	model.runs = fake.runs

	if command := model.loadSelectedActionCalls(); command != nil {
		t.Fatal("TUI loaded ActionCalls without an advertised capability")
	}
	if len(fake.actionCallFilters) != 0 || strings.Contains(model.renderRunsContent(100), "Skill executions") {
		t.Fatalf("unsupported ActionCall surface was not hidden: filters=%#v\n%s", fake.actionCallFilters, model.renderRunsContent(100))
	}
}

func TestRunViewProjectsCanonicalParentLineage(t *testing.T) {
	model := newTestModel(t, &fakeKernelClient{document: kernelapi.Capabilities()})
	model.ready = true
	model.runCapability = kernelapi.AgentRunsCapability()
	child := testRun("child-run", runtime.AgentRunStatusCompleted, 4)
	child.ParentRunID = "parent-run"
	model.runs = []*runtime.AgentRun{child}
	model.section = sectionRuns

	view := model.renderRunsContent(100)
	if !strings.Contains(view, "Parent Run parent-run") {
		t.Fatalf("parent lineage missing:\n%s", view)
	}

	child.ParentRunID = ""
	if view = model.renderRunsContent(100); strings.Contains(view, "Parent Run") {
		t.Fatalf("root Run rendered false parent lineage:\n%s", view)
	}
}

func applyKey(t *testing.T, model *Model, key string) {
	t.Helper()
	message := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	updated, command := model.Update(message)
	if updated != model || command != nil {
		t.Fatalf("key %q unexpectedly replaced model or dispatched command", key)
	}
}

func TestContractMismatchFailsClosed(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.CapabilityDocument{APIVersion: "agent-kernel/v99"}}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.ready || !strings.Contains(model.View(), "Capability unavailable") {
		t.Fatalf("mismatch did not fail closed:\n%s", model.View())
	}
}

func TestArtifactEvidenceAndVerifiedDownloadAreCapabilityGated(t *testing.T) {
	body := "cited market research"
	confidence := 0.91
	artifact := &runtime.Artifact{
		ID: "research-report", Version: 2, Name: "OpenClaw research.pdf", Type: "report",
		MediaType: "application/pdf", Digest: testDigest(body), SizeBytes: int64(len(body)),
		Classification: runtime.ArtifactClassificationInternal, ContentAvailability: runtime.ArtifactContentAvailable, CreatedAt: time.Now(),
		Provenance: runtime.ArtifactProvenance{
			Producer: runtime.ActivityActor{Type: "agent", ID: "researcher"}, RunID: "run-research",
		},
		Evidence: []runtime.EvidenceLink{{
			Relation: runtime.EvidenceRelationCites, TargetKind: runtime.EvidenceTargetExternalSource,
			TargetRef: "https://example.com/source", Summary: "Primary source", Confidence: &confidence,
		}},
	}
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(
			kernelapi.AgentRunsCapability(), kernelapi.ArtifactCapability(kernelapi.OperationDownload),
		),
		artifacts: []*runtime.Artifact{artifact}, downloadBody: body,
	}
	model := newTestModel(t, fake)
	model.config.DownloadDir = t.TempDir()
	applyCommand(t, model, model.loadCapabilities())
	model.section = sectionArtifacts
	model.focusPanelList()

	view := model.View()
	for _, expected := range []string{"Artifacts & evidence", "OpenClaw research.pdf", "1 evidence link(s)", "d download"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("artifact view missing %q:\n%s", expected, view)
		}
	}
	model.artifactExpanded = true
	if view = model.View(); !strings.Contains(view, "Primary source") || !strings.Contains(view, "91%") {
		t.Fatalf("expanded evidence missing provenance details:\n%s", view)
	}

	applyCommand(t, model, model.downloadSelectedArtifact())
	entries, err := os.ReadDir(model.config.DownloadDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("download entries = %v, err = %v", entries, err)
	}
	downloaded, err := os.ReadFile(model.config.DownloadDir + "/" + entries[0].Name())
	if err != nil || string(downloaded) != body || fake.downloadCalls != 1 {
		t.Fatalf("downloaded = %q, calls = %d, err = %v", downloaded, fake.downloadCalls, err)
	}

	model.artifactCapability = kernelapi.ArtifactCapability()
	if command := model.downloadSelectedArtifact(); command != nil {
		t.Fatal("TUI dispatched an unadvertised artifact download")
	}
	if strings.Contains(model.View(), "d download") {
		t.Fatal("TUI rendered an unadvertised artifact download")
	}
}

func TestUnavailableArtifactContentCannotRenderOrDispatchDownload(t *testing.T) {
	artifact := &runtime.Artifact{
		ID: "pending-report", Version: 1, Name: "pending-report.pdf", Type: "report",
		ContentAvailability: runtime.ArtifactContentUnavailable,
	}
	fake := &fakeKernelClient{
		document:  kernelapi.NewCapabilityDocument(kernelapi.AgentRunsCapability(), kernelapi.ArtifactCapability(kernelapi.OperationDownload)),
		artifacts: []*runtime.Artifact{artifact},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.section = sectionArtifacts
	model.focusPanelList()

	if command := model.downloadSelectedArtifact(); command != nil {
		t.Fatal("TUI dispatched download for unavailable artifact content")
	}
	if strings.Contains(model.View(), "d download") {
		t.Fatal("TUI rendered download for unavailable artifact content")
	}
}

func TestTeamChannelsRequireAdvertisedCapabilityAndConcreteClient(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.ChannelsCapability(kernelapi.ChannelCapabilityFeatures{Coordination: true, Changes: true}))}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.ready || strings.Contains(model.View(), "c Channels") {
		t.Fatalf("channel controls rendered without a channel client:\n%s", model.View())
	}
}

func TestTeamChannelProjectionShowsMessagesPresenceAndRoutingAudit(t *testing.T) {
	conversation := testConversation("release", "release-coordination", 2, 3)
	question := &runtime.ChannelMessage{
		ID: "question", Scope: conversation.Scope, ConversationID: conversation.ID, Sequence: 1,
		Sender: runtime.ConversationParticipant{Type: runtime.ConversationParticipantUser, ID: "local"},
		Intent: runtime.MessageIntentQuestion, Content: "Is the release ready?",
		Audience: runtime.ConversationAudience{Kind: runtime.ConversationAudienceChannel}, CreatedAt: time.Now().Add(-time.Minute),
	}
	answer := &runtime.ChannelMessage{
		ID: "answer", Scope: conversation.Scope, ConversationID: conversation.ID, Sequence: 2,
		Sender: runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: "developer"},
		Intent: runtime.MessageIntentAnswer, Content: "The canary is healthy.",
		Audience: runtime.ConversationAudience{Kind: runtime.ConversationAudienceChannel}, CreatedAt: time.Now(),
	}
	round := &runtime.ParticipationRoundResult{Round: &runtime.ParticipationRound{
		ID: "round-1", CommittedAt: time.Now(), Responders: []runtime.ConversationResponder{{
			Participant: runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: "developer"},
			Addressed:   runtime.ConversationAddressedRouted, Reason: "owns the canary",
		}},
	}}
	fake := &fakeChannelKernelClient{
		fakeKernelClient: &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.ChannelsCapability(kernelapi.ChannelCapabilityFeatures{Coordination: true, Changes: true}))},
		conversations:    []*runtime.Conversation{conversation}, messages: []*runtime.ChannelMessage{answer, question},
		rounds: []*runtime.ParticipationRoundResult{round},
		presence: []*runtime.ConversationPresence{{
			Participant: runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: "sre"},
			State:       runtime.ConversationPresenceWorking, Summary: "checking rollout", ExpiresAt: time.Now().Add(time.Minute),
		}},
	}
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	view := model.View()
	for _, expected := range []string{"c Channels", "release-coordination", "Is the release ready?", "The canary is healthy.", "sre is working", "1 participation round(s)"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("channel view missing %q:\n%s", expected, view)
		}
	}
	if len(fake.cursorAdvances) != 1 || fake.cursorAdvances[0].ReadSequence != 2 {
		t.Fatalf("cursor advances = %#v", fake.cursorAdvances)
	}
	model.channelAuditExpanded = true
	view = model.View()
	for _, expected := range []string{"agent:developer", "routed", "owns the canary"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("channel audit missing %q:\n%s", expected, view)
		}
	}
}

func TestBackgroundChannelRefreshDoesNotInventReadReceipt(t *testing.T) {
	conversation := testConversation("release", "release-coordination", 1, 2)
	fake := &fakeChannelKernelClient{
		fakeKernelClient: &fakeKernelClient{document: kernelapi.NewCapabilityDocument(
			kernelapi.AgentRunsCapability(), kernelapi.ChannelsCapability(kernelapi.ChannelCapabilityFeatures{Coordination: true, Changes: true}),
		)},
		conversations: []*runtime.Conversation{conversation},
		messages: []*runtime.ChannelMessage{{
			ID: "unread", Scope: conversation.Scope, ConversationID: conversation.ID, Sequence: 1,
			Sender: runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: "developer"},
			Intent: runtime.MessageIntentUpdate, Content: "Canary is ready.",
			Audience: runtime.ConversationAudience{Kind: runtime.ConversationAudienceChannel}, CreatedAt: time.Now(),
		}},
	}
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.section != sectionRuns || len(fake.cursorAdvances) != 0 {
		t.Fatalf("background refresh marked channel read: section=%v advances=%#v", model.section, fake.cursorAdvances)
	}
	model.section = sectionChannels
	applyCommand(t, model, model.loadSelectedConversation())
	if len(fake.cursorAdvances) != 1 || fake.cursorAdvances[0].ReadSequence != 1 {
		t.Fatalf("visible channel did not advance read state: %#v", fake.cursorAdvances)
	}
}

func TestTeamChannelCreateAndQuestionPostPreserveIdempotency(t *testing.T) {
	conversation := testConversation("research", "market-research", 0, 4)
	fake := &fakeChannelKernelClient{
		fakeKernelClient:         &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.ChannelsCapability(kernelapi.ChannelCapabilityFeatures{Coordination: true, Changes: true}))},
		conversations:            []*runtime.Conversation{conversation},
		createConversationErrors: []error{errors.New("temporary disconnect"), nil},
		postErrors:               []error{errors.New("revision conflict"), nil},
	}
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())

	model.mode = modeChannelCreate
	model.focusComposerEditor()
	model.editor.SetValue("launch-planning")
	applyCommand(t, model, model.submitConversation())
	applyCommand(t, model, model.submitConversation())
	if len(fake.createConversationKeys) != 2 || fake.createConversationKeys[0] == "" || fake.createConversationKeys[0] != fake.createConversationKeys[1] {
		t.Fatalf("conversation keys = %#v", fake.createConversationKeys)
	}
	if fake.createConversationRequests[0].Owner != model.config.Owner {
		t.Fatalf("conversation owner = %#v", fake.createConversationRequests[0].Owner)
	}

	model.selectedConversation = conversation.ID
	model.restoreConversationSelection()
	model.mode = modeChannelPost
	model.focusComposerEditor()
	model.editor.SetValue("Should we publish the cited report?")
	applyCommand(t, model, model.submitChannelMessage())
	applyCommand(t, model, model.submitChannelMessage())
	if len(fake.postKeys) != 2 || fake.postKeys[0] == "" || fake.postKeys[0] != fake.postKeys[1] {
		t.Fatalf("message keys = %#v", fake.postKeys)
	}
	request := fake.postRequests[0]
	if request.ExpectedRevision != 4 || request.Intent != runtime.MessageIntentQuestion || !request.RequiresResponse || request.Sender.Type != runtime.ConversationParticipantUser {
		t.Fatalf("post request = %#v", request)
	}
}

func TestTeamChannelTUIUsesPublicHTTPKernelBoundary(t *testing.T) {
	store := runtime.NewMemoryStore()
	api := server.NewServer(store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()

	config := DefaultConfig()
	config.Endpoint = httpServer.URL
	config.Scope = runtime.Scope{Kind: "tenant", ID: "one"}
	config.Owner = runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "engineering"}
	config.PollInterval = -1
	httpClient := client.NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	model, err := NewModel(context.Background(), httpClient, config)
	if err != nil {
		t.Fatal(err)
	}
	model.width, model.height = 120, 36
	applyCommand(t, model, model.loadCapabilities())

	model.mode = modeChannelCreate
	model.focusComposerEditor()
	model.editor.SetValue("release-room")
	applyCommand(t, model, model.submitConversation())
	if model.section != sectionChannels || model.selectedConversationRecord() == nil {
		t.Fatalf("created channel was not selected: %#v", model.conversations)
	}

	model.mode = modeChannelPost
	model.focusComposerEditor()
	model.editor.SetValue("Is the canary healthy?")
	applyCommand(t, model, model.submitChannelMessage())
	view := model.View()
	if !strings.Contains(view, "release-room") || !strings.Contains(view, "Is the canary healthy?") {
		t.Fatalf("HTTP-backed channel not rendered:\n%s", view)
	}
	conversation := model.selectedConversationRecord()
	messages, err := httpClient.ListChannelMessages(context.Background(), runtime.ChannelMessageFilter{
		Scope: config.Scope, ConversationID: conversation.ID,
	})
	if err != nil || len(messages) != 1 || messages[0].Intent != runtime.MessageIntentQuestion || !messages[0].RequiresResponse {
		t.Fatalf("durable HTTP messages = %#v, err = %v", messages, err)
	}
}

func TestAgentRequestTUICompletesLifecycleThroughPublicHTTPKernelBoundary(t *testing.T) {
	store := runtime.NewMemoryStore()
	scope := runtime.Scope{Kind: "tenant", ID: "one"}
	source, err := runtime.NewPortfolioService(store).CreateAgentRun(t.Context(), runtime.CreateAgentRunRequest{
		Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship the release", Source: runtime.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := server.NewServer(store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	httpClient := client.NewKernelHTTPClient(httpServer.URL, httpServer.Client())

	developerConfig := DefaultConfig()
	developerConfig.Endpoint, developerConfig.Scope = httpServer.URL, scope
	developerConfig.Owner = runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "developer"}
	developerConfig.PollInterval = -1
	developer, err := NewModel(t.Context(), httpClient, developerConfig)
	if err != nil {
		t.Fatal(err)
	}
	developer.width, developer.height = 120, 36
	applyCommand(t, developer, developer.loadCapabilities())
	if developer.selectedRun() == nil || developer.selectedRun().ID != source.ID {
		t.Fatalf("developer source Run not loaded: %#v", developer.runs)
	}
	developer.section, developer.mode = sectionRequests, modeRequestCreate
	developer.focusComposerEditor()
	developer.editor.SetValue("agent:marketing\nTurn the release into a reviewed launch brief")
	applyCommand(t, developer, developer.submitAgentRequestCreation())
	if developer.selectedAgentRequestRecord() == nil || developer.selectedAgentRequestRecord().Status != runtime.AgentRequestStatusPending {
		t.Fatalf("HTTP request was not created: %#v", developer.agentRequests)
	}
	requestID := developer.selectedAgentRequestRecord().ID

	marketingConfig := developerConfig
	marketingConfig.Owner = runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "marketing"}
	marketing, err := NewModel(t.Context(), httpClient, marketingConfig)
	if err != nil {
		t.Fatal(err)
	}
	marketing.width, marketing.height = 120, 36
	applyCommand(t, marketing, marketing.loadCapabilities())
	marketing.section = sectionRequests
	if marketing.selectedAgentRequestRecord() == nil || marketing.selectedAgentRequestRecord().ID != requestID || !strings.Contains(marketing.View(), "Turn the release into a reviewed launch brief") {
		t.Fatalf("recipient did not discover HTTP request:\n%s", marketing.View())
	}
	marketing.mode = modeRequestAccept
	marketing.focusComposerEditor()
	marketing.editor.SetValue("I will produce the brief")
	applyCommand(t, marketing, marketing.submitAgentRequestResponse(runtime.AgentRequestDecisionAccept))
	accepted := marketing.selectedAgentRequestRecord()
	if accepted == nil || accepted.Status != runtime.AgentRequestStatusAccepted || accepted.ChildRunID == "" || marketing.selectedRun() == nil || marketing.selectedRun().ID != accepted.ChildRunID {
		t.Fatalf("HTTP request was not accepted with child work: request=%#v runs=%#v", accepted, marketing.runs)
	}
	marketing.mode = modeRequestComplete
	marketing.focusComposerEditor()
	marketing.editor.SetValue("Reviewed launch brief delivered")
	applyCommand(t, marketing, marketing.submitAgentRequestCompletion())
	completed := marketing.selectedAgentRequestRecord()
	if completed == nil || completed.Status != runtime.AgentRequestStatusCompleted || completed.CompletionSummary != "Reviewed launch brief delivered" {
		t.Fatalf("HTTP request was not completed: %#v", completed)
	}
	restoredSource, err := httpClient.GetAgentRun(t.Context(), scope, source.ID)
	if err != nil || restoredSource.Status != runtime.AgentRunStatusQueued || restoredSource.WakeCondition != nil {
		t.Fatalf("source Run did not wake: %#v, %v", restoredSource, err)
	}
}

func TestActionApprovalTUIResolvesThroughGovernedHTTPKernelBoundary(t *testing.T) {
	store := runtime.NewMemoryStore()
	scope := runtime.Scope{Kind: "tenant", ID: "one"}
	now := time.Now().UTC()
	run, err := runtime.NewPortfolioService(store).CreateAgentRun(t.Context(), runtime.CreateAgentRunRequest{
		Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"}, AssignedAgentID: "operator",
		Goal: "Publish the release", Source: runtime.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The run loop proposes the governed call from a leased, running step and
	// parks the Run on its approval at step end.
	const worker = "loop-worker"
	runEvent := func(id string) *runtime.ActivityEvent {
		return &runtime.ActivityEvent{
			ID: id, Scope: scope, RunID: run.ID, EventType: "run.updated", Severity: runtime.ActivitySeverityInfo,
			Actor: runtime.ActivityActor{Type: "worker", ID: worker}, Summary: "Run loop step", Visibility: runtime.ActivityVisibilityScope, CreatedAt: now,
		}
	}
	leased := *run
	expires := now.Add(time.Hour)
	leased.Status, leased.LeaseOwner, leased.LeaseExpiresAt = runtime.AgentRunStatusRunning, worker, &expires
	leased.Revision++
	leased.UpdatedAt = now
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), &leased, run.Revision, runEvent("event-lease"), nil); err != nil {
		t.Fatal(err)
	}
	approval := &runtime.ApprovalCheckpoint{
		ID: "approval-publish", Scope: scope, RunID: run.ID, ActionCallID: "call-publish", Status: runtime.ApprovalStatusPending,
		Risk: "production", Summary: "Publish release announcement", PolicyReason: "External publication requires review",
		ProposedAction:    map[string]interface{}{"skillId": "publisher", "skillVersion": "1", "action": "publish"},
		EligibleApprovers: []runtime.ApprovalPrincipal{{Type: "user", ID: "local"}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call := &runtime.ActionCall{
		ID: "call-publish", Scope: scope, RunID: run.ID, TurnID: "step-1", DeploymentID: "operator", SkillID: "publisher", SkillVersion: "1", Action: "publish",
		Origin: runtime.ActionOriginLoop, Execution: runtime.ActionExecutionLoop,
		Status: runtime.ActionCallStatusWaitingApproval, Risk: "production", SideEffect: "external", IdempotencyKey: "publish-release",
		ApprovalID: approval.ID, MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call.InvocationDigest = runtime.ComputeActionInvocationDigest(call)
	if _, err := store.CreateActionProposal(t.Context(), runtime.ActionProposalRecord{
		Call: call, Approval: approval, ExpectedRunRevision: leased.Revision, Lease: &runtime.AgentRunLeaseGuard{WorkerID: worker, Now: now},
		Event: &runtime.ActivityEvent{ID: "event-approval", Scope: scope, RunID: run.ID, EventType: "action.approval_requested", Summary: "Approval requested", CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	parked := leased
	parked.Status = runtime.AgentRunStatusWaitingForApproval
	parked.WakeCondition = &runtime.WakeCondition{Type: "approval", Reference: approval.ID}
	parked.LeaseOwner, parked.LeaseExpiresAt = "", nil
	parked.Revision++
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), &parked, leased.Revision, runEvent("event-park"), nil); err != nil {
		t.Fatal(err)
	}
	api := server.NewServer(store, zap.NewNop().Sugar())
	api.SetActionApprovalAuthorizer(runtime.ApprovalAuthorizerFunc(func(_ context.Context, principal runtime.ApprovalPrincipal, checkpoint *runtime.ApprovalCheckpoint) error {
		if principal != (runtime.ApprovalPrincipal{Type: "user", ID: "local"}) || checkpoint.ID != approval.ID {
			return errors.New("not authorized")
		}
		return nil
	}))
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	httpClient := client.NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	config := DefaultConfig()
	config.Endpoint, config.Scope = httpServer.URL, scope
	config.PollInterval = -1
	model, err := NewModel(t.Context(), httpClient, config)
	if err != nil {
		t.Fatal(err)
	}
	model.width, model.height = 120, 36
	applyCommand(t, model, model.loadCapabilities())
	model.section = sectionApprovals
	if model.selectedActionApprovalRecord() == nil || !model.canResolveSelectedActionApproval() || !strings.Contains(model.View(), "External publication requires review") {
		t.Fatalf("governed approval was not projected:\n%s", model.View())
	}
	model.mode = modeApprovalApprove
	model.focusComposerEditor()
	model.editor.SetValue("Release evidence and publication window reviewed")
	applyCommand(t, model, model.submitActionApproval(true))
	resolved := model.selectedActionApprovalRecord()
	if resolved == nil || resolved.Status != runtime.ApprovalStatusApproved || resolved.DecisionBy == nil || resolved.DecisionBy.ID != "local" {
		t.Fatalf("HTTP approval was not resolved: %#v", resolved)
	}
	restoredRun, err := httpClient.GetAgentRun(t.Context(), scope, run.ID)
	// The approved loop call is ready; the Run is queued to resume its step's tool entry.
	if err != nil || restoredRun.Status != runtime.AgentRunStatusQueued || restoredRun.WakeCondition != nil {
		t.Fatalf("approved Run was not queued to resume its loop: %#v, %v", restoredRun, err)
	}
}

func TestActivityWorkspaceFlowsThroughPublicHTTPKernelBoundary(t *testing.T) {
	store := runtime.NewMemoryStore()
	scope := runtime.Scope{Kind: "local", ID: "activity-http"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"}
	portfolio := runtime.NewPortfolioService(store)
	run, err := portfolio.CreateAgentRun(t.Context(), runtime.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Verify durable Activity", Source: runtime.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.NewRunActivityService(store, store).AppendActivity(t.Context(), &runtime.ActivityEvent{
		ID: "activity-http-evidence", Scope: scope, RunID: run.ID, AgentID: owner.ID,
		EventType: "action.completed", Severity: runtime.ActivitySeverityInfo, Visibility: runtime.ActivityVisibilityScope,
		Actor: runtime.ActivityActor{Type: "system", ID: "worker"}, Summary: "Evidence persisted through the public boundary",
		Payload: map[string]interface{}{"artifactId": "report-pdf", "outcome": "verified"}, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	api := server.NewServer(store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	httpClient := client.NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	config := DefaultConfig()
	config.Endpoint, config.Scope, config.Owner, config.PollInterval = httpServer.URL, scope, owner, -1
	model, err := NewModel(t.Context(), httpClient, config)
	if err != nil {
		t.Fatal(err)
	}
	model.width, model.height = 120, 36
	applyCommand(t, model, model.loadCapabilities())
	model.section = sectionActivity
	if len(model.activity) != 1 || model.activity[0].ID != "activity-http-evidence" || model.activity[0].Payload != nil {
		t.Fatalf("summary-first HTTP activity=%#v", model.activity)
	}
	applyCommand(t, model, model.loadSelectedActivityDetail())
	if !model.activityExpanded || model.selectedActivityRecord().Payload["artifactId"] != "report-pdf" || !strings.Contains(model.View(), "Evidence persisted through the public boundary") {
		t.Fatalf("expanded HTTP Activity was not projected:\n%s", model.View())
	}
}

func newTestModel(t *testing.T, fake *fakeKernelClient) *Model {
	return newModelWithClient(t, fake)
}

func TestRuntimeReadinessRendersLatestCompilationTruth(t *testing.T) {
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(kernelapi.AgentDefinitionsCapability()),
		agentDeployments: []kernelapi.AgentDeploymentCatalogEntry{{
			Deployment: &kernelagent.AgentDeployment{ID: "operator", Scope: capability.ScopeReference{Kind: "local", ID: "default"}, DefinitionID: "operator", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive, Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1},
			Definition: &kernelagent.AgentDefinition{ID: "operator", Version: "1", DisplayName: "Operator", Purpose: "Operate safely"},
		}},
		compilations: []*kernelagent.DefinitionCompilation{{
			ID: "failed", Scope: capability.ScopeReference{Kind: "local", ID: "default"}, DeploymentID: "operator", DefinitionID: "operator", CandidateVersion: "source-2",
			Source: kernelagent.CompilationSource{Kind: "visual_graph", ID: "source", Version: "2", Digest: testDigest("source")}, Status: kernelagent.CompilationFailed,
			Diagnostics: []kernelagent.CompilationDiagnostic{{NodeID: "reddit", Path: "nodes.reddit", Code: "action.unavailable", Message: "Reddit action is unavailable"}}, CreatedAt: time.Now(),
		}},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.section = sectionReadiness
	view := model.View()
	for _, expected := range []string{"Agent runtime & placement", "Native runtime readiness", "Operator", "active · definition operator@1 · revision 1", "NEEDS ATTENTION", "Reddit action is unavailable", "Node reddit", "candidate was not activated", "p pause/resume"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("readiness view missing %q:\n%s", expected, view)
		}
	}
}

func TestRuntimeReadinessStartsExactTypedActiveRunbookOperation(t *testing.T) {
	entry := testActiveRunbookAgentEntry()
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(
			kernelapi.AgentRunsCapability(kernelapi.OperationCreate),
			kernelapi.AgentDefinitionsCapability(),
		),
		agentDeployments: []kernelapi.AgentDeploymentCatalogEntry{entry},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.section, model.focus = sectionReadiness, focusPanel

	view := model.View()
	for _, expected := range []string{
		"Callable operations · 1",
		"collect_findings · research-cycle@1.0.0",
		"Takes · limit:integer · subreddit:string",
		"Returns · findings:array",
		"n start operation",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("active operation view missing %q:\n%s", expected, view)
		}
	}

	model.prepareActiveRunbookOperation()
	if model.mode != modeRunbookOperationStart || !strings.Contains(model.editor.Placeholder, `"subreddit"`) {
		t.Fatalf("operation composer mode=%v placeholder=%q", model.mode, model.editor.Placeholder)
	}
	model.editor.SetValue(`{"subreddit":"vibecoding","limit":25}`)
	applyCommand(t, model, model.submitRunbookOperation())
	if len(fake.createRequests) != 1 || len(fake.createKeys) != 1 || fake.createKeys[0] == "" {
		t.Fatalf("run creates=%#v keys=%#v", fake.createRequests, fake.createKeys)
	}
	request := fake.createRequests[0]
	if request.Owner != (runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"}) ||
		request.AssignedAgentID != "operator" || request.Entrypoint != "collect_findings" ||
		request.Source != runtime.RunSourceManual || request.Context["subreddit"] != "vibecoding" ||
		fmt.Sprint(request.Context["limit"]) != "25" {
		t.Fatalf("created exact operation request = %#v", request)
	}
	if model.section != sectionRuns || model.selectedID != "created" {
		t.Fatalf("created Run was not immediately inspectable: section=%v selected=%q", model.section, model.selectedID)
	}
}

func TestRuntimeReadinessPreservesOperationIdempotencyAndValidatesInput(t *testing.T) {
	entry := testActiveRunbookAgentEntry()
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(
			kernelapi.AgentRunsCapability(kernelapi.OperationCreate),
			kernelapi.AgentDefinitionsCapability(),
		),
		agentDeployments: []kernelapi.AgentDeploymentCatalogEntry{entry},
		createErrors:     []error{errors.New("temporary transport failure"), nil},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.section, model.focus = sectionReadiness, focusPanel
	model.prepareActiveRunbookOperation()

	model.editor.SetValue(`{"subreddit":"vibecoding","limit":"many"}`)
	if command := model.submitRunbookOperation(); command != nil || len(fake.createRequests) != 0 ||
		!strings.Contains(model.status, "does not match") {
		t.Fatalf("invalid input dispatched: command=%v creates=%d status=%q", command != nil, len(fake.createRequests), model.status)
	}

	model.editor.SetValue(`{"subreddit":"vibecoding","limit":10}`)
	applyCommand(t, model, model.submitRunbookOperation())
	if len(fake.createKeys) != 1 || model.editor.Value() == "" || model.pendingKey == "" {
		t.Fatalf("failed attempt did not preserve retry intent: keys=%#v draft=%q pending=%q", fake.createKeys, model.editor.Value(), model.pendingKey)
	}
	applyCommand(t, model, model.submitRunbookOperation())
	if len(fake.createKeys) != 2 || fake.createKeys[0] != fake.createKeys[1] {
		t.Fatalf("retry idempotency keys = %#v", fake.createKeys)
	}
}

func TestRuntimeReadinessNeverOffersInactiveHistoricalUnauthorizedOrMalformedOperations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*kernelapi.AgentDeploymentCatalogEntry)
		runs   kernelapi.Capability
	}{
		{
			name: "inactive",
			mutate: func(entry *kernelapi.AgentDeploymentCatalogEntry) {
				entry.Deployment.RolloutStatus = kernelagent.RolloutPaused
			},
			runs: kernelapi.AgentRunsCapability(kernelapi.OperationCreate),
		},
		{
			name: "historical",
			mutate: func(entry *kernelapi.AgentDeploymentCatalogEntry) {
				entry.Definition.Version = "0.9.0"
			},
			runs: kernelapi.AgentRunsCapability(kernelapi.OperationCreate),
		},
		{
			name:   "unauthorized",
			mutate: func(*kernelapi.AgentDeploymentCatalogEntry) {},
			runs:   kernelapi.AgentRunsCapability(kernelapi.OperationList),
		},
		{
			name: "malformed",
			mutate: func(entry *kernelapi.AgentDeploymentCatalogEntry) {
				delete(entry.Definition.Runbook.Steps, "done")
			},
			runs: kernelapi.AgentRunsCapability(kernelapi.OperationCreate),
		},
		{
			name: "cognitive-only",
			mutate: func(entry *kernelapi.AgentDeploymentCatalogEntry) {
				entry.Definition.Runbook = nil
			},
			runs: kernelapi.AgentRunsCapability(kernelapi.OperationCreate),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			entry := testActiveRunbookAgentEntry()
			testCase.mutate(&entry)
			fake := &fakeKernelClient{
				document: kernelapi.NewCapabilityDocument(
					testCase.runs,
					kernelapi.AgentDefinitionsCapability(),
				),
				agentDeployments: []kernelapi.AgentDeploymentCatalogEntry{entry},
			}
			model := newTestModel(t, fake)
			applyCommand(t, model, model.loadCapabilities())
			model.section, model.focus = sectionReadiness, focusPanel
			if strings.Contains(model.View(), "n start operation") || model.canStartActiveRunbookOperation() {
				t.Fatalf("%s operation was presented as runnable:\n%s", testCase.name, model.View())
			}
		})
	}
}

func testActiveRunbookAgentEntry() kernelapi.AgentDeploymentCatalogEntry {
	return kernelapi.AgentDeploymentCatalogEntry{
		Deployment: &kernelagent.AgentDeployment{
			ID: "operator", Scope: capability.ScopeReference{Kind: "local", ID: "default"},
			DefinitionID: "researcher", ActiveVersion: "1.0.0", RolloutStatus: kernelagent.RolloutActive,
			Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1,
		},
		Definition: &kernelagent.AgentDefinition{
			ID: "researcher", Version: "1.0.0", DisplayName: "Researcher", Purpose: "Collect findings",
			Runbook: &runbook.Definition{
				APIVersion: runbook.APIVersion, ID: "research-cycle", Version: "1.0.0", Name: "Research cycle",
				Entrypoints: map[string]string{"collect_findings": "done"},
				Interfaces: map[string]runbook.Interface{"collect_findings": {
					Description: "Collect a bounded set of findings.",
					InputSchema: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"subreddit": map[string]interface{}{"type": "string"},
							"limit":     map[string]interface{}{"type": "integer"},
						},
						"required": []interface{}{"subreddit", "limit"},
					},
					OutputSchema: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"findings": map[string]interface{}{"type": "array"},
						},
						"required": []interface{}{"findings"},
					},
				}},
				Steps: map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
			},
		},
	}
}

func TestRuntimePlacementPauseUsesAdvertisedGovernedUpdate(t *testing.T) {
	definitionCapability := kernelapi.AgentDefinitionsCapability()
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(definitionCapability),
		agentDeployments: []kernelapi.AgentDeploymentCatalogEntry{{
			Deployment: &kernelagent.AgentDeployment{
				ID: "operator", Scope: capability.ScopeReference{Kind: "local", ID: "default"}, DefinitionID: "operator", ActiveVersion: "1",
				RolloutStatus: kernelagent.RolloutActive, Environment: "local", Credentials: map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "model-provider", ID: "credential://hidden/provider"}},
				Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 3,
			},
			Definition: &kernelagent.AgentDefinition{ID: "operator", Version: "1", DisplayName: "Operator", Purpose: "Operate safely"},
		}},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.section = sectionReadiness
	model.focus = focusPanel
	view := model.View()
	if !strings.Contains(view, "Credentials · 1 opaque reference(s) · model-provider") || strings.Contains(view, "credential://hidden/provider") {
		t.Fatalf("credential placement was not secret-safe:\n%s", view)
	}
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if updated != model || command == nil {
		t.Fatal("advertised Agent pause did not dispatch")
	}
	applyCommand(t, model, command)
	if len(fake.agentUpdates) != 1 || fake.agentUpdates[0].ExpectedRevision != 3 || fake.agentUpdates[0].Deployment.RolloutStatus != kernelagent.RolloutPaused ||
		fake.agentUpdates[0].Reason != "Pause Agent from the terminal" || model.agentDeployment.Deployment.Revision != 4 {
		t.Fatalf("Agent update = %#v deployment=%#v", fake.agentUpdates, model.agentDeployment)
	}

	readOnly := definitionCapability
	readOnly.Operations = []string{kernelapi.OperationGet, kernelapi.OperationList, kernelapi.OperationListCompilations}
	model.agentDefinitionCapability = readOnly
	if strings.Contains(model.renderReadinessContent(100), "p pause/resume") || model.pauseOrResumeAgentDeployment() != nil {
		t.Fatal("unadvertised Agent deployment update remained operable")
	}
}

func newModelWithClient(t *testing.T, kernelClient client.KernelClient) *Model {
	t.Helper()
	config := DefaultConfig()
	config.PollInterval = -1
	model, err := NewModel(context.Background(), kernelClient, config)
	if err != nil {
		t.Fatal(err)
	}
	model.width, model.height = 120, 36
	return model
}

func TestSkillBindingPromptPreservesExactAuthorityAndOpaqueReferences(t *testing.T) {
	binding, reason, err := parseSkillBindingPrompt("binding: reddit-research\nskill: reddit@2.1.0\nactions: search, post\nprompt: false\nrisk: external\ncredentials: reddit=credential:reddit-prod\nreason: approved market research", nil)
	if err != nil {
		t.Fatal(err)
	}
	if binding.SkillID != "reddit" || binding.SkillVersion != "2.1.0" || len(binding.AllowedActions) != 2 || binding.EnablePrompt || binding.MaximumRisk != capability.RiskLevelExternal || binding.Credentials["reddit"].ID != "reddit-prod" || reason != "approved market research" {
		t.Fatalf("parsed binding = %#v reason=%q", binding, reason)
	}
	if _, _, err := parseSkillBindingPrompt("binding: bad\nskill: reddit@2\nactions: post\nprompt: true\nrisk: external\ncredentials: reddit=secret-without-reference\nreason: test", nil); err == nil || !strings.Contains(err.Error(), "never paste a secret") {
		t.Fatalf("raw credential was not rejected: %v", err)
	}
}

func TestSkillBindingTUIUsesCASForAgentAndRecoversFromConflict(t *testing.T) {
	existing := &capability.Binding{ID: "reddit", Scope: capability.ScopeReference{Kind: "local", ID: "default"}, DeploymentID: "operator", SkillID: "reddit", SkillVersion: "1", AllowedActions: []string{"search"}, MaximumRisk: capability.RiskLevelRead, Revision: 7}
	fake := &fakeKernelClient{skillBindings: []*capability.Binding{existing}}
	model := newModelWithClient(t, fake)
	model.ready = true
	model.skillBindingCapability = kernelapi.SkillBindingsCapability(true)
	model.skillBindings = fake.skillBindings
	model.restoreSkillBindingSelection()
	model.prepareSkillBindingComposer(existing)
	model.editor.SetValue("binding: reddit\nskill: reddit@1\nactions: search,post\nprompt: true\nrisk: external\ncredentials: token=credential:reddit-prod\nreason: expand approved authority")
	applyCommand(t, model, model.submitSkillBindingUpsert())
	if len(fake.skillBindingUpserts) != 1 || fake.skillBindingUpserts[0].ExpectedRevision != 7 || fake.skillBindingUpserts[0].Binding.DeploymentID != "operator" || !fake.skillBindingUpserts[0].Binding.EnablePrompt {
		t.Fatalf("upsert = %#v", fake.skillBindingUpserts)
	}

	fake.skillBindingError = &client.APIError{StatusCode: 409, Message: "revision conflict"}
	model.prepareSkillBindingComposer(model.selectedSkillBindingRecord())
	command := model.submitSkillBindingUpsert()
	message := command()
	_, followup := model.Update(message)
	if followup == nil || !strings.Contains(model.status, "changed elsewhere") || model.editor.Value() == "" {
		t.Fatalf("conflict recovery status=%q draft=%q", model.status, model.editor.Value())
	}
}

func TestSkillBindingTUIReviewsAndAppliesAtomicReferenceUpgrade(t *testing.T) {
	scope := runtime.Scope{Kind: "local", ID: "default"}
	existing := &capability.Binding{
		ID: "research", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "research-team",
		SkillID: "source", SkillVersion: "1", SourceIdentity: "registry:source", AllowedActions: []string{"search"},
		MaximumRisk: capability.RiskLevelRead, Revision: 4,
	}
	plan := &runtime.SkillReferenceUpgradePlan{
		APIVersion: runtime.SkillReferenceUpgradeAPIVersion, Scope: scope, DeploymentID: "research-team", BindingID: existing.ID,
		ExpectedBindingRevision: existing.Revision,
		From:                    runtime.SkillReferenceIdentity{ID: "source", Version: "1", SourceIdentity: "registry:source"},
		To:                      runtime.SkillReferenceIdentity{ID: "source", Version: "2", SourceIdentity: "registry:source"},
		Objectives:              []runtime.SkillReferenceObjectiveImpact{{ID: "monitor-hourly", ExpectedRevision: 3}},
		Findings:                []runtime.SkillReferenceUpgradeFinding{{Code: "action_added", Message: "Target exposes one additional governed action."}},
		ApprovalRequired:        true, Digest: "sha256:reviewed",
	}
	fake := &fakeKernelClient{skillBindings: []*capability.Binding{existing}, skillUpgradePlan: plan}
	config := DefaultConfig()
	config.Owner = runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "research-team"}
	config.PollInterval = -1
	model, err := NewModel(context.Background(), fake, config)
	if err != nil {
		t.Fatal(err)
	}
	model.width, model.height = 120, 36
	model.ready = true
	model.section = sectionSkills
	model.skillBindingCapability = kernelapi.SkillBindingsCapability(true)
	model.skillBindings = fake.skillBindings
	model.restoreSkillBindingSelection()

	model.prepareSkillBindingUpgradeComposer()
	model.editor.SetValue("version: 2\nsource: registry:source")
	applyCommand(t, model, model.submitSkillBindingUpgradePlan())
	view := model.renderClawHubSkillsContent(120)
	for _, expected := range []string{"Reviewed update", "source@1", "source@2", "1 Objective(s)", "historical Runs stay unchanged", "additional governed action", "Explicit approval"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("upgrade review missing %q:\n%s", expected, view)
		}
	}
	if len(fake.skillUpgradePlans) != 1 || fake.skillUpgradePlans[0].BindingID != existing.ID || model.mode != modeSkillBindingUpgradeApply {
		t.Fatalf("plans=%#v mode=%v", fake.skillUpgradePlans, model.mode)
	}

	model.editor.SetValue("reason: reviewed dependent monitoring work\napproval: approved bounded action expansion")
	applyCommand(t, model, model.submitSkillBindingUpgradeApply())
	if len(fake.skillUpgradeApplies) != 1 || fake.skillUpgradeApplies[0].Approval == nil ||
		fake.skillUpgradeApplies[0].Approval.Reason != "approved bounded action expansion" ||
		model.skillBindingUpgradePlan != nil || !strings.Contains(model.status, "Skill updated to source@2") {
		t.Fatalf("applies=%#v plan=%#v status=%q", fake.skillUpgradeApplies, model.skillBindingUpgradePlan, model.status)
	}
}

func testConversation(id, title string, lastSequence, revision int64) *runtime.Conversation {
	return &runtime.Conversation{
		ID: id, Scope: runtime.Scope{Kind: "local", ID: "default"},
		Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"},
		Title: title, Status: runtime.ConversationStatusActive, LastSequence: lastSequence,
		Revision: revision, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

func applyCommand(t *testing.T, model *Model, command tea.Cmd) {
	t.Helper()
	if command == nil {
		t.Fatal("expected command")
	}
	applyCommandRecursive(t, model, command)
}

func applyCommandRecursive(t *testing.T, model *Model, command tea.Cmd) {
	t.Helper()
	message := command()
	if batch, ok := message.(tea.BatchMsg); ok {
		for _, child := range batch {
			if child != nil {
				applyCommandRecursive(t, model, child)
			}
		}
		return
	}
	updated, followup := model.Update(message)
	if updated != model {
		t.Fatal("model identity changed unexpectedly")
	}
	if followup != nil {
		applyCommandRecursive(t, model, followup)
	}
}

func testRun(id string, status runtime.AgentRunStatus, revision int64) *runtime.AgentRun {
	return &runtime.AgentRun{
		ID: id, Scope: runtime.Scope{Kind: "local", ID: "default"},
		Owner:           runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"},
		AssignedAgentID: "operator", Goal: "First durable outcome", Source: runtime.RunSourceManual,
		Status: status, Revision: revision, UpdatedAt: time.Now(),
	}
}

func testDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

type fakeAgentBuilderKernelClient struct {
	*fakeChannelKernelClient
	builderStarts    []kernelapi.StartAgentBuilderConversationRequest
	builderErrors    []error
	builderApprovals []*runtime.ApprovalCheckpoint
}

func (f *fakeAgentBuilderKernelClient) StartAgentBuilderConversation(_ context.Context, request kernelapi.StartAgentBuilderConversationRequest) (*kernelapi.StartAgentBuilderConversationResponse, error) {
	f.builderStarts = append(f.builderStarts, request)
	if len(f.builderErrors) > 0 {
		err := f.builderErrors[0]
		f.builderErrors = f.builderErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	conversation := testConversation("builder-chat", "New agent", 1, 2)
	conversation.Owner = runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: runtime.AgentBuilderDeploymentID}
	conversation.Origin = &runtime.ConversationReference{Kind: runtime.ConversationReferenceAgentBuilder, ID: request.IdempotencyKey}
	message := &runtime.ChannelMessage{
		ID: "builder-prompt", Scope: request.Scope, ConversationID: conversation.ID, Sequence: 1,
		Sender: runtime.ConversationParticipant{Type: runtime.ConversationParticipantUser, ID: "local-operator"},
		Intent: runtime.MessageIntentUpdate, Content: request.Prompt, BuilderParameters: &request.Parameters,
		Audience: runtime.ConversationAudience{Kind: runtime.ConversationAudienceChannel}, CreatedAt: time.Now(),
	}
	f.conversations = append(f.conversations, conversation)
	f.messages = []*runtime.ChannelMessage{message}
	return &kernelapi.StartAgentBuilderConversationResponse{Conversation: conversation, Message: message}, nil
}

// ListConversations and ListActionApprovals honour the owner filter, like the
// daemon: the builder chat and its cards belong to the builder Agent.
func (f *fakeAgentBuilderKernelClient) ListConversations(_ context.Context, filter runtime.ConversationFilter) ([]*runtime.Conversation, error) {
	var conversations []*runtime.Conversation
	for _, conversation := range f.conversations {
		if filter.Owner == nil || conversation.Owner == *filter.Owner {
			conversations = append(conversations, conversation)
		}
	}
	return conversations, nil
}

func (f *fakeAgentBuilderKernelClient) ListActionApprovals(ctx context.Context, filter runtime.ApprovalFilter) ([]*runtime.ApprovalCheckpoint, error) {
	if filter.Owner != nil && filter.Owner.ID == runtime.AgentBuilderDeploymentID {
		f.approvalFilters = append(f.approvalFilters, filter)
		return f.builderApprovals, nil
	}
	return f.fakeKernelClient.ListActionApprovals(ctx, filter)
}

func (f *fakeAgentBuilderKernelClient) GetActionApproval(ctx context.Context, scope runtime.Scope, id string) (*runtime.ApprovalCheckpoint, error) {
	for _, approval := range f.builderApprovals {
		if approval.ID == id {
			return approval, nil
		}
	}
	return f.fakeKernelClient.GetActionApproval(ctx, scope, id)
}

func (f *fakeAgentBuilderKernelClient) ResolveActionApproval(_ context.Context, _ runtime.Scope, id string, decision kernelapi.ResolveActionApprovalRequest, key string) (*runtime.ApprovalResolutionResult, error) {
	f.actionDecisions = append(f.actionDecisions, decision)
	f.actionDecisionKeys = append(f.actionDecisionKeys, key)
	approval, err := f.GetActionApproval(context.Background(), runtime.Scope{}, id)
	if err != nil {
		return nil, err
	}
	approval.Revision++
	approval.Status = runtime.ApprovalStatusRejected
	if decision.Decision == runtime.ApprovalDecisionApprove {
		approval.Status = runtime.ApprovalStatusApproved
	}
	return &runtime.ApprovalResolutionResult{Approval: approval, Resolved: true}, nil
}

func newAgentBuilderTestClient() *fakeAgentBuilderKernelClient {
	return &fakeAgentBuilderKernelClient{fakeChannelKernelClient: &fakeChannelKernelClient{
		fakeKernelClient: &fakeKernelClient{document: kernelapi.NewCapabilityDocument(
			kernelapi.AgentBuilderCapability(runtime.AgentBuilderDeploymentID),
			kernelapi.ChannelsCapability(kernelapi.ChannelCapabilityFeatures{}),
			kernelapi.ActionApprovalsCapability(kernelapi.ActionApprovalCapabilityFeatures{Resolution: true}),
		)},
		conversations: []*runtime.Conversation{testConversation("team-room", "team-room", 0, 1)},
	}}
}

func builderCard(id string, scope runtime.Scope) *runtime.ApprovalCheckpoint {
	now := time.Now()
	return &runtime.ApprovalCheckpoint{
		ID: id, Scope: scope, RunID: "builder-run", ActionCallID: "call-" + id,
		Status: runtime.ApprovalStatusPending, Risk: skill.RiskLevelWrite, Summary: "Create the agent Support triage",
		ProposedAction: map[string]interface{}{
			"skillId": "openseal.agents", "skillVersion": "1.5.0", "action": "create_agent",
			"resourceType": "agent_profile", "operation": "create_agent",
			"agent": map[string]interface{}{"displayName": "Support triage"},
		},
		ConversationContext: &runtime.ApprovalConversationContext{ConversationID: "builder-chat"},
		EligibleApprovers:   []runtime.ApprovalPrincipal{{Type: "user", ID: "local"}},
		ExpiresAt:           now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func TestAgentBuilderNewAgentOpensBuilderChat(t *testing.T) {
	fake := newAgentBuilderTestClient()
	fake.builderErrors = []error{errors.New("daemon restarting")}
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.section != sectionAuthoring || model.mode != modeAgentBuilderPrompt || !strings.Contains(model.View(), "New agent") {
		t.Fatalf("new agent prompt section=%v mode=%v view:\n%s", model.section, model.mode, model.View())
	}

	model.editor.SetValue("  An agent that triages support email  ")
	_, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	applyCommand(t, model, command)
	if len(fake.builderStarts) != 1 || model.mode != modeAgentBuilderPrompt || model.editor.Value() == "" || !strings.Contains(model.status, "kept for retry") {
		t.Fatalf("failed start starts=%#v mode=%v editor=%q status=%q", fake.builderStarts, model.mode, model.editor.Value(), model.status)
	}
	_, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	applyCommand(t, model, command)

	if len(fake.builderStarts) != 2 {
		t.Fatalf("starts = %#v", fake.builderStarts)
	}
	first, retry := fake.builderStarts[0], fake.builderStarts[1]
	if retry.Prompt != "An agent that triages support email" || retry.Parameters != (runtime.AgentBuilderParameters{Kind: runtime.AgentBuilderKindAgent}) ||
		retry.Scope != model.config.Scope || retry.SpaceID != "" || retry.IdempotencyKey == "" || retry.IdempotencyKey != first.IdempotencyKey {
		t.Fatalf("builder start first=%#v retry=%#v", first, retry)
	}
	if len(fake.postRequests) != 0 || len(fake.createConversationRequests) != 0 {
		t.Fatalf("new agent wrote a channel itself: posts=%#v creates=%#v", fake.postRequests, fake.createConversationRequests)
	}
	if model.section != sectionChannels || model.mode != modeChannelPost || model.selectedConversation != "builder-chat" ||
		model.editor.Value() != "" || model.pendingAgentBuilderKey != "" {
		t.Fatalf("builder chat not open section=%v mode=%v selected=%q editor=%q key=%q", model.section, model.mode, model.selectedConversation, model.editor.Value(), model.pendingAgentBuilderKey)
	}
	if record := model.selectedConversationRecord(); record == nil || record.ID != "builder-chat" || len(model.conversations) != 2 {
		t.Fatalf("conversations = %#v", model.conversations)
	}
	if view := model.View(); !strings.Contains(view, "An agent that triages support email") {
		t.Fatalf("builder chat view missing the request:\n%s", view)
	}

	// The reply goes into the builder chat through the existing channel post.
	model.focus = focusComposer
	model.editor.SetValue("Mostly billing questions")
	_, command = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	applyCommand(t, model, command)
	if len(fake.postRequests) != 1 || fake.postKeys[0] == "" || fake.postRequests[0].Content != "Mostly billing questions" || model.selectedConversation != "builder-chat" {
		t.Fatalf("reply posts=%#v selected=%q", fake.postRequests, model.selectedConversation)
	}
}

func TestAgentBuilderCardIsApprovedThroughActionApproval(t *testing.T) {
	fake := newAgentBuilderTestClient()
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.editor.SetValue("An agent that triages support email")
	_, command := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	applyCommand(t, model, command)

	create := builderCard("card-create", model.config.Scope)
	reject := builderCard("card-update", model.config.Scope)
	reject.ProposedAction["action"], reject.ProposedAction["operation"] = "update_agent", "update_agent"
	fake.builderApprovals = []*runtime.ApprovalCheckpoint{create, reject}
	applyCommand(t, model, model.loadActionApprovals())
	builderFilters := 0
	for _, filter := range fake.approvalFilters {
		if filter.Owner != nil && *filter.Owner == (runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: runtime.AgentBuilderDeploymentID}) {
			builderFilters++
		}
	}
	if builderFilters == 0 || len(model.actionApprovals) != 2 {
		t.Fatalf("builder cards not listed filters=%#v approvals=%#v", fake.approvalFilters, model.actionApprovals)
	}

	model.section = sectionApprovals
	model.focus = focusPanel
	model.actionApprovalSelected, model.selectedActionApproval = 0, create.ID
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if model.mode != modeApprovalApprove {
		t.Fatalf("approve mode = %v", model.mode)
	}
	model.editor.SetValue("This is the agent I asked for")
	_, command = model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)

	model.section = sectionApprovals
	model.focus = focusPanel
	model.actionApprovalSelected, model.selectedActionApproval = 1, reject.ID
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if model.mode != modeApprovalReject {
		t.Fatalf("reject mode = %v", model.mode)
	}
	model.editor.SetValue("Not this change")
	_, command = model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	applyCommand(t, model, command)

	if len(fake.actionDecisions) != 2 ||
		fake.actionDecisions[0].Decision != runtime.ApprovalDecisionApprove || fake.actionDecisions[0].Principal != (runtime.ApprovalPrincipal{Type: "user", ID: "local"}) ||
		fake.actionDecisions[1].Decision != runtime.ApprovalDecisionReject ||
		create.Status != runtime.ApprovalStatusApproved || reject.Status != runtime.ApprovalStatusRejected {
		t.Fatalf("decisions=%#v create=%s reject=%s", fake.actionDecisions, create.Status, reject.Status)
	}
}

func TestAgentBuilderPromptUnavailableWithoutBuilder(t *testing.T) {
	fake := newAgentBuilderTestClient()
	fake.document = kernelapi.NewCapabilityDocument(kernelapi.ChannelsCapability(kernelapi.ChannelCapabilityFeatures{}))
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.section, model.mode = sectionAuthoring, modeAgentBuilderPrompt
	model.editor.SetValue("An agent that triages support email")
	if command := model.submitAgentBuilderPrompt(); command != nil || len(fake.builderStarts) != 0 {
		t.Fatalf("started without the agent-builder capability: %#v", fake.builderStarts)
	}
	if !strings.Contains(model.View(), "This server has no agent builder.") {
		t.Fatalf("view:\n%s", model.View())
	}
}
