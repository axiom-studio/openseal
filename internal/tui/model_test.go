package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/internal/server"
	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
	tea "github.com/charmbracelet/bubbletea"
	"go.uber.org/zap"
)

type fakeKernelClient struct {
	document            kernelapi.CapabilityDocument
	agentDeployments    []kernelapi.AgentDeploymentCatalogEntry
	runs                []*runtime.AgentRun
	agentRequests       []*runtime.AgentRequest
	agentRequestFilters []runtime.AgentRequestFilter
	agentRequestCreates []kernelapi.CreateAgentRequestRequest
	agentRequestKeys    []string
	agentResponses      []kernelapi.RespondAgentRequestRequest
	agentCompletions    []kernelapi.CompleteAgentRequestRequest
	agentCompletionKeys []string
	actionApprovals     []*runtime.ApprovalCheckpoint
	approvalFilters     []runtime.ApprovalFilter
	actionDecisions     []kernelapi.ResolveActionApprovalRequest
	actionDecisionKeys  []string
	compilations        []*kernelagent.DefinitionCompilation
	createErrors        []error
	createKeys          []string
	createRequests      []kernelapi.CreateAgentRunRequest
	objectives          []*runtime.Objective
	objectiveKeys       []string
	objectiveCreates    []kernelapi.CreateObjectiveRequest
	objectiveUpdates    []kernelapi.UpdateObjectiveRequest
	initiatives         []*runtime.Initiative
	monitorCheckpoints  map[string]*runtime.SourceMonitorCheckpoint
	monitorObservations map[string][]*runtime.SourceObservation
	activityPages       map[string]*runtime.ActivityFeedPage
	activityRequests    []runtime.ActivityFeedRequest
	initiativeKeys      []string
	initiativeCreates   []kernelapi.CreateInitiativeRequest
	initiativeUpdates   []kernelapi.UpdateInitiativeRequest
	commands            []kernelapi.AgentRunCommandRequest
	artifacts           []*runtime.Artifact
	downloadBody        string
	downloadCalls       int
	authoringResult     *authoring.CompileResult
	authoringRequests   []authoring.GenerateRequest
	authoringErrors     []error
	changeSets          []*authoring.ChangeSet
	changeSetRequests   []authoring.CreateChangeSetRequest
	changeSetKeys       []string
	changeSetErrors     []error
	approvalRequests    []authoring.ResolveChangeSetApprovalRequest
	approvalKeys        []string
	applyRequests       []authoring.ApplyChangeSetRequest
	applyKeys           []string
	retryRequests       []authoring.RetryChangeSetGenerationRequest
	retryKeys           []string
	governanceResults   []*authoring.ChangeSet
	governanceErrors    []error
	skillActions        []capability.ModelAction
	skillActionCalls    int
}

var (
	_ client.KernelClient = (*fakeKernelClient)(nil)
	_ client.KernelClient = (*fakeChannelKernelClient)(nil)
	_ client.KernelClient = (*fakeClawHubKernelClient)(nil)
)

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

