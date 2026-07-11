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
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
	tea "github.com/charmbracelet/bubbletea"
	"go.uber.org/zap"
)

type fakeKernelClient struct {
	document          kernelapi.CapabilityDocument
	runs              []*runtime.AgentRun
	createErrors      []error
	createKeys        []string
	createRequests    []kernelapi.CreateAgentRunRequest
	objectives        []*runtime.Objective
	objectiveKeys     []string
	objectiveCreates  []kernelapi.CreateObjectiveRequest
	objectiveUpdates  []kernelapi.UpdateObjectiveRequest
	commands          []kernelapi.AgentRunCommandRequest
	artifacts         []*runtime.Artifact
	downloadBody      string
	downloadCalls     int
	authoringResult   *authoring.CompileResult
	authoringRequests []authoring.GenerateRequest
	authoringErrors   []error
	changeSets        []*authoring.ChangeSet
	changeSetRequests []authoring.CreateChangeSetRequest
	changeSetKeys     []string
	changeSetErrors   []error
	approvalRequests  []authoring.ResolveChangeSetApprovalRequest
	approvalKeys      []string
	applyRequests     []authoring.ApplyChangeSetRequest
	applyKeys         []string
	retryRequests     []authoring.RetryChangeSetGenerationRequest
	retryKeys         []string
	governanceResults []*authoring.ChangeSet
	governanceErrors  []error
}

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

func TestModelDiscoversCapabilitiesBeforeRenderingActions(t *testing.T) {
	limited := kernelapi.Capability{
		ID: kernelapi.AgentRunsCapabilityID, Version: kernelapi.AgentRunsCapabilityVersion,
		Available: true, Operations: []string{kernelapi.OperationCreate, kernelapi.OperationList},
	}
	fake := &fakeKernelClient{
		document: kernelapi.CapabilityDocument{Version: kernelapi.Version, Capabilities: []kernelapi.Capability{limited}},
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
		document:        kernelapi.NewCapabilityDocument(kernelapi.WorkforceAuthoringCapability(), kernelapi.ObjectivesCapability()),
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
		document:        kernelapi.NewCapabilityDocument(kernelapi.WorkforceAuthoringCapability(true)),
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
	approvalCapability := kernelapi.WorkforceAuthoringCapability(true)
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
	applyCapability := kernelapi.WorkforceAuthoringCapability(true)
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

func TestContractMismatchFailsClosed(t *testing.T) {
	fake := &fakeKernelClient{document: kernelapi.CapabilityDocument{Version: "99"}}
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
	fake := &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.TeamChannelsCapability())}
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
		fakeKernelClient: &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.TeamChannelsCapability())},
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
			kernelapi.AgentRunsCapability(), kernelapi.TeamChannelsCapability(),
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
		fakeKernelClient:         &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.TeamChannelsCapability())},
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

func newTestModel(t *testing.T, fake *fakeKernelClient) *Model {
	return newModelWithClient(t, fake)
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