func (f *fakeChannelKernelClient) CoordinateParticipation(context.Context, string, kernelapi.CoordinateParticipationRequest, string) (*runtime.ParticipationRoundResult, error) {
	return nil, errors.New("not implemented by test client")
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

func (f *fakeKernelClient) WorkforceChangeSetCapabilities(context.Context, capability.ScopeReference, string) (kernelapi.CapabilityDocument, error) {
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
	if decision.Approve {
		approval.Status = runtime.ApprovalStatusApproved
	}
	return &runtime.ApprovalResolutionResult{Approval: approval, Resolved: true}, nil
}

func (f *fakeKernelClient) CompileWorkforce(_ context.Context, request authoring.GenerateRequest) (*authoring.CompileResult, error) {
	f.authoringRequests = append(f.authoringRequests, request)
	if len(f.authoringErrors) > 0 {
		err := f.authoringErrors[0]
		f.authoringErrors = f.authoringErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	return f.authoringResult, nil
}

func (f *fakeKernelClient) CreateWorkforceChangeSet(_ context.Context, request authoring.CreateChangeSetRequest, key string) (*authoring.ChangeSet, error) {
	f.changeSetRequests = append(f.changeSetRequests, request)
	f.changeSetKeys = append(f.changeSetKeys, key)
	if len(f.changeSetErrors) > 0 {
		err := f.changeSetErrors[0]
		f.changeSetErrors = f.changeSetErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(f.changeSets) == 0 {
		return nil, errors.New("workforce change sets are not configured in this test")
	}
	result := f.changeSets[0]
	if len(f.changeSets) > 1 {
		f.changeSets = f.changeSets[1:]
	}
	return result, nil
}

func (f *fakeKernelClient) GetWorkforceChangeSet(context.Context, capability.ScopeReference, string) (*authoring.ChangeSet, error) {
	if len(f.governanceResults) > 0 {
		return f.governanceResults[0], nil
	}
	return nil, authoring.ErrChangeSetNotFound
}

func (f *fakeKernelClient) EvaluateWorkforceChangeSet(context.Context, authoring.SubmitChangeSetEvaluationRequest, string) (*authoring.ChangeSet, error) {
	return nil, errors.New("policy evaluation is not initiated by the TUI")
}

func (f *fakeKernelClient) ResolveWorkforceChangeSetApproval(_ context.Context, request authoring.ResolveChangeSetApprovalRequest, key string) (*authoring.ChangeSet, error) {
	f.approvalRequests = append(f.approvalRequests, request)
	f.approvalKeys = append(f.approvalKeys, key)
	return f.nextGovernanceResult()
}

func (f *fakeKernelClient) ApplyWorkforceChangeSet(_ context.Context, request authoring.ApplyChangeSetRequest, key string) (*authoring.ChangeSet, error) {
	f.applyRequests = append(f.applyRequests, request)
	f.applyKeys = append(f.applyKeys, key)
	return f.nextGovernanceResult()
}

func (f *fakeKernelClient) RetryWorkforceChangeSetGeneration(_ context.Context, request authoring.RetryChangeSetGenerationRequest, key string) (*authoring.ChangeSet, error) {
	f.retryRequests = append(f.retryRequests, request)
	f.retryKeys = append(f.retryKeys, key)
	return f.nextGovernanceResult()
}

func (f *fakeKernelClient) nextGovernanceResult() (*authoring.ChangeSet, error) {
	if len(f.governanceErrors) > 0 {
		err := f.governanceErrors[0]
		f.governanceErrors = f.governanceErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(f.governanceResults) == 0 {
		return nil, errors.New("no governance result")
	}
	result := f.governanceResults[0]
	f.governanceResults = f.governanceResults[1:]
	return result, nil
}

func TestFailedWorkforceGenerationRetriesWithStableIdentity(t *testing.T) {
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	failed := &authoring.ChangeSet{ID: "failed-one", Scope: scope, Status: authoring.ChangeSetFailed, Revision: 4, Generation: &authoring.ChangeSetGeneration{RunID: "run-one", Attempt: 1, LastError: "provider unavailable"}}
	evaluating := *failed
	evaluating.Status = authoring.ChangeSetEvaluating
	evaluating.Revision = 5
	fake := &fakeKernelClient{governanceResults: []*authoring.ChangeSet{&evaluating, &evaluating}}
	m := newModelWithClient(t, fake)
	m.ready, m.section, m.focus = true, sectionAuthoring, focusComposer
	m.authoringChangeSet = failed
	m.authoringCapability = kernelapi.Capability{ID: kernelapi.WorkforceAuthoringCapabilityID, Version: kernelapi.WorkforceAuthoringCapabilityVersion, Available: true, Operations: []string{kernelapi.OperationRetry}, Context: &kernelapi.CapabilityContext{ChangeSetID: failed.ID, Revision: failed.Revision}}
	m.mode = modeWorkforceRetry
	m.editor.SetValue("provider recovered")
	msg := m.submitWorkforceRetry()()
	if _, ok := msg.(workforceGoverned); !ok {
		t.Fatalf("message = %T", msg)
	}
	firstKey := fake.retryKeys[0]
	m.busy = false
	msg = m.submitWorkforceRetry()()
	if _, ok := msg.(workforceGoverned); !ok || fake.retryKeys[1] != firstKey || firstKey == "" {
		t.Fatalf("retry keys = %#v", fake.retryKeys)
	}
	if fake.retryRequests[0].Actor.ID != "" {
		t.Fatalf("client forged retry actor = %#v", fake.retryRequests[0].Actor)
	}
	m.busy = false
	view := m.renderAuthoringContent(80)
	if !strings.Contains(view, "provider unavailable") || !strings.Contains(view, "r retry generation") {
		t.Fatalf("failed generation not rendered: %s", view)
	}
}

func TestEvaluatingWorkforceRefreshDoesNotRenderAnEmptyCandidate(t *testing.T) {
	m := newModelWithClient(t, &fakeKernelClient{})
	changeSet := &authoring.ChangeSet{ID: "queued", Status: authoring.ChangeSetEvaluating, Generation: &authoring.ChangeSetGeneration{RunID: "run-queued"}}
	updated, _ := m.Update(workforceLoaded{changeSet: changeSet})
	model := updated.(*Model)
	if model.authoringResult != nil {
		t.Fatalf("empty evaluating candidate became review result: %#v", model.authoringResult)
	}
	if view := model.renderAuthoringContent(80); !strings.Contains(view, "continues in the background") {
		t.Fatalf("evaluating state not rendered: %s", view)
	}
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
			return &kernelapi.ObjectiveDetail{Objective: objective, Runs: f.runs}, nil
		}
	}
	return nil, runtime.ErrObjectiveNotFound
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

func (f *fakeKernelClient) CreateInitiative(_ context.Context, request kernelapi.CreateInitiativeRequest, key string) (*runtime.Initiative, error) {
	f.initiativeKeys = append(f.initiativeKeys, key)
	f.initiativeCreates = append(f.initiativeCreates, request)
	initiative := &runtime.Initiative{ID: "initiative-created", Scope: request.Scope, Owner: request.Owner, Title: request.Title, Purpose: request.Purpose, Status: request.Status, ObjectiveRefs: request.ObjectiveRefs, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.initiatives = append([]*runtime.Initiative{initiative}, f.initiatives...)
	return initiative, nil
}

func (f *fakeKernelClient) ListInitiatives(context.Context, runtime.InitiativeFilter) ([]*runtime.Initiative, error) {
	return f.initiatives, nil
}
func (f *fakeKernelClient) ListSourceObservations(_ context.Context, filter runtime.SourceObservationFilter) ([]*runtime.SourceObservation, error) {
	return f.monitorObservations[sourceMonitorStatusKey(filter.InitiativeID, filter.MonitorID)], nil
}
func (f *fakeKernelClient) ListActivity(_ context.Context, request runtime.ActivityFeedRequest) (*runtime.ActivityFeedPage, error) {
	f.activityRequests = append(f.activityRequests, request)
	if page := f.activityPages[request.RunID]; page != nil {
		return page, nil
	}
	return &runtime.ActivityFeedPage{}, nil
}
func (f *fakeKernelClient) GetSourceMonitorCheckpoint(_ context.Context, _ runtime.Scope, initiativeID, monitorID string) (*runtime.SourceMonitorCheckpoint, error) {
	checkpoint := f.monitorCheckpoints[sourceMonitorStatusKey(initiativeID, monitorID)]
	if checkpoint == nil {
		return nil, runtime.ErrSourceObservationNotFound
	}
	return checkpoint, nil
}
func (f *fakeKernelClient) GetInitiative(_ context.Context, _ runtime.Scope, id string) (*runtime.Initiative, error) {
	for _, initiative := range f.initiatives {
		if initiative.ID == id {
			return initiative, nil
		}
	}
	return nil, runtime.ErrInitiativeNotFound
}
func (f *fakeKernelClient) PatchInitiative(_ context.Context, _ runtime.Scope, id string, request kernelapi.UpdateInitiativeRequest) (*runtime.Initiative, error) {
	f.initiativeUpdates = append(f.initiativeUpdates, request)
	for _, initiative := range f.initiatives {
		if initiative.ID == id {
			if request.Purpose != nil {
				initiative.Purpose = *request.Purpose
			}
			if request.Status != nil {
				initiative.Status = *request.Status
			}
			if request.ObjectiveRefs != nil {
				initiative.ObjectiveRefs = append([]string(nil), (*request.ObjectiveRefs)...)
			}
			initiative.Revision++
			initiative.UpdatedAt = time.Now()
			return initiative, nil
		}
	}
	return nil, runtime.ErrInitiativeNotFound
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
	return f.compilations, nil
}

func (f *fakeKernelClient) GetAgentDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelapi.AgentDeploymentCatalogEntry, error) {
	for i := range f.agentDeployments {
		entry := &f.agentDeployments[i]
		if entry.Deployment != nil && entry.Deployment.ID == id && entry.Deployment.Scope == scope {
			return entry, nil
		}
	}
	return nil, kernelagent.ErrDeploymentNotFound
}

func (f *fakeKernelClient) ListAgentDeployments(_ context.Context, scope capability.ScopeReference) (*kernelapi.AgentDeploymentList, error) {
	items := make([]kernelapi.AgentDeploymentCatalogEntry, 0, len(f.agentDeployments))
	for _, entry := range f.agentDeployments {
		if entry.Deployment != nil && entry.Deployment.Scope == scope {
			items = append(items, entry)
		}
	}
	return &kernelapi.AgentDeploymentList{Items: items}, nil
}

func (f *fakeKernelClient) GetAgentRun(context.Context, runtime.Scope, string) (*runtime.AgentRun, error) {
	if len(f.runs) == 0 {
		return nil, runtime.ErrRunNotFound
	}
	return f.runs[0], nil
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

func (f *fakeKernelClient) ListTeamDeployments(context.Context, capability.ScopeReference) (*kernelapi.TeamDeploymentList, error) {
	return &kernelapi.TeamDeploymentList{}, nil
}

func (f *fakeKernelClient) ActivateTeamDefinition(context.Context, string, kernelapi.ActivateTeamDefinitionRequest) (*kernelapi.TeamDeploymentResult, error) {
	return nil, errors.New("not implemented by test client")
}

func (f *fakeKernelClient) UpdateTeamDeployment(context.Context, string, kernelapi.UpdateTeamDeploymentRequest) (*kernelapi.TeamDeploymentResult, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeKernelClient) ListTeamDefinitionActivations(context.Context, capability.ScopeReference, string) ([]workforce.DefinitionActivation, error) {
	return nil, errors.New("not implemented by test client")
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
	list, err := fake.ListAgentDeployments(context.Background(), tenantOne)
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
	for _, expected := range []string{"R Requests", "Collaboration requests", "Review the cited report", "from agent:researcher", "y accept", "? clarify", "x reject"} {
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
	if len(fake.actionDecisions) != 1 || !fake.actionDecisions[0].Approve || fake.actionDecisions[0].ExpectedRevision != 3 || fake.actionDecisions[0].Principal != (runtime.ApprovalPrincipal{Type: "user", ID: "local"}) || fake.actionDecisionKeys[0] == "" || fake.actionDecisions[0].DecisionID != fake.actionDecisionKeys[0] {
		t.Fatalf("decision=%#v keys=%#v", fake.actionDecisions, fake.actionDecisionKeys)
	}

	readOnly := &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.ActionApprovalsCapability(kernelapi.ActionApprovalCapabilityFeatures{})), actionApprovals: []*runtime.ApprovalCheckpoint{approval}}
	readOnlyModel := newTestModel(t, readOnly)
	applyCommand(t, readOnlyModel, readOnlyModel.loadCapabilities())
	if strings.Contains(readOnlyModel.View(), "y approve") || strings.Contains(readOnlyModel.View(), "x reject") {
		t.Fatalf("resolution controls rendered without advertised operation:\n%s", readOnlyModel.View())
	}
}

func TestMismatchedRequestCapabilityFailsClosedWithoutHidingMatchingWork(t *testing.T) {
	document := kernelapi.NewCapabilityDocument(
		kernelapi.AgentRunsCapability(),
		kernelapi.Capability{ID: kernelapi.AgentRequestsCapabilityID, Version: "future", Available: true, Operations: []string{kernelapi.OperationList, kernelapi.OperationRespond}},
	)
	model := newTestModel(t, &fakeKernelClient{document: document})
	applyCommand(t, model, model.loadCapabilities())
	if !model.ready || model.runCapability.Available == false || model.requestCapability.Available || strings.Contains(model.View(), "R Requests") {
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

func TestPromptFirstWorkforceAuthoringIsCapabilityGatedAndPreviewOnly(t *testing.T) {
	result := &authoring.CompileResult{
		Valid: false,
		Candidate: authoring.WorkforceCandidate{
			Agents: []*kernelagent.AgentDefinition{{
				ID: "researcher", Version: "1", DisplayName: "Researcher", Purpose: "Gather evidence", SystemPrompt: "Research carefully.",
				Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 2},
			}},
			Team: &kernelteam.Definition{
				ID: "research", Version: "1", DisplayName: "Research Team", Purpose: "Find customer pain points",
				Roles:        []kernelteam.RoleSlot{{ID: "researcher", DisplayName: "Researcher", Purpose: "Gather evidence", MinimumMembers: 1}},
				Coordination: kernelteam.CoordinationPolicy{Mode: kernelteam.CoordinationDynamic},
				Approvals:    kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
			},
		},
		Questions: []string{"Which sources are authorized?"},
		Diff:      []authoring.FieldDiff{{Path: "team.approvals", AfterDigest: "candidate"}},
	}
	fake := &fakeKernelClient{
		document:        kernelapi.NewCapabilityDocument(kernelapi.WorkforceAuthoringCapability(kernelapi.WorkforceAuthoringCapabilityFeatures{}), kernelapi.ObjectivesCapability()),
		authoringResult: result,
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.section != sectionAuthoring || model.mode != modeWorkforceAuthoring || !strings.Contains(model.View(), "Create Agents and Teams") {
		t.Fatalf("authoring was not the primary capability:\n%s", model.View())
	}
	model.editor.SetValue("Create a customer research Team")
	applyCommand(t, model, model.submitWorkforceAuthoring())
	view := model.View()
	if len(fake.authoringRequests) != 1 || fake.authoringRequests[0].Catalog.Skills != nil {
		t.Fatalf("authoring requests = %#v", fake.authoringRequests)
	}
	for _, expected := range []string{"Research Team", "1 Agents", "Which sources are authorized?", "Nothing is active"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("authoring preview missing %q:\n%s", expected, view)
		}
	}
	if strings.Contains(view, "field changes") {
		t.Fatalf("create preview was presented as an amendment:\n%s", view)
	}
	model.editor.SetValue("Require approval before external outreach")
	applyCommand(t, model, model.submitWorkforceAuthoring())
	if len(fake.authoringRequests) != 2 || fake.authoringRequests[1].Mode != authoring.ModeAmend ||
		fake.authoringRequests[1].Existing == nil || fake.authoringRequests[1].Existing.Team.ID != "research" {
		t.Fatalf("follow-up authoring request = %#v", fake.authoringRequests)
	}
	if model.editor.Value() != "" || !strings.Contains(model.editor.Placeholder, "change") {
		t.Fatalf("follow-up composer = %q / %q", model.editor.Value(), model.editor.Placeholder)
	}
	if !strings.Contains(model.View(), "1 field changes") {
		t.Fatalf("amendment diff was not rendered:\n%s", model.View())
	}
}

func TestWorkforceAuthoringPersistsChangeSetsAndRefinesByParent(t *testing.T) {
	createResult := authoring.CompileResult{
		Valid: true,
		Candidate: authoring.WorkforceCandidate{Team: &kernelteam.Definition{
			ID: "research", Version: "1", DisplayName: "Research Team", Purpose: "Find customer pain points",
			Roles:        []kernelteam.RoleSlot{{ID: "researcher", DisplayName: "Researcher", Purpose: "Gather evidence", MinimumMembers: 1}},
			Coordination: kernelteam.CoordinationPolicy{Mode: kernelteam.CoordinationDynamic},
			Approvals:    kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
		}},
	}
	amendResult := createResult
	amendResult.Diff = []authoring.FieldDiff{{Path: "team.approvals", AfterDigest: "approval-required"}}
	created := &authoring.ChangeSet{
		ID: "change-create", Scope: capability.ScopeReference{Kind: "local", ID: "default"},
		Mode: authoring.ModeCreate, Result: createResult, Status: authoring.ChangeSetReview, Revision: 1,
	}
	amended := &authoring.ChangeSet{
		ID: "change-amend", ParentID: created.ID, Scope: created.Scope,
		Mode: authoring.ModeAmend, Result: amendResult, Status: authoring.ChangeSetReview, Revision: 1,
	}
	fake := &fakeKernelClient{
		document:        kernelapi.NewCapabilityDocument(kernelapi.WorkforceAuthoringCapability(kernelapi.WorkforceAuthoringCapabilityFeatures{ChangeSets: true})),
		changeSets:      []*authoring.ChangeSet{created, amended},
		changeSetErrors: []error{errors.New("temporary disconnect"), nil},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())

	model.editor.SetValue("Create a customer research Team")
	applyCommand(t, model, model.submitWorkforceAuthoring())
	if model.editor.Value() == "" || model.pendingAuthoringKey == "" {
		t.Fatal("failed proposal did not preserve prompt and idempotency key")
	}
	applyCommand(t, model, model.submitWorkforceAuthoring())
	if len(fake.changeSetKeys) != 2 || fake.changeSetKeys[0] == "" || fake.changeSetKeys[0] != fake.changeSetKeys[1] {
		t.Fatalf("change set retry keys = %#v", fake.changeSetKeys)
	}
	request := fake.changeSetRequests[0]
	if request.Scope != created.Scope || request.Actor.Type != "user" || request.Actor.ID != "local" || request.ParentID != "" {
		t.Fatalf("create change set request = %#v", request)
	}
	if len(fake.authoringRequests) != 0 || model.authoringChangeSet == nil || model.authoringChangeSet.ID != created.ID {
		t.Fatalf("authoring used ephemeral compilation or lost change set: requests=%#v changeSet=%#v", fake.authoringRequests, model.authoringChangeSet)
	}
	if !strings.Contains(model.View(), "change-create") || !strings.Contains(model.View(), "review") {
		t.Fatalf("durable change set identity was not rendered:\n%s", model.View())
	}

	model.editor.SetValue("Require approval before external outreach")
	applyCommand(t, model, model.submitWorkforceAuthoring())
	if len(fake.changeSetRequests) != 3 || fake.changeSetRequests[2].ParentID != created.ID || fake.changeSetKeys[2] == fake.changeSetKeys[1] {
		t.Fatalf("refinement did not create a child change set: requests=%#v keys=%#v", fake.changeSetRequests, fake.changeSetKeys)
	}
	if model.authoringChangeSet == nil || model.authoringChangeSet.ID != amended.ID || !model.authoringAmendment || !strings.Contains(model.View(), "1 field changes") {
		t.Fatalf("durable refinement was not rendered: changeSet=%#v\n%s", model.authoringChangeSet, model.View())
	}
}

func TestWorkforceGovernanceSelectsExactRequirementAndAppliesWithStableRetries(t *testing.T) {
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	evaluation := authoring.ChangeSetEvaluation{ID: "evaluation-1", Allowed: true, ApprovalRequirements: []authoring.ChangeSetApprovalRequirement{{PolicyID: "production", Role: "operator", Count: 1}, {PolicyID: "outreach", Role: "reviewer", Count: 1}}}
	awaiting := &authoring.ChangeSet{ID: "change-1", Scope: scope, Status: authoring.ChangeSetAwaitingApproval, Revision: 2, CandidateDigest: "digest-1", Evaluations: []authoring.ChangeSetEvaluation{evaluation}, Result: authoring.CompileResult{Valid: true}}
	ready := *awaiting
	ready.Status, ready.Revision = authoring.ChangeSetReady, 3
	applied := ready
	applied.Status, applied.Revision = authoring.ChangeSetApplied, 4
	applied.ApplyReceipt = &authoring.ChangeSetApplyReceipt{ID: "receipt-1", CandidateDigest: ready.CandidateDigest, Reason: "Create the reviewed workforce", Actor: authoring.ChangeSetActor{Type: "user", ID: "server-operator"}, AppliedAt: time.Now(), Resources: []authoring.AppliedResourceReference{{Kind: "agent_definition", ID: "researcher", Version: "1"}, {Kind: "team_deployment", ID: "research-live", Revision: 1}}}
	approvalCapability := kernelapi.WorkforceAuthoringCapability(kernelapi.WorkforceAuthoringCapabilityFeatures{ChangeSets: true})
	approvalCapability.Operations = append(approvalCapability.Operations, kernelapi.OperationApprove)
	approvalCapability.Context = &kernelapi.CapabilityContext{ChangeSetID: awaiting.ID, Revision: awaiting.Revision, EligibleApprovalRequirements: []kernelapi.ApprovalRequirementReference{
		{EvaluationID: evaluation.ID, PolicyID: "production", Role: "operator"},
		{EvaluationID: evaluation.ID, PolicyID: "outreach", Role: "reviewer"},
	}}
	fake := &fakeKernelClient{
		document:          kernelapi.NewCapabilityDocument(approvalCapability),
		governanceErrors:  []error{errors.New("temporary disconnect"), nil, nil},
		governanceResults: []*authoring.ChangeSet{&ready, &applied},
	}
	model := newTestModel(t, fake)
	model.authoringChangeSet, model.authoringResult = awaiting, &awaiting.Result
	applyCommand(t, model, model.loadCapabilities())
	if !strings.Contains(model.View(), "outreach / reviewer") || !strings.Contains(model.View(), "production / operator") {
		t.Fatalf("eligible requirements not rendered:\n%s", model.View())
	}
	model.focusPanelList()
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if model.authoringApprovalSelected != 1 {
		t.Fatalf("selected requirement = %d", model.authoringApprovalSelected)
	}
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model.editor.SetValue("Reviewed external outreach boundaries")
	applyCommand(t, model, model.submitWorkforceApproval(true))
	if model.editor.Value() == "" || model.pendingGovernanceKey == "" {
		t.Fatal("failed approval did not preserve reason and retry identity")
	}
	firstKey := model.pendingGovernanceKey
	applyCapability := kernelapi.WorkforceAuthoringCapability(kernelapi.WorkforceAuthoringCapabilityFeatures{ChangeSets: true})
	applyCapability.Operations = append(applyCapability.Operations, kernelapi.OperationApply)
	applyCapability.Context = &kernelapi.CapabilityContext{ChangeSetID: ready.ID, Revision: ready.Revision}
	fake.document = kernelapi.NewCapabilityDocument(applyCapability)
	applyCommand(t, model, model.submitWorkforceApproval(true))
	if len(fake.approvalRequests) != 2 || fake.approvalKeys[0] != firstKey || fake.approvalKeys[1] != firstKey || fake.approvalRequests[1].PolicyID != "outreach" || fake.approvalRequests[1].Role != "reviewer" || fake.approvalRequests[1].EvaluationID != evaluation.ID {
		t.Fatalf("approval requests=%#v keys=%#v", fake.approvalRequests, fake.approvalKeys)
	}
	if fake.approvalRequests[1].PolicyID == "production" {
		t.Fatal("TUI submitted an unselected requirement")
	}
	if !model.canApplyWorkforce() || !strings.Contains(model.View(), "Enter create this exact reviewed workforce") {
		t.Fatalf("ready Apply was not capability gated:\n%s", model.View())
	}
	_, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	model.editor.SetValue("Create the reviewed workforce")
	applyCommand(t, model, model.submitWorkforceApply())
	if len(fake.applyRequests) != 1 || fake.applyRequests[0].ExpectedRevision != ready.Revision || fake.applyRequests[0].CandidateDigest != ready.CandidateDigest || fake.applyRequests[0].Reason != "Create the reviewed workforce" || fake.applyKeys[0] == "" {
		t.Fatalf("Apply requests=%#v keys=%#v", fake.applyRequests, fake.applyKeys)
	}
	if !strings.Contains(model.View(), "Created atomically") || !strings.Contains(model.View(), "receipt-1") || !strings.Contains(model.View(), "research-live") {
		t.Fatalf("Apply receipt was not rendered:\n%s", model.View())
	}
}

func TestObjectivePortfolioCreateAndAmendUsePublicCapability(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.Capabilities()}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if !model.objectiveCapability.Available || model.section != sectionObjectives || !strings.Contains(model.View(), "Objective portfolio") {
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
	model.mode = modeObjectiveEdit
	model.focusComposerEditor()
	model.editor.SetValue("Monitor competitor pain points and publish a weekly cited report.")
	applyCommand(t, model, model.submitObjectiveAmendment())
	if len(fake.objectiveUpdates) != 1 || fake.objectiveUpdates[0].ExpectedRevision != 1 ||
		fake.objectiveUpdates[0].Goal == nil || !strings.Contains(*fake.objectiveUpdates[0].Goal, "weekly") {
		t.Fatalf("objective update = %#v", fake.objectiveUpdates)
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

func TestInitiativePortfolioComposesSelectedObjectiveAndPatchesLifecycle(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.Capabilities(), objectives: []*runtime.Objective{{ID: "objective-research", Scope: runtime.Scope{Kind: "local", ID: "default"}, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"}, Title: "Research customer pain", Goal: "Gather cited evidence", Status: runtime.ObjectiveStatusActive, Revision: 1}}}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if !model.initiativeCapability.Available {
		t.Fatal("Initiative capability was not discovered")
	}
	model.section, model.mode = sectionInitiatives, modeInitiativeCreate
	model.focusComposerEditor()
	model.editor.SetValue("Customer insight campaign\nCoordinate monitoring and a cited report.")
	applyCommand(t, model, model.submitInitiative())
	if len(fake.initiativeCreates) != 1 || fake.initiativeKeys[0] == "" || fake.initiativeCreates[0].Status != runtime.InitiativeStatusActive || len(fake.initiativeCreates[0].ObjectiveRefs) != 1 || fake.initiativeCreates[0].ObjectiveRefs[0] != "objective-research" {
		t.Fatalf("Initiative create=%#v keys=%#v", fake.initiativeCreates, fake.initiativeKeys)
	}
	if view := model.View(); !strings.Contains(view, "Initiative portfolio") || !strings.Contains(view, "1 objectives") {
		t.Fatalf("Initiative not rendered:\n%s", view)
	}
	model.mode = modeInitiativeEdit
	model.focusComposerEditor()
	model.editor.SetValue("Monitor evidence, coordinate outreach, and publish a cited report.")
	applyCommand(t, model, model.submitInitiativeAmendment())
	if len(fake.initiativeUpdates) != 1 || fake.initiativeUpdates[0].ExpectedRevision != 1 || fake.initiativeUpdates[0].Purpose == nil {
		t.Fatalf("Initiative update=%#v", fake.initiativeUpdates)
	}
	applyCommand(t, model, model.pauseOrResumeInitiative())
	if len(fake.initiativeUpdates) != 2 || fake.initiativeUpdates[1].ExpectedRevision != 2 || fake.initiativeUpdates[1].Status == nil || *fake.initiativeUpdates[1].Status != runtime.InitiativeStatusPaused {
		t.Fatalf("Initiative lifecycle=%#v", fake.initiativeUpdates)
	}
	second := &runtime.Objective{ID: "objective-outreach", Scope: fake.objectives[0].Scope, Owner: fake.objectives[0].Owner, Title: "Coordinate outreach", Goal: "Follow up safely", Status: runtime.ObjectiveStatusActive, Revision: 1}
	fake.objectives = append(fake.objectives, second)
	model.objectives = fake.objectives
	model.objectiveSelected = 1
	model.selectedObjective = second.ID
	applyCommand(t, model, model.toggleSelectedObjectiveLink())
	last := fake.initiativeUpdates[len(fake.initiativeUpdates)-1]
	if last.ObjectiveRefs == nil || len(*last.ObjectiveRefs) != 2 || (*last.ObjectiveRefs)[1] != second.ID {
		t.Fatalf("linked objectives=%#v", last.ObjectiveRefs)
	}
}

func TestInitiativePortfolioProjectsDurableSourceMonitorEvidence(t *testing.T) {
	scope := runtime.Scope{Kind: "local", ID: "default"}
	initiative := &runtime.Initiative{
		ID: "initiative-research", Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"},
		Title: "Market research", Purpose: "Monitor governed sources", Status: runtime.InitiativeStatusActive, Revision: 3,
		SourceMonitors: []runtime.SourceMonitorReference{{
			ID: "reddit-kubernetes", ObjectiveID: "objective-research", AssignedAgentID: "researcher",
			SkillID: "openseal.source", SkillVersion: "1.0.2", Action: "observe_feed",
			SourcePolicyRef: "public-reddit-research@2026-07-13", Deduplication: runtime.SourceMonitorDeduplicateStableSourceAndContent,
		}},
	}
	nextEvaluation := time.Now().Add(5 * time.Minute)
	objective := &runtime.Objective{
		ID: "objective-research", Scope: scope, Owner: initiative.Owner, Title: "Monitor sources", Goal: "Collect evidence", Status: runtime.ObjectiveStatusActive,
		NextEvaluationAt: &nextEvaluation, ScheduleCondition: &runtime.ObjectiveScheduleCondition{
			State: runtime.ObjectiveScheduleBackpressured, Reason: "Maximum concurrent Runs are already active", Since: time.Now().Add(-time.Minute), UpdatedAt: time.Now(),
		},
	}
	key := sourceMonitorStatusKey(initiative.ID, "reddit-kubernetes")
	fake := &fakeKernelClient{
		document: kernelapi.Capabilities(), initiatives: []*runtime.Initiative{initiative}, objectives: []*runtime.Objective{objective},
		monitorCheckpoints: map[string]*runtime.SourceMonitorCheckpoint{key: {
			Scope: scope, InitiativeID: initiative.ID, MonitorID: "reddit-kubernetes", LastRunID: "run-live-123",
			ObservationCount: 5, LastSuccessAt: time.Now().Add(-time.Minute), Revision: 2,
		}},
		monitorObservations: map[string][]*runtime.SourceObservation{key: {{
			ID: "observation-1", Scope: scope, InitiativeID: initiative.ID, MonitorID: "reddit-kubernetes",
			Summary: "Operators want simpler upgrades", SourceURI: "https://www.reddit.com/r/kubernetes/comments/example",
			ArtifactRef: &runtime.ResourceReference{Kind: runtime.ResourceKindArtifact, ID: "captured-thread", Revision: 2},
		}}},
		activityPages: map[string]*runtime.ActivityFeedPage{"run-live-123": {Items: []runtime.ActivityProjection{{
			ID: "source-policy-call-1", EventType: "source_policy.authorized", InitiativeID: initiative.ID, RunID: "run-live-123", CreatedAt: time.Now().Add(-2 * time.Minute),
			Payload: map[string]interface{}{"monitorId": "reddit-kubernetes", "policyId": "public-reddit-research", "policyVersion": "2026-07-13", "sourceHost": "www.reddit.com", "pathPrefix": "/r/kubernetes", "maximumItems": float64(5)},
		}}}},
	}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.section = sectionInitiatives
	view := model.View()
	for _, expected := range []string{"reddit-kubernetes", "openseal.source@1.0.2", "public-reddit-research@2026-07-13", "Next evaluation", "Schedule backpressured", "Maximum concurrent Runs are", "5 evidence", "Authorized by public-reddit-research@2026-07-13", "www.reddit.com/r/kubernetes · up to 5 items", "Operators want simpler upgrades", "Artifact captured-thread · revision 2"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("Initiative monitor view missing %q:\n%s", expected, view)
		}
	}
	if len(fake.activityRequests) != 1 || fake.activityRequests[0].RunID != "run-live-123" || !fake.activityRequests[0].IncludeDetails || len(fake.activityRequests[0].EventTypes) != 1 || fake.activityRequests[0].EventTypes[0] != "source_policy.authorized" {
		t.Fatalf("activity requests=%#v", fake.activityRequests)
	}
}

func TestInitiativePolicyDecisionFlowsFromDurableActivityThroughPublicHTTPBoundary(t *testing.T) {
	store := runtime.NewMemoryStore(100)
	scope := runtime.Scope{Kind: "local", ID: "research"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "researcher"}
	portfolio := runtime.NewPortfolioService(store)
	objective, err := portfolio.CreateObjective(t.Context(), runtime.CreateObjectiveRequest{
		Scope: scope, Owner: owner, Title: "Monitor Kubernetes", Goal: "Collect governed evidence", Status: runtime.ObjectiveStatusActive,
		Cadence: &runtime.ObjectiveCadence{Type: runtime.ObjectiveCadenceInterval, IntervalSeconds: 60, AssignedAgentID: owner.ID, RunTemplate: &runtime.ObjectiveRunTemplate{
			Context:    map[string]interface{}{"initiativeId": "initiative-policy", "sourceMonitorId": "reddit-kubernetes"},
			Policy:     map[string]interface{}{"sourcePolicyRef": "public-reddit@1"},
			Capability: &runtime.ObjectiveCapabilityInvocation{SkillID: "openseal.source", SkillVersion: "1.0.2", Action: "observe_feed"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	initiative, _, err := runtime.NewInitiativeService(store, store).Create(t.Context(), runtime.CreateInitiativeRequest{Initiative: &runtime.Initiative{
		ID: "initiative-policy", Scope: scope, Owner: owner, Title: "Governed research", Purpose: "Collect permitted evidence", Status: runtime.InitiativeStatusActive,
		ObjectiveRefs: []string{objective.ID}, SourceMonitors: []runtime.SourceMonitorReference{{
			ID: "reddit-kubernetes", ObjectiveID: objective.ID, AssignedAgentID: owner.ID, SkillID: "openseal.source", SkillVersion: "1.0.2", Action: "observe_feed",
			SourcePolicyRef: "public-reddit@1", Deduplication: runtime.SourceMonitorDeduplicateStableSourceAndContent,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.CreateAgentRun(t.Context(), runtime.CreateAgentRunRequest{
		Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: owner.ID, Goal: objective.Goal, Source: runtime.RunSourceSchedule,
		Context: map[string]interface{}{"initiativeId": initiative.ID, "sourceMonitorId": "reddit-kubernetes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.NewSourceMonitorService(store, store, store, store).AdvanceCheckpoint(t.Context(), runtime.AdvanceSourceMonitorCheckpointRequest{
		Scope: scope, InitiativeID: initiative.ID, MonitorID: "reddit-kubernetes", RunID: run.ID, AgentID: owner.ID,
		SkillID: "openseal.source", SkillVersion: "1.0.2", Action: "observe_feed", ActionCallID: "call-policy",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.NewRunActivityService(store, store).AppendActivity(t.Context(), &runtime.ActivityEvent{
		ID: "source-policy-call-policy", Scope: scope, InitiativeID: initiative.ID, ObjectiveID: objective.ID, RunID: run.ID, AgentID: owner.ID,
		EventType: "source_policy.authorized", Severity: runtime.ActivitySeverityInfo, Actor: runtime.ActivityActor{Type: "system", ID: "source-policy"},
		Summary: "Source access authorized by policy", Visibility: runtime.ActivityVisibilityScope, CreatedAt: time.Now().UTC(),
		Payload: map[string]interface{}{"monitorId": "reddit-kubernetes", "actionCallId": "call-policy", "policyId": "public-reddit", "policyVersion": "1", "sourceHost": "www.reddit.com", "pathPrefix": "/r/kubernetes", "maximumItems": 5},
	}); err != nil {
		t.Fatal(err)
	}
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	httpClient := client.NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	config := DefaultConfig()
	config.Endpoint, config.Scope, config.Owner, config.PollInterval = httpServer.URL, scope, owner, -1
	model, err := NewModel(t.Context(), httpClient, config)
	if err != nil {
		t.Fatal(err)
	}
	model.width, model.height = 120, 40
	applyCommand(t, model, model.loadCapabilities())
	model.section = sectionInitiatives
	view := model.View()
	for _, expected := range []string{"Last success", "Authorized by public-reddit@1", "www.reddit.com/r/kubernetes · up to 5 items"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("HTTP-backed policy decision missing %q:\n%s", expected, view)
		}
	}
}

func TestInitiativeCreationRequiresRealObjective(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.Capabilities()}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	model.mode = modeInitiativeCreate
	model.editor.SetValue("Unbound campaign")
	if command := model.submitInitiative(); command != nil {
		t.Fatal("created Initiative without an Objective")
	}
	if !strings.Contains(model.status, "Objective") || len(fake.initiativeCreates) != 0 {
		t.Fatalf("status=%q creates=%#v", model.status, fake.initiativeCreates)
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
		Classification: runtime.ArtifactClassificationInternal, CreatedAt: time.Now(),
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

func TestTeamChannelsRequireAdvertisedCapabilityAndConcreteClient(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.ChannelsCapability(kernelapi.ChannelCapabilityFeatures{Coordination: true, Changes: true}))}
	model := newTestModel(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.ready || strings.Contains(model.View(), "c Channels") {
		t.Fatalf("channel controls rendered without a channel client:\n%s", model.View())
	}
}

func TestTeamChannelProjectionShowsMessagesPresenceAndArbitrationAudit(t *testing.T) {
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
		ID: "round-1", CommittedAt: time.Now(), Arbitration: runtime.ConversationArbitration{
			RoundID: "round-1", Decisions: []runtime.ParticipationDecision{{
				ProposalID: "observer", Participant: runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: "observer"},
				Disposition: runtime.ParticipationSilent, Score: 0, Reasons: []runtime.ParticipationReason{runtime.ParticipationReasonNoNewInformation},
			}},
		},
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
	for _, expected := range []string{"agent:observer", "silent", "no new information"} {
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
	store := runtime.NewMemoryStore(100)
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
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
	store := runtime.NewMemoryStore(100)
	scope := runtime.Scope{Kind: "tenant", ID: "one"}
	source, err := runtime.NewPortfolioService(store).CreateAgentRun(t.Context(), runtime.CreateAgentRunRequest{
		Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Ship the release", Source: runtime.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
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
	store := runtime.NewMemoryStore(100)
	scope := runtime.Scope{Kind: "tenant", ID: "one"}
	now := time.Now().UTC()
	run, err := runtime.NewPortfolioService(store).CreateAgentRun(t.Context(), runtime.CreateAgentRunRequest{
		Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"}, AssignedAgentID: "operator",
		Goal: "Publish the release", Source: runtime.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	approval := &runtime.ApprovalCheckpoint{
		ID: "approval-publish", Scope: scope, RunID: run.ID, ActionCallID: "call-publish", Status: runtime.ApprovalStatusPending,
		Risk: "production", Summary: "Publish release announcement", PolicyReason: "External publication requires review",
		ProposedAction:    map[string]interface{}{"skillId": "publisher", "skillVersion": "1", "action": "publish"},
		EligibleApprovers: []runtime.ApprovalPrincipal{{Type: "user", ID: "local"}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call := &runtime.ActionCall{
		ID: "call-publish", Scope: scope, RunID: run.ID, DeploymentID: "operator", SkillID: "publisher", SkillVersion: "1", Action: "publish",
		Status: runtime.ActionCallStatusWaitingApproval, Risk: "production", SideEffect: "external", IdempotencyKey: "publish-release",
		ApprovalID: approval.ID, MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call.InvocationDigest = runtime.ComputeActionInvocationDigest(call)
	waitingRun := *run
	waitingRun.Status = runtime.AgentRunStatusWaitingForApproval
	waitingRun.WakeCondition = &runtime.WakeCondition{Type: "approval", Reference: approval.ID}
	waitingRun.Revision++
	waitingRun.UpdatedAt = now
	if _, err := store.CreateActionProposal(t.Context(), runtime.ActionProposalRecord{
		Call: call, Approval: approval, Run: &waitingRun, ExpectedRunRevision: run.Revision,
		Event: &runtime.ActivityEvent{ID: "event-approval", Scope: scope, RunID: run.ID, EventType: "action.approval_requested", Summary: "Approval requested", CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	api := server.NewServer(nil, nil, store, zap.NewNop().Sugar())
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
	if err != nil || restoredRun.Status != runtime.AgentRunStatusWaitingForDependency || restoredRun.WakeCondition == nil || restoredRun.WakeCondition.Reference != call.ID {
		t.Fatalf("approved Run was not resumed for action execution: %#v, %v", restoredRun, err)
	}
}

func newTestModel(t *testing.T, fake *fakeKernelClient) *Model {
	return newModelWithClient(t, fake)
}

func TestRuntimeReadinessRendersLatestCompilationTruth(t *testing.T) {
	fake := &fakeKernelClient{
		document: kernelapi.NewCapabilityDocument(kernelapi.AgentDefinitionsCapability()),
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
	for _, expected := range []string{"Native runtime readiness", "NEEDS ATTENTION", "Reddit action is unavailable", "Node reddit", "candidate was not activated"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("readiness view missing %q:\n%s", expected, view)
		}
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
