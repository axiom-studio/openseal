// Package tui implements OpenSeal's prompt-first terminal client. It owns no
// durable state: every action is discovered from and sent to the public kernel
// API used by other embedding surfaces.
package tui

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	kernelbundle "github.com/axiom-studio/openseal/pkg/bundle"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/uuid"
)

type Config struct {
	Endpoint     string
	Scope        runtime.Scope
	Owner        runtime.ObjectiveOwner
	Actor        runtime.ActivityActor
	DownloadDir  string
	PollInterval time.Duration
}

type runbookOperationFocus struct {
	AgentID        string
	AgentName      string
	RunbookID      string
	RunbookVersion string
	Entrypoint     string
}

type runbookOperationRow struct {
	Focus      runbookOperationFocus
	Definition *kernelagent.AgentDefinition
	FirstStep  string
}

func DefaultConfig() Config {
	return Config{
		Endpoint:     client.DefaultKernelBaseURL,
		Scope:        runtime.Scope{Kind: "local", ID: "default"},
		Owner:        runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "operator"},
		Actor:        runtime.ActivityActor{Type: "user", ID: "local"},
		DownloadDir:  "artifacts",
		PollInterval: 5 * time.Second,
	}
}

func (c Config) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if err := c.Owner.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Endpoint) == "" {
		return errors.New("OpenSeal API endpoint is required")
	}
	if strings.TrimSpace(c.DownloadDir) == "" {
		return errors.New("artifact download directory is required")
	}
	return nil
}

type focusArea int

const (
	focusComposer focusArea = iota
	focusPanel
)

type panelSection int

const (
	sectionOverview panelSection = iota
	sectionAuthoring
	sectionBundles
	sectionReadiness
	sectionTeams
	sectionObjectives
	sectionIntegrations
	sectionSkills
	sectionRuns
	sectionRequests
	sectionApprovals
	sectionActivity
	sectionArtifacts
	sectionChannels
)

type editorMode int

const (
	modeCreate editorMode = iota
	modeAgentBuilderPrompt
	modeWorkforceBundleInspect
	modeGuide
	modeRunbookOperationStart
	modeChannelCreate
	modeChannelPost
	modeObjectiveCreate
	modeObjectiveEdit
	modeIntegrationCreate
	modeIntegrationActivate
	modeIntegrationPause
	modeIntegrationRetire
	modeSkillBindingUpsert
	modeSkillBindingDisable
	modeSkillBindingUpgradePlan
	modeSkillBindingUpgradeApply
	modeRequestCreate
	modeRequestAccept
	modeRequestReject
	modeRequestClarify
	modeRequestProvideClarification
	modeRequestComplete
	modeApprovalApprove
	modeApprovalReject
	modeAgentAmendmentPropose
	modeAgentAmendmentEvaluate
	modeAgentAmendmentApprove
	modeAgentAmendmentReject
	modeAgentAmendmentActivate
	modeTeamAmendmentPropose
	modeTeamAmendmentEvaluate
	modeTeamAmendmentApprove
	modeTeamAmendmentReject
	modeTeamAmendmentActivate
)

type Model struct {
	ctx                                context.Context
	client                             client.KernelClient
	agentCapabilityClient              client.AgentDefinitionCapabilityClient
	agentLifecycleClient               client.AgentDefinitionLifecycleClient
	conversationClient                 client.ConversationClient
	conversationGatewayClient          client.ExternalConversationGatewayClient
	skillBindingClient                 client.SkillBindingClient
	workforceBundleClient              client.WorkforceBundleClient
	agentBuilderClient                 agentBuilderConversationClient
	config                             Config
	editor                             textarea.Model
	focus                              focusArea
	section                            panelSection
	mode                               editorMode
	width                              int
	height                             int
	loading                            bool
	busy                               bool
	ready                              bool
	showHelp                           bool
	unavailable                        string
	err                                error
	status                             string
	runCapability                      kernelapi.Capability
	agentTurnCapability                kernelapi.Capability
	actionCallCapability               kernelapi.Capability
	requestCapability                  kernelapi.Capability
	approvalCapability                 kernelapi.Capability
	objectiveCapability                kernelapi.Capability
	runbookCapability                  kernelapi.Capability
	conversationGatewayCapability      kernelapi.Capability
	activityCapability                 kernelapi.Capability
	skillActionCapability              kernelapi.Capability
	skillBindingCapability             kernelapi.Capability
	artifactCapability                 kernelapi.Capability
	channelCapability                  kernelapi.Capability
	agentBuilderCapability             kernelapi.Capability
	workforceBundleCapability          kernelapi.Capability
	agentDefinitionCapability          kernelapi.Capability
	teamDefinitionCapability           kernelapi.Capability
	activeRunbookSelected              int
	runs                               []*runtime.AgentRun
	agentTurns                         []*kernelapi.AgentTurnRecord
	agentTurnsRunID                    string
	agentTurnsErr                      error
	loadingAgentTurns                  bool
	actionCalls                        []*runtime.ActionCall
	actionCallsRunID                   string
	actionCallsErr                     error
	loadingActionCalls                 bool
	agentRequests                      []*runtime.AgentRequest
	agentRequestSelected               int
	selectedAgentRequest               string
	actionApprovals                    []*runtime.ApprovalCheckpoint
	actionApprovalSelected             int
	selectedActionApproval             string
	activity                           []runtime.ActivityProjection
	activitySelected                   int
	selectedActivity                   string
	activityExpanded                   bool
	activityNextCursor                 string
	activityHasMore                    bool
	compilations                       []*kernelagent.DefinitionCompilation
	agentDeployment                    *kernelapi.AgentDeploymentCatalogEntry
	agentAmendments                    []*kernelagent.DefinitionAmendment
	agentAmendmentSelected             int
	selectedAgentAmendment             string
	teamDeployments                    []kernelapi.TeamDeploymentCatalogEntry
	teamDeploymentSelected             int
	selectedTeamDeployment             string
	teamAmendments                     []*kernelteam.DefinitionAmendment
	teamAmendmentSelected              int
	selectedTeamAmendment              string
	objectives                         []*runtime.Objective
	runbooks                           []*runtime.RunbookActivation
	objectiveSelected                  int
	selectedObjective                  string
	runbookSelected                    int
	selectedRunbook                    string
	runbookDetail                      *runtime.RunbookDetail
	runbookDetailExpanded              bool
	conversationGateways               []*runtime.ExternalConversationGatewayRegistration
	conversationGatewaySelected        int
	selectedConversationGateway        string
	conversationGatewayAdapterSelected int
	skillActions                       []capability.ModelAction
	skillBindings                      []*capability.Binding
	skillBindingUpgradePlan            *runtime.SkillReferenceUpgradePlan
	skillBindingSelected               int
	selectedSkillBinding               string
	selected                           int
	selectedID                         string
	artifacts                          []*runtime.Artifact
	artifactSelected                   int
	selectedArtifact                   string
	artifactExpanded                   bool
	conversations                      []*runtime.Conversation
	conversationSelected               int
	selectedConversation               string
	channelMessages                    []*runtime.ChannelMessage
	channelRounds                      []*runtime.ParticipationRoundResult
	channelPresence                    []*runtime.ConversationPresence
	channelAuditExpanded               bool
	pendingKey                         string
	pendingGoal                        string
	workforceBundleInspection          *kernelbundle.Inspection
	workforceBundlePath                string
	pendingObjectiveKey                string
	pendingObjectivePrompt             string
	pendingConversationKey             string
	pendingConversationTitle           string
	pendingAgentBuilderKey             string
	pendingAgentBuilderPrompt          string
	// builderConversation is the builder chat opened from "New agent". It is
	// owned by the builder Agent, so the owner-scoped channel and approval
	// lists add it (and its cards) explicitly.
	builderConversation         *runtime.Conversation
	pendingMessageKey           string
	pendingMessageContent       string
	pendingMessageChannelID     string
	pendingAgentRequestKey      string
	pendingAgentRequestPrompt   string
	pendingAgentRequestSourceID string
	pendingRequestCompletionKey string
	pendingRequestCompletionID  string
	pendingApprovalKey          string
	pendingApprovalIntent       string
}

type capabilitiesLoaded struct {
	document kernelapi.CapabilityDocument
	err      error
}

type workforceBundleInspected struct {
	path       string
	inspection *kernelbundle.Inspection
	err        error
}

type teamDeploymentsLoaded struct {
	items []kernelapi.TeamDeploymentCatalogEntry
	err   error
}

type teamDeploymentUpdated struct {
	result *kernelapi.TeamDeploymentResult
	err    error
}

type teamAmendmentsLoaded struct {
	deploymentID string
	items        []*kernelteam.DefinitionAmendment
	err          error
}

type teamAmendmentChanged struct {
	amendment  *kernelteam.DefinitionAmendment
	deployment *kernelteam.Deployment
	action     string
	err        error
}

type runsLoaded struct {
	runs []*runtime.AgentRun
	err  error
}

type actionCallsLoaded struct {
	runID string
	calls []*runtime.ActionCall
	err   error
}

type agentTurnsLoaded struct {
	runID string
	turns []*kernelapi.AgentTurnRecord
	err   error
}

type agentRequestsLoaded struct {
	requests []*runtime.AgentRequest
	err      error
}

type agentRequestCreated struct {
	result *runtime.AgentRequestResult
	err    error
}

type agentRequestResponded struct {
	result *runtime.AgentRequestResult
	action string
	err    error
}

type agentRequestCompleted struct {
	result *runtime.AgentRequestResult
	err    error
}

type actionApprovalsLoaded struct {
	approvals []*runtime.ApprovalCheckpoint
	err       error
}

type actionApprovalResolved struct {
	result *runtime.ApprovalResolutionResult
	action string
	err    error
}

type activityLoaded struct {
	page   *runtime.ActivityFeedPage
	append bool
	err    error
}

type activityDetailLoaded struct {
	activity *runtime.ActivityProjection
	err      error
}

type compilationsLoaded struct {
	compilations []*kernelagent.DefinitionCompilation
	deployment   *kernelapi.AgentDeploymentCatalogEntry
	amendments   []*kernelagent.DefinitionAmendment
	err          error
}

type agentDeploymentUpdated struct {
	result *kernelapi.AgentDeploymentUpdateResult
	err    error
}

type agentAmendmentChanged struct {
	amendment  *kernelagent.DefinitionAmendment
	deployment *kernelagent.AgentDeployment
	action     string
	err        error
}

type objectivesLoaded struct {
	objectives []*runtime.Objective
	runbooks   []*runtime.RunbookActivation
	err        error
}

type runbookDetailLoaded struct {
	id     string
	detail *runtime.RunbookDetail
	err    error
}

type runbookChanged struct {
	activation *runtime.RunbookActivation
	action     string
	err        error
}

type runbookStarted struct {
	result *runtime.AgentRunCommandResult
	err    error
}

type objectiveCreated struct {
	objective *runtime.Objective
	err       error
}

type objectiveUpdated struct {
	objective *runtime.Objective
	err       error
}

type runbookSchedulesReconciled struct {
	reconciliation *kernelapi.RunbookScheduleReconciliation
	err            error
}

type skillActionsLoaded struct {
	actions []capability.ModelAction
	err     error
}
type skillBindingsLoaded struct {
	bindings []*capability.Binding
	err      error
}

type skillBindingChanged struct {
	binding *capability.Binding
	action  string
	err     error
}
type skillBindingUpgradePlanned struct {
	plan *runtime.SkillReferenceUpgradePlan
	err  error
}
type skillBindingUpgradeApplied struct {
	receipt *runtime.SkillReferenceUpgradeReceipt
	err     error
}

type artifactsLoaded struct {
	artifacts []*runtime.Artifact
	err       error
}

type conversationsLoaded struct {
	conversations []*runtime.Conversation
	err           error
}

type channelDetailLoaded struct {
	conversationID string
	messages       []*runtime.ChannelMessage
	rounds         []*runtime.ParticipationRoundResult
	presence       []*runtime.ConversationPresence
	err            error
}

// agentBuilderConversationClient starts a builder conversation: the "New
// agent" request becomes the first message of a chat with the builder Agent.
type agentBuilderConversationClient interface {
	StartAgentBuilderConversation(context.Context, kernelapi.StartAgentBuilderConversationRequest) (*kernelapi.StartAgentBuilderConversationResponse, error)
}

const agentBuilderPromptPlaceholder = "Describe the agent you need…"

type agentBuilderConversationStarted struct {
	response *kernelapi.StartAgentBuilderConversationResponse
	err      error
}

type conversationCreated struct {
	conversation *runtime.Conversation
	err          error
}

type channelMessagePosted struct {
	result *runtime.ChannelMessageCommitResult
	err    error
}

type conversationCursorAdvanced struct {
	conversationID string
	err            error
}

type artifactDownloaded struct {
	path string
	err  error
}

type runCreated struct {
	result *runtime.AgentRunCommandResult
	err    error
}

type runCommanded struct {
	result *runtime.AgentRunCommandResult
	kind   runtime.AgentRunCommandKind
	err    error
}

type pollTick time.Time

func NewModel(ctx context.Context, kernelClient client.KernelClient, config Config) (*Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if kernelClient == nil {
		return nil, errors.New("kernel client is required")
	}
	if config.PollInterval == 0 {
		config.PollInterval = 5 * time.Second
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	editor := textarea.New()
	editor.Placeholder = "Describe the outcome you want…"
	editor.Prompt = "│ "
	editor.ShowLineNumbers = false
	editor.CharLimit = 8_000
	editor.MaxHeight = 8
	editor.SetHeight(4)
	editor.SetWidth(48)
	editor.Focus()
	return &Model{
		ctx: ctx, client: kernelClient, config: config, editor: editor,
		focus: focusComposer, section: sectionOverview, mode: modeAgentBuilderPrompt, width: 100, height: 30,
		agentLifecycleClient:      agentLifecycleClient(kernelClient),
		agentCapabilityClient:     agentCapabilityClient(kernelClient),
		conversationClient:        conversationClient(kernelClient),
		conversationGatewayClient: conversationGatewayClient(kernelClient),
		skillBindingClient:        skillBindingClient(kernelClient),
		workforceBundleClient:     workforceBundleClient(kernelClient),
		agentBuilderClient:        agentBuilderClient(kernelClient),
	}, nil
}

func Run(ctx context.Context, kernelClient client.KernelClient, config Config) error {
	model, err := NewModel(ctx, kernelClient, config)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(model, tea.WithContext(ctx), tea.WithAltScreen()).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.loadCapabilities(), m.poll())
}

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(msg.Width, 40), max(msg.Height, 16)
		m.editor.SetWidth(max(m.composerWidth()-6, 24))
		return m, nil
	case capabilitiesLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			m.ready = false
			return m, nil
		}
		if msg.document.APIVersion != kernelapi.APIVersion {
			m.unavailable = fmt.Sprintf("Server contract %s is not supported by this TUI (requires %s).", msg.document.APIVersion, kernelapi.APIVersion)
			m.ready = false
			return m, nil
		}
		runCapability, hasRuns := msg.document.Find(kernelapi.AgentRunsCapabilityID, kernelapi.AgentRunsCapabilityVersion)
		agentTurnCapability, hasAgentTurns := msg.document.Find(kernelapi.AgentTurnsCapabilityID, kernelapi.AgentTurnsCapabilityVersion)
		actionCallCapability, hasActionCalls := msg.document.Find(kernelapi.ActionCallsCapabilityID, kernelapi.ActionCallsCapabilityVersion)
		requestCapability, hasRequests := msg.document.Find(kernelapi.AgentRequestsCapabilityID, kernelapi.AgentRequestsCapabilityVersion)
		approvalCapability, hasApprovals := msg.document.Find(kernelapi.ActionApprovalsCapabilityID, kernelapi.ActionApprovalsCapabilityVersion)
		objectiveCapability, hasObjectives := msg.document.Find(kernelapi.ObjectivesCapabilityID, kernelapi.ObjectivesCapabilityVersion)
		runbookCapability, hasRunbooks := msg.document.Find(kernelapi.RunbooksCapabilityID, kernelapi.RunbooksCapabilityVersion)
		conversationGatewayCapability, hasConversationGateways := msg.document.Find(kernelapi.ConversationGatewaysCapabilityID, kernelapi.ConversationGatewaysCapabilityVersion)
		activityCapability, hasActivity := msg.document.Find(kernelapi.ActivityCapabilityID, kernelapi.ActivityCapabilityVersion)
		skillActionCapability, hasSkillActions := msg.document.Find(kernelapi.SkillActionsCapabilityID, kernelapi.SkillActionsCapabilityVersion)
		skillBindingCapability, hasSkillBindings := msg.document.Find(kernelapi.SkillBindingsCapabilityID, kernelapi.SkillBindingsCapabilityVersion)
		artifactCapability, hasArtifacts := msg.document.Find(kernelapi.ArtifactsCapabilityID, kernelapi.ArtifactsCapabilityVersion)
		channelCapability, hasChannels := msg.document.Find(kernelapi.ChannelsCapabilityID, kernelapi.ChannelsCapabilityVersion)
		agentBuilderCapability, hasAgentBuilder := msg.document.Find(kernelapi.AgentBuilderCapabilityID, kernelapi.AgentBuilderCapabilityVersion)
		workforceBundleCapability, hasWorkforceBundles := msg.document.Find(kernelapi.WorkforceBundlesCapabilityID, kernelapi.WorkforceBundlesCapabilityVersion)
		agentDefinitionCapability, hasAgentDefinitions := msg.document.Find(kernelapi.AgentDefinitionsCapabilityID, kernelapi.AgentDefinitionsCapabilityVersion)
		teamDefinitionCapability, hasTeamDefinitions := msg.document.Find(kernelapi.TeamDefinitionsCapabilityID, kernelapi.TeamDefinitionsCapabilityVersion)
		m.runCapability = runCapability
		m.agentTurnCapability = agentTurnCapability
		m.actionCallCapability = actionCallCapability
		m.requestCapability = requestCapability
		m.approvalCapability = approvalCapability
		m.objectiveCapability = objectiveCapability
		m.runbookCapability = runbookCapability
		m.conversationGatewayCapability = conversationGatewayCapability
		m.activityCapability = activityCapability
		m.skillActionCapability = skillActionCapability
		m.skillBindingCapability = skillBindingCapability
		m.artifactCapability = artifactCapability
		m.channelCapability = channelCapability
		m.agentBuilderCapability = agentBuilderCapability
		m.workforceBundleCapability = workforceBundleCapability
		m.agentDefinitionCapability = agentDefinitionCapability
		m.teamDefinitionCapability = teamDefinitionCapability
		if !hasRuns || !runCapability.Available {
			m.runCapability = kernelapi.Capability{}
		}
		if !hasAgentTurns || !agentTurnCapability.Available || !agentTurnCapability.Supports(kernelapi.OperationList) {
			m.agentTurnCapability = kernelapi.Capability{}
			m.agentTurns, m.agentTurnsRunID, m.agentTurnsErr = nil, "", nil
			m.loadingAgentTurns = false
		}
		if !hasActionCalls || !actionCallCapability.Available || !actionCallCapability.Supports(kernelapi.OperationList) {
			m.actionCallCapability = kernelapi.Capability{}
			m.actionCalls, m.actionCallsRunID, m.actionCallsErr = nil, "", nil
		}
		if !hasRequests || !requestCapability.Available {
			m.requestCapability = kernelapi.Capability{}
		}
		if !hasApprovals || !approvalCapability.Available {
			m.approvalCapability = kernelapi.Capability{}
		}
		if !hasObjectives || !objectiveCapability.Available {
			m.objectiveCapability = kernelapi.Capability{}
		}
		if !hasRunbooks || !runbookCapability.Available {
			m.runbookCapability = kernelapi.Capability{}
		}
		if !hasConversationGateways || !conversationGatewayCapability.Available || !conversationGatewayCapability.Supports(kernelapi.OperationList) || m.conversationGatewayClient == nil {
			m.conversationGatewayCapability = kernelapi.Capability{}
		}
		if !hasActivity || !activityCapability.Available {
			m.activityCapability = kernelapi.Capability{}
		}
		if !hasSkillActions || !skillActionCapability.Available {
			m.skillActionCapability = kernelapi.Capability{}
		}
		if !hasSkillBindings || !skillBindingCapability.Available || m.skillBindingClient == nil || (m.config.Owner.Type != runtime.OwnerTypeAgent && m.config.Owner.Type != runtime.OwnerTypeTeam) {
			m.skillBindingCapability = kernelapi.Capability{}
		}
		if !hasArtifacts || !artifactCapability.Available {
			m.artifactCapability = kernelapi.Capability{}
		}
		if !hasChannels || !channelCapability.Available || m.conversationClient == nil {
			m.channelCapability = kernelapi.Capability{}
		}
		if !hasAgentBuilder || !agentBuilderCapability.Available || m.agentBuilderClient == nil {
			m.agentBuilderCapability = kernelapi.Capability{}
		}
		if !hasWorkforceBundles || !workforceBundleCapability.Available || !workforceBundleCapability.Supports(kernelapi.OperationInspect) || m.workforceBundleClient == nil {
			m.workforceBundleCapability = kernelapi.Capability{}
			m.workforceBundleInspection = nil
			m.workforceBundlePath = ""
		}
		if !hasAgentDefinitions || !agentDefinitionCapability.Available || m.config.Owner.Type != runtime.OwnerTypeAgent {
			m.agentDefinitionCapability = kernelapi.Capability{}
		}
		if !hasTeamDefinitions || !teamDefinitionCapability.Available {
			m.teamDefinitionCapability = kernelapi.Capability{}
		}
		if !m.objectiveCapability.Available && !m.conversationGatewayCapability.Available && !m.skillActionCapability.Available && !m.skillBindingCapability.Available && !m.runCapability.Available && !m.requestCapability.Available && !m.approvalCapability.Available && !m.activityCapability.Available && !m.artifactCapability.Available && !m.channelCapability.Available && !m.agentBuilderCapability.Available && !m.workforceBundleCapability.Available && !m.agentDefinitionCapability.Available && !m.teamDefinitionCapability.Available {
			m.unavailable = "This server does not advertise an agent builder, objectives, canonical work, requests, approvals, activity, Team channels, or artifact evidence."
			m.ready = false
			return m, nil
		}
		m.ready = true
		m.unavailable = ""
		m.err = nil
		if m.agentBuilderCapability.Available {
			m.section = sectionAuthoring
			// A full standalone workspace starts on the guided Home page.
			// Narrow embedded clients still open directly on their one useful
			// capability, preserving focused and backwards-compatible behavior.
			if len(m.availableSections()) >= 7 {
				m.section = sectionOverview
			}
			m.mode = modeAgentBuilderPrompt
			m.editor.Placeholder = agentBuilderPromptPlaceholder
		} else if m.workforceBundleCapability.Available {
			m.section = sectionBundles
			m.mode = modeWorkforceBundleInspect
			m.editor.Placeholder = "Path to a signed workforce bundle…"
		} else if m.objectiveCapability.Available {
			m.section = sectionObjectives
			m.mode = modeObjectiveCreate
			m.editor.Placeholder = "Describe the objective and desired outcome…"
		} else if m.conversationGatewayCapability.Available {
			m.section = sectionIntegrations
			m.focusPanelList()
		} else if m.skillActionCapability.Available || m.skillBindingCapability.Available {
			m.section = sectionSkills
			m.focusPanelList()
		} else if !m.objectiveCapability.Available && m.runCapability.Available {
			m.section = sectionRuns
			m.mode = modeCreate
		} else if m.requestCapability.Available {
			m.section = sectionRequests
			m.focusPanelList()
		} else if m.approvalCapability.Available {
			m.section = sectionApprovals
			m.focusPanelList()
		} else if m.activityCapability.Available {
			m.section = sectionActivity
			m.focusPanelList()
		} else if !m.objectiveCapability.Available && !m.runCapability.Available && m.channelCapability.Available {
			m.section = sectionChannels
			m.focusPanelList()
		} else if !m.objectiveCapability.Available && !m.runCapability.Available && m.artifactCapability.Available {
			m.section = sectionArtifacts
			m.focusPanelList()
		} else if m.agentDefinitionCapability.Available {
			m.section = sectionReadiness
			m.focusPanelList()
		} else if m.teamDefinitionCapability.Available {
			m.section = sectionTeams
			m.focusPanelList()
		}
		if len(m.availableSections()) >= 7 {
			m.section = sectionOverview
			m.focusPanelList()
			m.resetComposerMode()
		}
		return m, tea.Batch(m.loadCompilations(), m.loadTeamDeployments(), m.loadObjectives(), m.loadConversationGateways(), m.loadSkillBindings(), m.loadSkillActions(), m.loadRuns(), m.loadAgentRequests(), m.loadActionApprovals(), m.loadActivity(false), m.loadArtifacts(), m.loadConversations())
	case workforceBundleInspected:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Bundle inspection failed. The selected artifact was not changed."
			return m, nil
		}
		m.err = nil
		m.workforceBundlePath = msg.path
		m.workforceBundleInspection = msg.inspection
		m.status = "Portable workforce bundle validated and inspected by the kernel."
		m.editor.Reset()
		m.resetComposerMode()
		m.focusPanelList()
		return m, nil
	case teamDeploymentsLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.teamDeployments = msg.items
		m.restoreTeamDeploymentSelection()
		return m, m.loadTeamAmendments()
	case teamAmendmentsLoaded:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		if deployment := m.selectedTeamDeploymentRecord(); deployment == nil || deployment.Deployment == nil || deployment.Deployment.ID != msg.deploymentID {
			return m, nil
		}
		m.err = nil
		m.teamAmendments = msg.items
		m.restoreTeamAmendmentSelection()
		return m, nil
	case teamDeploymentUpdated:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Team lifecycle update failed. Refreshing current state."
			return m, m.loadTeamDeployments()
		}
		m.err = nil
		if msg.result != nil && msg.result.Deployment != nil {
			m.selectedTeamDeployment = msg.result.Deployment.ID
			m.status = fmt.Sprintf("Team is now %s at revision %d.", msg.result.Deployment.Status, msg.result.Deployment.Revision)
		}
		return m, m.loadTeamDeployments()
	case teamAmendmentChanged:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = msg.action + " failed. Refreshing authoritative Team governance state."
			return m, tea.Batch(m.loadTeamDeployments(), m.loadTeamAmendments())
		}
		m.err = nil
		m.editor.Reset()
		m.resetComposerMode()
		m.focusPanelList()
		if msg.amendment != nil {
			m.selectedTeamAmendment = msg.amendment.ID
			m.status = fmt.Sprintf("%s recorded · %s · revision %d.", msg.action, msg.amendment.Status, msg.amendment.Revision)
		}
		if msg.deployment != nil {
			m.selectedTeamDeployment = msg.deployment.ID
		}
		return m, m.loadTeamDeployments()
	case objectivesLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.objectives = msg.objectives
		m.runbooks = msg.runbooks
		m.restoreObjectiveSelection()
		m.restoreRunbookSelection()
		return m, nil
	case runbookDetailLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "The reviewed Runbook could not be loaded."
			return m, nil
		}
		m.err = nil
		if msg.id == m.selectedRunbook {
			m.runbookDetail = msg.detail
		}
		return m, nil
	case runbookChanged:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = msg.action + " failed. Authoritative Runbook state was preserved."
			return m, m.loadObjectives()
		}
		m.err = nil
		if msg.activation != nil {
			m.selectedRunbook = msg.activation.ID
			m.status = fmt.Sprintf("Runbook %s · revision %d.", msg.action, msg.activation.Revision)
		}
		return m, m.loadObjectives()
	case runbookStarted:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Runbook start failed. No client-authored Run was substituted."
			return m, nil
		}
		m.err = nil
		if msg.result != nil && msg.result.Run != nil {
			m.status = fmt.Sprintf("Runbook started · Run %s.", msg.result.Run.ID)
		} else {
			m.status = "Runbook started."
		}
		return m, tea.Batch(m.loadObjectives(), m.loadRuns())
	case runbookSchedulesReconciled:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Schedule reconciliation failed. No local schedule state was changed."
			return m, nil
		}
		m.err = nil
		if msg.reconciliation != nil && msg.reconciliation.Result != nil {
			m.status = fmt.Sprintf("Schedules reconciled · %d examined · %d Runs scheduled.", msg.reconciliation.Result.Examined, msg.reconciliation.Result.Scheduled)
		} else {
			m.status = "Schedules reconciled."
		}
		return m, tea.Batch(m.loadObjectives(), m.loadRuns())
	case skillActionsLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.skillActions = msg.actions
		return m, nil
	case skillBindingsLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err, m.skillBindings = nil, msg.bindings
		m.restoreSkillBindingSelection()
		return m, nil
	case skillBindingChanged:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			if isHTTPStatus(msg.err, http.StatusConflict) {
				m.status = "This binding changed elsewhere. Latest state reloaded; review and retry."
				return m, m.loadSkillBindings()
			}
			m.status = "Skill binding change failed. Your draft is preserved for retry."
			return m, nil
		}
		m.err = nil
		m.editor.Reset()
		m.resetComposerMode()
		m.focusPanelList()
		if msg.binding != nil {
			m.selectedSkillBinding = msg.binding.ID
			m.status = fmt.Sprintf("Skill binding %s · revision %d.", msg.action, msg.binding.Revision)
		}
		return m, tea.Batch(m.loadSkillBindings(), m.loadSkillActions())
	case skillBindingUpgradePlanned:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Skill update review failed. The requested target is preserved for retry."
			return m, nil
		}
		m.err, m.skillBindingUpgradePlan = nil, msg.plan
		m.mode = modeSkillBindingUpgradeApply
		m.editor.Reset()
		if msg.plan != nil && msg.plan.ApprovalRequired {
			m.editor.SetValue("reason: reviewed every affected reference\napproval: record why the widened authority is approved")
		} else {
			m.editor.SetValue("reason: reviewed every affected reference")
		}
		m.editor.Placeholder = "reason: why this exact update should be applied\napproval: required only when authority widens"
		m.focusComposerEditor()
		m.status = "Update plan ready. Review its complete impact, record the audit reason, then apply."
		return m, nil
	case skillBindingUpgradeApplied:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			if isHTTPStatus(msg.err, http.StatusConflict) {
				m.status = "This binding or dependent work changed. Latest state reloaded; request a new update plan."
				m.skillBindingUpgradePlan = nil
				return m, m.loadSkillBindings()
			}
			m.status = "Skill update was not applied. The reviewed plan is preserved for retry."
			return m, nil
		}
		m.err, m.skillBindingUpgradePlan = nil, nil
		m.editor.Reset()
		m.resetComposerMode()
		m.focusPanelList()
		if msg.receipt != nil {
			m.selectedSkillBinding = msg.receipt.BindingID
			m.status = fmt.Sprintf("Skill updated to %s@%s · binding revision %d.", msg.receipt.To.ID, msg.receipt.To.Version, msg.receipt.BindingRevision)
		}
		return m, tea.Batch(m.loadSkillBindings(), m.loadSkillActions(), m.loadObjectives(), m.loadActivity(false))
	case runsLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.runs = msg.runs
		m.restoreSelection()
		return m, m.loadSelectedRunHistory()
	case agentTurnsLoaded:
		if run := m.selectedRun(); run == nil || run.ID != msg.runID {
			return m, nil
		}
		m.loadingAgentTurns = false
		m.agentTurnsRunID = msg.runID
		m.agentTurns = msg.turns
		m.agentTurnsErr = msg.err
		return m, nil
	case actionCallsLoaded:
		if run := m.selectedRun(); run == nil || run.ID != msg.runID {
			return m, nil
		}
		m.loadingActionCalls = false
		m.actionCallsRunID = msg.runID
		m.actionCalls = msg.calls
		m.actionCallsErr = msg.err
		return m, nil
	case agentRequestsLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.agentRequests = msg.requests
		m.restoreAgentRequestSelection()
		return m, nil
	case agentRequestCreated:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Request creation failed. The draft and retry identity are preserved."
			return m, m.loadAgentRequests()
		}
		m.err = nil
		m.pendingAgentRequestKey, m.pendingAgentRequestPrompt, m.pendingAgentRequestSourceID = "", "", ""
		m.editor.Reset()
		if msg.result != nil && msg.result.Request != nil {
			m.selectedAgentRequest = msg.result.Request.ID
		}
		m.status = "Collaboration request created with durable Run lineage."
		m.resetComposerMode()
		m.focusPanelList()
		return m, tea.Batch(m.loadAgentRequests(), m.loadRuns())
	case agentRequestResponded:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = msg.action + " failed. The response draft is preserved for retry."
			return m, m.loadAgentRequests()
		}
		m.err = nil
		m.editor.Reset()
		if msg.result != nil && msg.result.Request != nil {
			m.selectedAgentRequest = msg.result.Request.ID
		}
		m.status = msg.action + " recorded in the durable collaboration audit."
		m.resetComposerMode()
		m.focusPanelList()
		return m, tea.Batch(m.loadAgentRequests(), m.loadRuns())
	case agentRequestCompleted:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Request completion failed. The summary and retry identity are preserved."
			return m, m.loadAgentRequests()
		}
		m.err = nil
		m.pendingRequestCompletionKey, m.pendingRequestCompletionID = "", ""
		m.editor.Reset()
		if msg.result != nil && msg.result.Request != nil {
			m.selectedAgentRequest = msg.result.Request.ID
		}
		m.status = "Completion recorded and the requesting work was resumed."
		m.resetComposerMode()
		m.focusPanelList()
		return m, tea.Batch(m.loadAgentRequests(), m.loadRuns(), m.loadArtifacts())
	case actionApprovalsLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.actionApprovals = msg.approvals
		m.restoreActionApprovalSelection()
		return m, nil
	case actionApprovalResolved:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = msg.action + " failed. The reason and retry identity are preserved."
			return m, m.loadActionApprovals()
		}
		m.err = nil
		m.pendingApprovalKey, m.pendingApprovalIntent = "", ""
		m.editor.Reset()
		if msg.result != nil && msg.result.Approval != nil {
			m.selectedActionApproval = msg.result.Approval.ID
		}
		m.status = msg.action + " recorded. The governed work was resumed."
		m.resetComposerMode()
		m.focusPanelList()
		return m, tea.Batch(m.loadActionApprovals(), m.loadRuns(), m.loadActivity(false))
	case activityLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.mergeActivityPage(msg.page, msg.append)
		m.restoreActivitySelection()
		return m, nil
	case activityDetailLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		if msg.activity != nil {
			for index := range m.activity {
				if m.activity[index].ID == msg.activity.ID {
					m.activity[index] = *msg.activity
					m.activityExpanded = true
					break
				}
			}
		}
		return m, nil
	case compilationsLoaded:
		m.loading = false
		if msg.err != nil {
			if isHTTPStatus(msg.err, http.StatusNotFound) {
				m.agentDefinitionCapability = kernelapi.Capability{}
				m.compilations = nil
				if m.section == sectionReadiness {
					m.section = m.defaultOperationalSection()
				}
				return m, nil
			}
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.compilations = msg.compilations
		m.agentDeployment = msg.deployment
		m.agentAmendments = msg.amendments
		m.restoreAgentAmendmentSelection()
		return m, nil
	case agentDeploymentUpdated:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Agent deployment changed elsewhere or the update violates its definition bounds. Refresh and try again."
			return m, m.loadCompilations()
		}
		m.err = nil
		if msg.result != nil && msg.result.Deployment != nil {
			if m.agentDeployment == nil {
				m.agentDeployment = &kernelapi.AgentDeploymentCatalogEntry{}
			}
			m.agentDeployment.Deployment = msg.result.Deployment
		}
		m.status = "Agent deployment operating state updated with an immutable audit record."
		return m, nil
	case agentAmendmentChanged:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = msg.action + " failed. Refreshing authoritative Agent governance state."
			return m, m.loadCompilations()
		}
		m.err = nil
		m.editor.Reset()
		m.resetComposerMode()
		m.focusPanelList()
		if msg.amendment != nil {
			m.selectedAgentAmendment = msg.amendment.ID
			m.status = fmt.Sprintf("%s recorded · %s · revision %d.", msg.action, msg.amendment.Status, msg.amendment.Revision)
		}
		if msg.deployment != nil && m.agentDeployment != nil {
			m.agentDeployment.Deployment = msg.deployment
		}
		return m, m.loadCompilations()
	case artifactsLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.artifacts = msg.artifacts
		m.restoreArtifactSelection()
		return m, nil
	case conversationsLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.conversations = msg.conversations
		m.restoreConversationSelection()
		return m, m.loadSelectedConversation()
	case channelDetailLoaded:
		if selected := m.selectedConversationRecord(); selected == nil || selected.ID != msg.conversationID {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.channelMessages = msg.messages
		m.channelRounds = msg.rounds
		m.channelPresence = msg.presence
		if m.section == sectionChannels {
			return m, m.markSelectedConversationRead()
		}
		return m, nil
	case agentBuilderConversationStarted:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "The builder chat didn't start. Your request is kept for retry."
			return m, nil
		}
		if msg.response == nil || msg.response.Conversation == nil {
			m.err = errors.New("the builder returned no conversation")
			m.status = "The builder chat didn't start. Your request is kept for retry."
			return m, nil
		}
		m.err = nil
		m.pendingAgentBuilderKey, m.pendingAgentBuilderPrompt = "", ""
		m.editor.Reset()
		m.builderConversation = msg.response.Conversation
		m.selectedConversation = msg.response.Conversation.ID
		m.channelMessages, m.channelRounds, m.channelPresence = nil, nil, nil
		m.status = "Builder chat opened. Answer its questions here; approve its card in Approvals before anything is created."
		if !m.channelCapability.Available {
			return m, m.loadActionApprovals()
		}
		m.section = sectionChannels
		m.mode = modeChannelPost
		m.editor.Placeholder = "Reply to the builder…"
		m.focusPanelList()
		return m, tea.Batch(m.loadConversations(), m.loadActionApprovals())
	case conversationCreated:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Channel creation failed. The title is preserved for retry."
			return m, nil
		}
		m.err = nil
		m.pendingConversationKey, m.pendingConversationTitle = "", ""
		m.editor.Reset()
		m.selectedConversation = msg.conversation.ID
		m.status = "Team channel created."
		m.section = sectionChannels
		m.focusPanelList()
		return m, m.loadConversations()
	case channelMessagePosted:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Message was not sent. Its content is preserved for retry."
			return m, m.loadConversations()
		}
		m.err = nil
		m.pendingMessageKey, m.pendingMessageContent, m.pendingMessageChannelID = "", "", ""
		m.editor.Reset()
		m.mode = modeChannelPost
		m.selectedConversation = msg.result.Conversation.ID
		m.status = "Message posted to the durable Team channel."
		m.focusPanelList()
		return m, m.loadConversations()
	case conversationCursorAdvanced:
		if selected := m.selectedConversationRecord(); selected == nil || selected.ID != msg.conversationID {
			return m, nil
		}
		if msg.err != nil && !isCursorRefreshConflict(msg.err) {
			m.err = msg.err
		}
		return m, nil
	case artifactDownloaded:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Artifact download failed. No partial file was kept."
			return m, nil
		}
		m.err = nil
		m.status = "Artifact verified and saved to " + msg.path
		return m, nil
	case runCreated:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Creation failed. Your prompt is preserved for retry."
			return m, nil
		}
		m.err = nil
		m.pendingKey, m.pendingGoal = "", ""
		m.editor.Reset()
		m.selectedID = msg.result.Run.ID
		m.status = "Work started. It is now durable and safe to leave running."
		m.section = sectionRuns
		m.focusPanelList()
		return m, m.loadRuns()
	case objectiveCreated:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Objective creation failed. Your prompt is preserved for retry."
			return m, nil
		}
		m.err = nil
		m.pendingObjectiveKey, m.pendingObjectivePrompt = "", ""
		m.editor.Reset()
		m.selectedObjective = msg.objective.ID
		m.status = "Objective added to the durable portfolio."
		m.section = sectionObjectives
		m.focusPanelList()
		return m, m.loadObjectives()
	case objectiveUpdated:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "The objective changed elsewhere. Refresh and try again."
			return m, m.loadObjectives()
		}
		m.err = nil
		m.editor.Reset()
		m.selectedObjective = msg.objective.ID
		m.status = "Objective amended and revision recorded."
		m.resetComposerMode()
		m.focusPanelList()
		return m, m.loadObjectives()
	case conversationGatewaysLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err, m.conversationGateways = nil, msg.items
		m.restoreConversationGatewaySelection()
		return m, nil
	case conversationGatewayChanged:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = msg.action + " failed. Refreshing authoritative integration state."
			return m, m.loadConversationGateways()
		}
		m.err = nil
		m.editor.Reset()
		m.resetComposerMode()
		m.focusPanelList()
		if msg.gateway != nil {
			m.selectedConversationGateway = msg.gateway.ID
			m.status = fmt.Sprintf("%s recorded · %s · revision %d.", msg.action, msg.gateway.Status, msg.gateway.Revision)
		}
		return m, m.loadConversationGateways()
	case runCommanded:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "The run changed elsewhere. Refresh and try again."
			return m, m.loadRuns()
		}
		m.err = nil
		m.selectedID = msg.result.Run.ID
		m.status = commandSuccessMessage(msg.kind)
		if msg.kind == runtime.AgentRunCommandIntervene {
			m.mode = modeCreate
			m.editor.Reset()
			m.editor.Placeholder = "Describe the outcome you want…"
		}
		return m, m.loadRuns()
	case pollTick:
		commands := []tea.Cmd{m.poll()}
		if m.ready && !m.loading && !m.busy {
			commands = append(commands, m.loadCompilations(), m.loadTeamDeployments(), m.loadObjectives(), m.loadConversationGateways(), m.loadRuns(), m.loadAgentRequests(), m.loadActionApprovals(), m.loadActivity(false), m.loadArtifacts(), m.loadConversations())
		}
		return m, tea.Batch(commands...)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	if m.focus == focusComposer {
		var command tea.Cmd
		m.editor, command = m.editor.Update(message)
		return m, command
	}
	return m, nil
}

func (m *Model) View() string {
	return m.render()
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.showHelp {
		switch key {
		case "ctrl+c":
			return m, tea.Quit
		case "?", "esc", "enter":
			m.showHelp = false
			return m, nil
		}
		return m, nil
	}
	switch key {
	case "ctrl+c":
		return m, tea.Quit
	case "?":
		m.showHelp = true
		return m, nil
	case "tab", "ctrl+i":
		if m.focus == focusComposer {
			m.focusPanelList()
		} else {
			m.prepareComposerForSection()
		}
		return m, nil
	case "esc":
		if m.mode != modeCreate {
			if m.mode == modeSkillBindingUpgradePlan || m.mode == modeSkillBindingUpgradeApply {
				m.skillBindingUpgradePlan = nil
			}
			m.resetComposerMode()
			m.editor.Reset()
			m.status = "Draft canceled."
			return m, nil
		}
	case "ctrl+s":
		if m.focus == focusComposer {
			switch m.mode {
			case modeGuide:
				return m, m.submitGuidance()
			case modeRunbookOperationStart:
				return m, m.submitRunbookOperation()
			case modeChannelCreate:
				return m, m.submitConversation()
			case modeChannelPost:
				return m, m.submitChannelMessage()
			case modeObjectiveCreate:
				return m, m.submitObjective()
			case modeObjectiveEdit:
				return m, m.submitObjectiveAmendment()
			case modeIntegrationCreate:
				return m, m.submitConversationGatewayCreation()
			case modeIntegrationActivate:
				return m, m.submitConversationGatewayLifecycle(runtime.ExternalConversationGatewayActive, "activated")
			case modeIntegrationPause:
				return m, m.submitConversationGatewayLifecycle(runtime.ExternalConversationGatewayPaused, "paused")
			case modeIntegrationRetire:
				return m, m.submitConversationGatewayLifecycle(runtime.ExternalConversationGatewayRetired, "retired")
			case modeSkillBindingUpsert:
				return m, m.submitSkillBindingUpsert()
			case modeSkillBindingDisable:
				return m, m.submitSkillBindingDisable()
			case modeSkillBindingUpgradePlan:
				return m, m.submitSkillBindingUpgradePlan()
			case modeSkillBindingUpgradeApply:
				return m, m.submitSkillBindingUpgradeApply()
			case modeAgentBuilderPrompt:
				return m, m.submitAgentBuilderPrompt()
			case modeWorkforceBundleInspect:
				return m, m.submitWorkforceBundleInspection()
			case modeRequestCreate:
				return m, m.submitAgentRequestCreation()
			case modeRequestAccept:
				return m, m.submitAgentRequestResponse(runtime.AgentRequestDecisionAccept)
			case modeRequestReject:
				return m, m.submitAgentRequestResponse(runtime.AgentRequestDecisionReject)
			case modeRequestClarify:
				return m, m.submitAgentRequestResponse(runtime.AgentRequestDecisionRequestClarification)
			case modeRequestProvideClarification:
				return m, m.submitAgentRequestResponse(runtime.AgentRequestDecisionProvideClarification)
			case modeRequestComplete:
				return m, m.submitAgentRequestCompletion()
			case modeApprovalApprove:
				return m, m.submitActionApproval(true)
			case modeApprovalReject:
				return m, m.submitActionApproval(false)
			case modeAgentAmendmentPropose:
				return m, m.submitAgentAmendmentProposal()
			case modeAgentAmendmentEvaluate:
				return m, m.submitAgentAmendmentEvaluation()
			case modeAgentAmendmentApprove:
				return m, m.submitAgentAmendmentDecision(true)
			case modeAgentAmendmentReject:
				return m, m.submitAgentAmendmentDecision(false)
			case modeAgentAmendmentActivate:
				return m, m.submitAgentAmendmentActivation()
			case modeTeamAmendmentPropose:
				return m, m.submitTeamAmendmentProposal()
			case modeTeamAmendmentEvaluate:
				return m, m.submitTeamAmendmentEvaluation()
			case modeTeamAmendmentApprove:
				return m, m.submitTeamAmendmentDecision(true)
			case modeTeamAmendmentReject:
				return m, m.submitTeamAmendmentDecision(false)
			case modeTeamAmendmentActivate:
				return m, m.submitTeamAmendmentActivation()
			default:
				return m, m.submitRun()
			}
		}
	case "enter":
		if m.focus == focusComposer {
			switch m.mode {
			case modeAgentBuilderPrompt:
				return m, m.submitAgentBuilderPrompt()
			case modeChannelPost:
				return m, m.submitChannelMessage()
			}
		}
	}

	if m.focus == focusPanel {
		if key == "enter" && m.section == sectionOverview {
			if m.agentBuilderCapability.Available {
				m.section = sectionAuthoring
				m.mode = modeAgentBuilderPrompt
				m.editor.Placeholder = agentBuilderPromptPlaceholder
				m.focusComposerEditor()
			}
			return m, nil
		}
		switch key {
		case "up", "k":
			if m.section == sectionReadiness && len(m.activeRunbookOperations()) > 0 {
				m.moveActiveRunbookSelection(-1)
			} else {
				m.movePanelSelection(-1)
			}
			if m.section == sectionChannels {
				return m, m.loadSelectedConversation()
			} else if m.section == sectionTeams {
				return m, m.loadTeamAmendments()
			} else if m.section == sectionRuns {
				return m, m.loadSelectedRunHistory()
			}
		case "down", "j":
			if m.section == sectionReadiness && len(m.activeRunbookOperations()) > 0 {
				m.moveActiveRunbookSelection(1)
			} else {
				m.movePanelSelection(1)
			}
			if m.section == sectionChannels {
				return m, m.loadSelectedConversation()
			} else if m.section == sectionTeams {
				return m, m.loadTeamAmendments()
			} else if m.section == sectionRuns {
				return m, m.loadSelectedRunHistory()
			}
		case "left":
			m.moveSection(-1)
			m.resetComposerMode()
			return m, m.loadPanel()
		case "right":
			m.moveSection(1)
			m.resetComposerMode()
			return m, m.loadPanel()
		case "w":
			if m.runCapability.Available {
				m.section = sectionRuns
				return m, m.loadSelectedRunHistory()
			}
		case "R":
			if m.requestCapability.Available {
				m.section = sectionRequests
			}
		case "A":
			if m.approvalCapability.Available {
				m.section = sectionApprovals
			}
		case "t":
			if m.activityCapability.Available {
				m.section = sectionActivity
			}
		case "f":
			if m.agentBuilderCapability.Available {
				m.section = sectionAuthoring
			}
		case "B":
			if m.workforceBundleCapability.Available {
				m.section = sectionBundles
				m.resetComposerMode()
			}
		case "h":
			if m.agentDefinitionCapability.Available {
				m.section = sectionReadiness
				return m, m.loadCompilations()
			}
		case "T":
			if m.teamDefinitionCapability.Available {
				m.section = sectionTeams
				return m, m.loadTeamDeployments()
			}
		case "o":
			if m.objectiveCapability.Available {
				m.section = sectionObjectives
				return m, tea.Batch(m.loadObjectives(), m.loadRuns())
			}
		case "I":
			if m.conversationGatewayCapability.Available {
				m.section = sectionIntegrations
				return m, m.loadConversationGateways()
			}
		case "s":
			if m.skillActionCapability.Available || m.skillBindingCapability.Available {
				m.section = sectionSkills
				return m, tea.Batch(m.loadSkillBindings(), m.loadSkillActions())
			}
		case "a":
			if m.artifactCapability.Available {
				m.section = sectionArtifacts
			}
		case "c":
			if m.channelCapability.Available {
				m.section = sectionChannels
				return m, m.loadSelectedConversation()
			}
		case "n":
			if m.section == sectionReadiness && m.canStartActiveRunbookOperation() {
				m.prepareActiveRunbookOperation()
			} else if m.section == sectionRequests && m.canCreateAgentRequestFromSelectedRun() {
				m.mode = modeRequestCreate
				m.editor.Reset()
				m.editor.Placeholder = "First line: agent:researcher or handoff team:marketing\nRemaining lines: requested outcome"
				m.focusComposerEditor()
			} else if m.section == sectionChannels && m.supportsChannel(kernelapi.OperationCreate) {
				m.mode = modeChannelCreate
				m.editor.Reset()
				m.editor.Placeholder = "Name the Team channel…"
				m.focusComposerEditor()
			} else if m.section == sectionObjectives && m.supportsObjective(kernelapi.OperationCreate) {
				m.mode = modeObjectiveCreate
				m.editor.Reset()
				m.editor.Placeholder = "Describe the objective and desired outcome…"
				m.focusComposerEditor()
			} else if m.section == sectionIntegrations && m.canCreateConversationGateway() {
				m.prepareConversationGatewayCreation()
			} else if m.section == sectionRuns && m.supportsRun(kernelapi.OperationCreate) {
				m.mode = modeCreate
				m.editor.Placeholder = "Describe the outcome you want…"
				m.focusComposerEditor()
			}
		case "r":
			return m, m.loadPanel()
		case "[":
			if m.section == sectionReadiness {
				m.moveAgentAmendmentSelection(-1)
			} else if m.section == sectionTeams {
				m.moveTeamAmendmentSelection(-1)
			} else if m.section == sectionIntegrations {
				m.moveConversationGatewayAdapterChoice(-1)
			}
		case "]":
			if m.section == sectionReadiness {
				m.moveAgentAmendmentSelection(1)
			} else if m.section == sectionTeams {
				m.moveTeamAmendmentSelection(1)
			} else if m.section == sectionIntegrations {
				m.moveConversationGatewayAdapterChoice(1)
			}
		case "b":
			if m.section == sectionSkills && m.supportsSkillBinding(kernelapi.OperationUpsert) {
				m.prepareSkillBindingComposer(nil)
			}
		case "m":
			if m.section == sectionActivity && m.activityHasMore {
				return m, m.loadActivity(true)
			} else if m.section == sectionReadiness && m.canProposeAgentBehaviorAmendment() {
				m.prepareAgentAmendmentComposer(modeAgentAmendmentPropose, "systemPrompt|personality\nConcise rationale\nNew immutable value")
			} else if m.section == sectionTeams && m.canProposeTeamPurposeAmendment() {
				m.prepareTeamAmendmentComposer(modeTeamAmendmentPropose, "First line: concise rationale\nRemaining lines: the Team's new purpose")
			} else if m.section == sectionChannels && m.selectedConversationRecord() != nil && m.supportsChannel(kernelapi.OperationPost) {
				m.mode = modeChannelPost
				m.editor.Reset()
				m.editor.Placeholder = "Share an update or ask a question…"
				m.focusComposerEditor()
			}
		case "p":
			if m.section == sectionRuns {
				return m, m.pauseOrResume()
			} else if m.section == sectionObjectives {
				return m, m.pauseOrResumeSelectedRunbook()
			} else if m.section == sectionReadiness {
				return m, m.pauseOrResumeAgentDeployment()
			} else if m.section == sectionTeams {
				return m, m.pauseOrResumeTeamDeployment()
			} else if m.section == sectionIntegrations {
				if item := m.selectedConversationGatewayRecord(); item != nil {
					if item.Status == runtime.ExternalConversationGatewayActive {
						m.prepareConversationGatewayLifecycle(modeIntegrationPause)
					} else if item.Status == runtime.ExternalConversationGatewayPaused {
						m.prepareConversationGatewayLifecycle(modeIntegrationActivate)
					}
				}
			}
		case "x":
			if m.section == sectionRuns {
				return m, m.commandSelected(runtime.AgentRunCommandCancel, "")
			} else if m.section == sectionRequests && m.canRespondToSelectedRequest(runtime.AgentRequestDecisionReject) {
				m.prepareRequestComposer(modeRequestReject, "Explain why this request cannot be accepted…")
			} else if m.section == sectionApprovals && m.canResolveSelectedActionApproval() {
				m.prepareRequestComposer(modeApprovalReject, "Explain why this action must not proceed…")
			} else if m.section == sectionReadiness && m.canResolveSelectedAgentAmendment() {
				m.prepareAgentAmendmentComposer(modeAgentAmendmentReject, "Record why this exact Agent amendment must not proceed…")
			} else if m.section == sectionTeams && m.canResolveSelectedTeamAmendment() {
				m.prepareTeamAmendmentComposer(modeTeamAmendmentReject, "Record why this exact Team amendment must not proceed…")
			} else if m.section == sectionIntegrations {
				m.prepareConversationGatewayLifecycle(modeIntegrationRetire)
			} else if m.section == sectionSkills && m.selectedSkillBindingRecord() != nil && !m.selectedSkillBindingRecord().Disabled && m.supportsSkillBinding(kernelapi.OperationDisable) {
				m.mode = modeSkillBindingDisable
				m.editor.Reset()
				m.editor.Placeholder = "Why should this binding be disabled?"
				m.focusComposerEditor()
			}
		case "y":
			if m.section == sectionRequests && m.canRespondToSelectedRequest(runtime.AgentRequestDecisionAccept) {
				placeholder := "Optionally record a concise acceptance note…"
				if request := m.selectedAgentRequestRecord(); request != nil && request.Recipient.Type == runtime.OwnerTypeTeam {
					placeholder = "First line: assigned Agent ID\nOptional remaining lines: acceptance note"
				}
				m.prepareRequestComposer(modeRequestAccept, placeholder)
			} else if m.section == sectionApprovals && m.canResolveSelectedActionApproval() {
				m.prepareRequestComposer(modeApprovalApprove, "Record why this exact action is safe to approve…")
			} else if m.section == sectionReadiness && m.canResolveSelectedAgentAmendment() {
				m.prepareAgentAmendmentComposer(modeAgentAmendmentApprove, "Record why this exact Agent amendment is approved…")
			} else if m.section == sectionTeams && m.canResolveSelectedTeamAmendment() {
				m.prepareTeamAmendmentComposer(modeTeamAmendmentApprove, "Record why this exact Team amendment is approved…")
			}
		case "q":
			if m.section == sectionRequests && m.canRespondToSelectedRequest(runtime.AgentRequestDecisionRequestClarification) {
				m.prepareRequestComposer(modeRequestClarify, "Ask the requester for the missing information…")
			}
		case "M":
			if m.section == sectionRequests && m.canRespondToSelectedRequest(runtime.AgentRequestDecisionProvideClarification) {
				m.prepareRequestComposer(modeRequestProvideClarification, "Provide the clarification requested by the recipient…")
			}
		case "g":
			if m.section == sectionSkills && m.selectedSkillBindingRecord() != nil && m.supportsSkillBinding(kernelapi.OperationPlanUpgrade) {
				m.prepareSkillBindingUpgradeComposer()
			} else if m.section == sectionObjectives {
				return m, m.reconcileRunbookSchedules()
			} else if m.section == sectionRuns {
				if run := m.selectedRun(); run != nil && m.supportsRun(kernelapi.OperationIntervene) && !isTerminal(run.Status) {
					m.mode = modeGuide
					m.editor.Reset()
					m.editor.Placeholder = "Give concise guidance without replacing the objective…"
					m.focusComposerEditor()
				}
			}
		case "e", "enter":
			if m.section == sectionSkills && m.selectedSkillBindingRecord() != nil && m.supportsSkillBinding(kernelapi.OperationUpsert) {
				m.prepareSkillBindingComposer(m.selectedSkillBindingRecord())
			} else if m.section == sectionReadiness && m.canEvaluateSelectedAgentAmendment() {
				m.prepareAgentAmendmentComposer(modeAgentAmendmentEvaluate, agentEvaluationPlaceholder(m.agentDeployment))
			} else if m.section == sectionTeams && m.canEvaluateSelectedTeamAmendment() {
				m.prepareTeamAmendmentComposer(modeTeamAmendmentEvaluate, teamEvaluationPlaceholder(m.selectedTeamDeploymentRecord()))
			} else if m.section == sectionObjectives && key == "enter" && m.selectedRunbookRecord() != nil && m.supportsRunbook(kernelapi.OperationGet) {
				return m, m.toggleSelectedRunbookDetail()
			} else if m.section == sectionObjectives && m.selectedObjectiveRecord() != nil && m.supportsObjective(kernelapi.OperationUpdate) {
				m.mode = modeObjectiveEdit
				m.editor.Reset()
				m.editor.Placeholder = "Describe the amended objective…"
				m.focusComposerEditor()
			} else if m.section == sectionRequests && m.canCompleteSelectedAgentRequest() {
				m.prepareRequestComposer(modeRequestComplete, "Summarize the completed outcome…")
			} else if m.section == sectionActivity && m.selectedActivityRecord() != nil {
				if m.activityExpanded {
					m.activityExpanded = false
				} else if !m.selectedActivityRecord().DetailAvailable {
					m.activityExpanded = true
				} else {
					return m, m.loadSelectedActivityDetail()
				}
			} else if m.section == sectionArtifacts && m.selectedArtifactRecord() != nil {
				m.artifactExpanded = !m.artifactExpanded
			} else if m.section == sectionChannels && m.supportsChannel(kernelapi.OperationAudit) && len(m.channelRounds) > 0 {
				m.channelAuditExpanded = !m.channelAuditExpanded
			}
		case "d":
			if m.section == sectionArtifacts {
				return m, m.downloadSelectedArtifact()
			}
		case "u":
			if m.section == sectionObjectives {
				return m, m.startSelectedRunbook()
			}
		case "v":
			if m.section == sectionReadiness && m.canActivateSelectedAgentAmendment() {
				m.prepareAgentAmendmentComposer(modeAgentAmendmentActivate, "Record why this reviewed Agent definition should become active…")
			} else if m.section == sectionTeams && m.canActivateSelectedTeamAmendment() {
				m.prepareTeamAmendmentComposer(modeTeamAmendmentActivate, "Record why this reviewed Team definition should become active…")
			}
		}
		return m, nil
	}

	var command tea.Cmd
	m.editor, command = m.editor.Update(msg)
	return m, command
}

func (m *Model) loadCapabilities() tea.Cmd {
	m.loading = true
	return func() tea.Msg {
		var document kernelapi.CapabilityDocument
		var err error
		if m.config.Owner.Type == runtime.OwnerTypeAgent && m.agentCapabilityClient != nil {
			document, err = m.agentCapabilityClient.AgentDefinitionCapabilities(m.ctx, capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID}, m.config.Owner.ID)
		} else {
			document, err = m.client.Capabilities(m.ctx)
		}
		return capabilitiesLoaded{document: document, err: err}
	}
}

// submitAgentBuilderPrompt is "New agent": it opens a builder chat whose
// first message is the request and switches to that chat. The builder asks
// its questions there and proposes the agent as an approval card.
func (m *Model) submitAgentBuilderPrompt() tea.Cmd {
	prompt := strings.TrimSpace(m.editor.Value())
	if !m.supportsAgentBuilder() || m.busy || prompt == "" {
		if prompt == "" {
			m.status = "Describe the agent you need."
		}
		return nil
	}
	if m.pendingAgentBuilderKey == "" || m.pendingAgentBuilderPrompt != prompt {
		m.pendingAgentBuilderKey, m.pendingAgentBuilderPrompt = uuid.NewString(), prompt
	}
	request := kernelapi.StartAgentBuilderConversationRequest{
		Scope: m.config.Scope, Prompt: prompt,
		Parameters:     runtime.AgentBuilderParameters{Kind: runtime.AgentBuilderKindAgent},
		IdempotencyKey: m.pendingAgentBuilderKey,
	}
	m.busy, m.err = true, nil
	m.status = "Opening a chat with the builder…"
	builder := m.agentBuilderClient
	return func() tea.Msg {
		response, err := builder.StartAgentBuilderConversation(m.ctx, request)
		return agentBuilderConversationStarted{response: response, err: err}
	}
}

func (m *Model) submitWorkforceBundleInspection() tea.Cmd {
	path := strings.TrimSpace(m.editor.Value())
	if path == "" || m.busy || m.workforceBundleClient == nil || !m.workforceBundleCapability.Supports(kernelapi.OperationInspect) {
		return nil
	}
	m.busy = true
	return func() tea.Msg {
		data, err := os.ReadFile(path)
		if err != nil {
			return workforceBundleInspected{path: path, err: err}
		}
		artifact, err := kernelbundle.DecodeYAML(data)
		if err != nil {
			return workforceBundleInspected{path: path, err: err}
		}
		inspection, err := m.workforceBundleClient.InspectWorkforceBundle(m.ctx, artifact)
		return workforceBundleInspected{path: path, inspection: inspection, err: err}
	}
}

func (m *Model) loadRuns() tea.Cmd {
	if !m.supportsRun(kernelapi.OperationList) {
		return nil
	}
	m.loading = true
	owner := m.config.Owner
	return func() tea.Msg {
		owned, err := m.client.ListAgentRuns(m.ctx, runtime.AgentRunFilter{
			Scope: m.config.Scope, Owner: &m.config.Owner, Limit: 100,
		})
		if err != nil {
			return runsLoaded{err: err}
		}
		if owner.Type != runtime.OwnerTypeAgent {
			return runsLoaded{runs: owned}
		}
		assigned, err := m.client.ListAgentRuns(m.ctx, runtime.AgentRunFilter{
			Scope: m.config.Scope, AssignedAgentID: owner.ID, Limit: 100,
		})
		if err != nil {
			return runsLoaded{err: err}
		}
		byID := make(map[string]*runtime.AgentRun, len(owned)+len(assigned))
		for _, run := range append(owned, assigned...) {
			if run != nil {
				byID[run.ID] = run
			}
		}
		runs := make([]*runtime.AgentRun, 0, len(byID))
		for _, run := range byID {
			runs = append(runs, run)
		}
		sort.Slice(runs, func(i, j int) bool {
			if runs[i].UpdatedAt.Equal(runs[j].UpdatedAt) {
				return runs[i].ID < runs[j].ID
			}
			return runs[i].UpdatedAt.After(runs[j].UpdatedAt)
		})
		return runsLoaded{runs: runs}
	}
}

func (m *Model) loadSelectedRunHistory() tea.Cmd {
	return tea.Batch(m.loadSelectedAgentTurns(), m.loadSelectedActionCalls())
}

func (m *Model) loadSelectedAgentTurns() tea.Cmd {
	run := m.selectedRun()
	if run == nil || !m.agentTurnCapability.Supports(kernelapi.OperationList) {
		m.agentTurns, m.agentTurnsRunID, m.agentTurnsErr = nil, "", nil
		m.loadingAgentTurns = false
		return nil
	}
	runID := run.ID
	m.loadingAgentTurns = true
	return func() tea.Msg {
		turns, err := m.client.ListAgentTurns(m.ctx, runtime.AgentTurnFilter{
			Scope: m.config.Scope, RunID: runID, Limit: 100,
		})
		return agentTurnsLoaded{runID: runID, turns: turns, err: err}
	}
}

func (m *Model) loadSelectedActionCalls() tea.Cmd {
	run := m.selectedRun()
	if run == nil || !m.actionCallCapability.Supports(kernelapi.OperationList) {
		m.actionCalls, m.actionCallsRunID, m.actionCallsErr = nil, "", nil
		m.loadingActionCalls = false
		return nil
	}
	runID := run.ID
	m.loadingActionCalls = true
	return func() tea.Msg {
		calls, err := m.client.ListActionCalls(m.ctx, runtime.ActionFilter{
			Scope: m.config.Scope, RunID: runID, Limit: 100,
		})
		return actionCallsLoaded{runID: runID, calls: calls, err: err}
	}
}

func (m *Model) activityFeedRequest(cursor string, includeDetails bool, limit int) runtime.ActivityFeedRequest {
	request := runtime.ActivityFeedRequest{
		Scope: m.config.Scope, Cursor: cursor, IncludeDetails: includeDetails, Limit: limit,
	}
	if m.config.Owner.Type == runtime.OwnerTypeTeam {
		request.TeamID = m.config.Owner.ID
	} else {
		request.AgentID = m.config.Owner.ID
	}
	return request
}

func (m *Model) loadActivity(appendPage bool) tea.Cmd {
	if !m.activityCapability.Supports(kernelapi.OperationList) {
		return nil
	}
	cursor := ""
	if appendPage {
		cursor = m.activityNextCursor
		if cursor == "" {
			return nil
		}
	}
	m.loading = true
	request := m.activityFeedRequest(cursor, false, 25)
	return func() tea.Msg {
		page, err := m.client.ListActivity(m.ctx, request)
		return activityLoaded{page: page, append: appendPage, err: err}
	}
}

func (m *Model) loadSelectedActivityDetail() tea.Cmd {
	selected := m.selectedActivityRecord()
	if selected == nil || !m.activityCapability.Supports(kernelapi.OperationList) {
		return nil
	}
	m.loading = true
	targetID := selected.ID
	request := m.activityFeedRequest("", true, 100)
	if selected.RunID != "" {
		request.AgentID, request.TeamID, request.RunID = "", "", selected.RunID
	} else if selected.ObjectiveID != "" {
		request.AgentID, request.TeamID, request.ObjectiveID = "", "", selected.ObjectiveID
	}
	maximumPages := max(1, (len(m.activity)+request.Limit-1)/request.Limit+1)
	return func() tea.Msg {
		for pageNumber := 0; pageNumber < maximumPages; pageNumber++ {
			page, err := m.client.ListActivity(m.ctx, request)
			if err != nil {
				return activityDetailLoaded{err: err}
			}
			for index := range page.Items {
				if page.Items[index].ID == targetID {
					activity := page.Items[index]
					return activityDetailLoaded{activity: &activity}
				}
			}
			if !page.HasMore || page.NextCursor == "" {
				break
			}
			request.Cursor = page.NextCursor
		}
		return activityDetailLoaded{err: errors.New("selected activity detail is no longer available")}
	}
}

func (m *Model) mergeActivityPage(page *runtime.ActivityFeedPage, appendPage bool) {
	if page == nil {
		return
	}
	wasEmpty := len(m.activity) == 0
	byID := make(map[string]runtime.ActivityProjection, len(m.activity)+len(page.Items))
	for _, item := range m.activity {
		byID[item.ID] = item
	}
	for _, item := range page.Items {
		if existing, ok := byID[item.ID]; ok && activityProjectionHasDetails(existing) && !activityProjectionHasDetails(item) {
			continue
		}
		byID[item.ID] = item
	}
	m.activity = m.activity[:0]
	for _, item := range byID {
		m.activity = append(m.activity, item)
	}
	sort.Slice(m.activity, func(i, j int) bool {
		if m.activity[i].CreatedAt.Equal(m.activity[j].CreatedAt) {
			return m.activity[i].ID > m.activity[j].ID
		}
		return m.activity[i].CreatedAt.After(m.activity[j].CreatedAt)
	})
	if appendPage || wasEmpty {
		m.activityNextCursor = page.NextCursor
		m.activityHasMore = page.HasMore
	}
}

func activityProjectionHasDetails(item runtime.ActivityProjection) bool {
	return item.Payload != nil || len(item.ConversationRefs) > 0 || item.CorrelationID != "" || item.CausationID != ""
}

func (m *Model) loadAgentRequests() tea.Cmd {
	if !m.supportsAgentRequest(kernelapi.OperationList) {
		return nil
	}
	m.loading = true
	party := m.localCollaborationParty()
	return func() tea.Msg {
		outgoing, err := m.client.ListAgentRequests(m.ctx, runtime.AgentRequestFilter{
			Scope: m.config.Scope, Requester: &party, Limit: 100,
		})
		if err != nil {
			return agentRequestsLoaded{err: err}
		}
		incoming, err := m.client.ListAgentRequests(m.ctx, runtime.AgentRequestFilter{
			Scope: m.config.Scope, Recipient: &party, Limit: 100,
		})
		if err != nil {
			return agentRequestsLoaded{err: err}
		}
		byID := make(map[string]*runtime.AgentRequest, len(outgoing)+len(incoming))
		for _, request := range append(outgoing, incoming...) {
			if request != nil {
				byID[request.ID] = request
			}
		}
		requests := make([]*runtime.AgentRequest, 0, len(byID))
		for _, request := range byID {
			requests = append(requests, request)
		}
		sort.Slice(requests, func(i, j int) bool {
			if requests[i].UpdatedAt.Equal(requests[j].UpdatedAt) {
				return requests[i].ID < requests[j].ID
			}
			return requests[i].UpdatedAt.After(requests[j].UpdatedAt)
		})
		return agentRequestsLoaded{requests: requests}
	}
}

func (m *Model) loadActionApprovals() tea.Cmd {
	if !m.supportsActionApproval(kernelapi.OperationList) {
		return nil
	}
	m.loading = true
	owner := m.config.Owner
	builder := m.builderConversation
	return func() tea.Msg {
		approvals, err := m.client.ListActionApprovals(m.ctx, runtime.ApprovalFilter{
			Scope: m.config.Scope, Owner: &owner, Limit: 100,
		})
		if err != nil || builder == nil || builder.Owner == owner {
			return actionApprovalsLoaded{approvals: approvals, err: err}
		}
		// Builder cards belong to the builder Agent, not to this TUI's
		// owner; the person who asked for the agent approves them here.
		builderOwner := builder.Owner
		cards, err := m.client.ListActionApprovals(m.ctx, runtime.ApprovalFilter{
			Scope: builder.Scope, Owner: &builderOwner, Limit: 100,
		})
		if err != nil {
			return actionApprovalsLoaded{err: err}
		}
		return actionApprovalsLoaded{approvals: append(cards, approvals...)}
	}
}

func (m *Model) loadCompilations() tea.Cmd {
	if !m.supportsAgentDefinition(kernelapi.OperationGet) || m.config.Owner.Type != runtime.OwnerTypeAgent {
		return nil
	}
	m.loading = true
	return func() tea.Msg {
		scope := capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID}
		deployment, err := m.client.GetAgentDeployment(m.ctx, scope, m.config.Owner.ID)
		if err != nil {
			return compilationsLoaded{err: err}
		}
		var values []*kernelagent.DefinitionCompilation
		if m.supportsAgentDefinition(kernelapi.OperationListCompilations) {
			values, err = m.client.ListAgentDefinitionCompilations(m.ctx, scope, m.config.Owner.ID)
		}
		var amendments []*kernelagent.DefinitionAmendment
		if err == nil && m.supportsAgentDefinition(kernelapi.OperationListAmendments) && m.agentLifecycleClient != nil {
			var result *kernelapi.AgentDefinitionAmendmentList
			result, err = m.agentLifecycleClient.ListAgentDefinitionAmendments(m.ctx, scope, m.config.Owner.ID)
			if result != nil {
				amendments = result.Items
			}
		}
		return compilationsLoaded{compilations: values, deployment: deployment, amendments: amendments, err: err}
	}
}

func (m *Model) loadTeamDeployments() tea.Cmd {
	if !m.supportsTeamDefinition(kernelapi.OperationList) {
		return nil
	}
	m.loading = true
	scope := capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID}
	return func() tea.Msg {
		result, err := m.client.ListTeamDeployments(m.ctx, scope)
		if err != nil {
			return teamDeploymentsLoaded{err: err}
		}
		if result == nil {
			return teamDeploymentsLoaded{}
		}
		return teamDeploymentsLoaded{items: result.Items}
	}
}

func (m *Model) loadTeamAmendments() tea.Cmd {
	if !m.supportsTeamDefinition(kernelapi.OperationListAmendments) {
		m.teamAmendments = nil
		m.selectedTeamAmendment = ""
		m.teamAmendmentSelected = 0
		return nil
	}
	entry := m.selectedTeamDeploymentRecord()
	if entry == nil || entry.Deployment == nil {
		return nil
	}
	scope, deploymentID := entry.Deployment.Scope, entry.Deployment.ID
	return func() tea.Msg {
		result, err := m.client.ListTeamDefinitionAmendments(m.ctx, scope, deploymentID)
		if err != nil {
			return teamAmendmentsLoaded{deploymentID: deploymentID, err: err}
		}
		if result == nil {
			return teamAmendmentsLoaded{deploymentID: deploymentID}
		}
		return teamAmendmentsLoaded{deploymentID: deploymentID, items: result.Items}
	}
}

func (m *Model) loadObjectives() tea.Cmd {
	if !m.supportsObjective(kernelapi.OperationList) {
		return nil
	}
	m.loading = true
	return func() tea.Msg {
		objectives, err := m.client.ListObjectives(m.ctx, runtime.ObjectiveFilter{Scope: m.config.Scope, Owner: &m.config.Owner, Limit: 100})
		if err != nil {
			return objectivesLoaded{err: err}
		}
		var runbooks []*runtime.RunbookActivation
		if m.supportsRunbook(kernelapi.OperationList) {
			runbooks, err = m.client.ListRunbooks(m.ctx, runtime.RunbookActivationFilter{Scope: m.config.Scope, Owner: &m.config.Owner, Limit: 500})
		}
		return objectivesLoaded{objectives: objectives, runbooks: runbooks, err: err}
	}
}

func (m *Model) toggleSelectedRunbookDetail() tea.Cmd {
	selected := m.selectedRunbookRecord()
	if selected == nil || !m.supportsRunbook(kernelapi.OperationGet) {
		return nil
	}
	if m.runbookDetailExpanded {
		m.runbookDetailExpanded = false
		return nil
	}
	m.runbookDetailExpanded = true
	if m.runbookDetail != nil && m.runbookDetail.Activation != nil && m.runbookDetail.Activation.ID == selected.ID {
		return nil
	}
	m.loading = true
	id := selected.ID
	return func() tea.Msg {
		detail, err := m.client.GetRunbook(m.ctx, m.config.Scope, id)
		return runbookDetailLoaded{id: id, detail: detail, err: err}
	}
}

func (m *Model) startSelectedRunbook() tea.Cmd {
	selected := m.selectedRunbookRecord()
	if selected == nil || selected.Status != runtime.RunbookActivationActive || !m.supportsRunbook(kernelapi.OperationExecute) || m.busy {
		return nil
	}
	m.busy, m.err = true, nil
	m.status = "Starting the exact reviewed Runbook…"
	id := selected.ID
	return func() tea.Msg {
		result, err := m.client.StartRunbook(m.ctx, m.config.Scope, id, runtime.StartRunbookActivationRequest{
			IdempotencyKey: uuid.NewString(),
			Actor:          m.config.Actor,
		})
		return runbookStarted{result: result, err: err}
	}
}

func (m *Model) pauseOrResumeSelectedRunbook() tea.Cmd {
	selected := m.selectedRunbookRecord()
	if selected == nil || selected.Status == runtime.RunbookActivationRetired || !m.supportsRunbook(kernelapi.OperationUpdate) || m.busy {
		return nil
	}
	status, action := runtime.RunbookActivationActive, "resumed"
	if selected.Status == runtime.RunbookActivationActive {
		status, action = runtime.RunbookActivationPaused, "paused"
	}
	m.busy, m.err = true, nil
	m.status = "Updating Runbook lifecycle…"
	id, revision := selected.ID, selected.Revision
	return func() tea.Msg {
		activation, err := m.client.UpdateRunbook(m.ctx, m.config.Scope, id, runtime.UpdateRunbookActivationRequest{ExpectedRevision: revision, Status: status})
		return runbookChanged{activation: activation, action: action, err: err}
	}
}

func (m *Model) reconcileRunbookSchedules() tea.Cmd {
	if !m.supportsRunbook(kernelapi.OperationReconcile) || m.busy {
		return nil
	}
	m.busy, m.err = true, nil
	m.status = "Reconciling due Runbook schedules…"
	return func() tea.Msg {
		result, err := m.client.ReconcileRunbookSchedules(m.ctx, kernelapi.ReconcileRunbookSchedulesRequest{Scope: m.config.Scope, Limit: 100})
		return runbookSchedulesReconciled{reconciliation: result, err: err}
	}
}

func (m *Model) loadSkillActions() tea.Cmd {
	if !m.supportsSkillAction(kernelapi.OperationList) || m.config.Owner.Type != runtime.OwnerTypeAgent {
		return nil
	}
	m.loading = true
	return func() tea.Msg {
		result, err := m.client.ListAgentSkillActions(
			m.ctx,
			capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID},
			m.config.Owner.ID,
			nil,
			"",
		)
		if err != nil {
			return skillActionsLoaded{err: err}
		}
		return skillActionsLoaded{actions: result.Actions}
	}
}

func (m *Model) skillBindingOwner() client.SkillBindingOwner {
	return client.SkillBindingOwner{Type: m.config.Owner.Type, DeploymentID: m.config.Owner.ID}
}

func (m *Model) loadSkillBindings() tea.Cmd {
	if !m.supportsSkillBinding(kernelapi.OperationList) {
		return nil
	}
	m.loading = true
	return func() tea.Msg {
		result, err := m.skillBindingClient.ListSkillBindings(m.ctx, capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID}, m.skillBindingOwner())
		if err != nil {
			return skillBindingsLoaded{err: err}
		}
		return skillBindingsLoaded{bindings: result.Items}
	}
}

func (m *Model) loadArtifacts() tea.Cmd {
	if !m.supportsArtifact(kernelapi.OperationList) {
		return nil
	}
	m.loading = true
	return func() tea.Msg {
		artifacts, err := m.client.ListArtifacts(m.ctx, runtime.ArtifactFilter{
			Scope: m.config.Scope, Owner: &m.config.Owner, LatestOnly: true, Limit: 100,
		})
		return artifactsLoaded{artifacts: artifacts, err: err}
	}
}

func (m *Model) loadConversations() tea.Cmd {
	if !m.supportsChannel(kernelapi.OperationList) || m.conversationClient == nil {
		return nil
	}
	m.loading = true
	builder := m.builderConversation
	return func() tea.Msg {
		conversations, err := m.conversationClient.ListConversations(m.ctx, runtime.ConversationFilter{
			Scope: m.config.Scope, Owner: &m.config.Owner, Limit: 100,
		})
		if err != nil || builder == nil || builder.Owner == m.config.Owner {
			return conversationsLoaded{conversations: conversations, err: err}
		}
		current, err := m.conversationClient.GetConversation(m.ctx, builder.Scope, builder.ID)
		if err != nil {
			return conversationsLoaded{err: err}
		}
		return conversationsLoaded{conversations: append([]*runtime.Conversation{current}, conversations...)}
	}
}

func (m *Model) loadSelectedConversation() tea.Cmd {
	conversation := m.selectedConversationRecord()
	if conversation == nil || m.conversationClient == nil || !m.supportsChannel(kernelapi.OperationList) {
		m.channelMessages, m.channelRounds, m.channelPresence = nil, nil, nil
		return nil
	}
	m.loading = true
	conversationID := conversation.ID
	conversationClient := m.conversationClient
	ctx, scope := m.ctx, m.config.Scope
	includeAudit := m.supportsChannel(kernelapi.OperationAudit)
	includePresence := m.supportsChannel(kernelapi.OperationPresence)
	return func() tea.Msg {
		messages, err := conversationClient.ListChannelMessages(ctx, runtime.ChannelMessageFilter{
			Scope: scope, ConversationID: conversationID, Limit: 50, Descending: true,
		})
		if err != nil {
			return channelDetailLoaded{conversationID: conversationID, err: err}
		}
		var rounds []*runtime.ParticipationRoundResult
		if includeAudit {
			rounds, err = conversationClient.ListParticipationRounds(ctx, runtime.ParticipationRoundFilter{
				Scope: scope, ConversationID: conversationID, Limit: 20,
			})
			if err != nil {
				return channelDetailLoaded{conversationID: conversationID, err: err}
			}
		}
		var presence []*runtime.ConversationPresence
		if includePresence {
			presence, err = conversationClient.ListConversationPresence(ctx, scope, conversationID)
		}
		return channelDetailLoaded{conversationID: conversationID, messages: messages, rounds: rounds, presence: presence, err: err}
	}
}

func (m *Model) loadPanel() tea.Cmd {
	if m.section == sectionReadiness {
		return m.loadCompilations()
	}
	if m.section == sectionTeams {
		return m.loadTeamDeployments()
	}
	if m.section == sectionObjectives {
		return tea.Batch(m.loadObjectives(), m.loadRuns())
	}
	if m.section == sectionIntegrations {
		return m.loadConversationGateways()
	}
	if m.section == sectionSkills {
		return tea.Batch(m.loadSkillBindings(), m.loadSkillActions())
	}
	if m.section == sectionRequests {
		return m.loadAgentRequests()
	}
	if m.section == sectionApprovals {
		return m.loadActionApprovals()
	}
	if m.section == sectionActivity {
		return m.loadActivity(false)
	}
	if m.section == sectionChannels {
		return m.loadConversations()
	}
	if m.section == sectionArtifacts {
		return m.loadArtifacts()
	}
	return m.loadRuns()
}

func (m *Model) submitAgentRequestResponse(decision runtime.AgentRequestDecision) tea.Cmd {
	request := m.selectedAgentRequestRecord()
	if request == nil || m.busy || !m.canRespondToSelectedRequest(decision) {
		return nil
	}
	message := strings.TrimSpace(m.editor.Value())
	assignedAgentID := ""
	if decision == runtime.AgentRequestDecisionAccept && request.Recipient.Type == runtime.OwnerTypeTeam {
		lines := strings.Split(message, "\n")
		assignedAgentID = strings.TrimSpace(lines[0])
		if assignedAgentID == "" {
			m.status = "Name the Team Agent that will own this accepted work."
			return nil
		}
		message = strings.TrimSpace(strings.Join(lines[1:], "\n"))
	}
	if (decision == runtime.AgentRequestDecisionReject || decision == runtime.AgentRequestDecisionRequestClarification || decision == runtime.AgentRequestDecisionProvideClarification) && message == "" {
		m.status = "Record a concise reason or clarification before submitting."
		return nil
	}
	principal := request.Recipient
	if decision == runtime.AgentRequestDecisionProvideClarification {
		principal = request.Requester
	}
	m.busy, m.err = true, nil
	action := agentRequestDecisionLabel(decision)
	m.status = action + "…"
	payload := kernelapi.RespondAgentRequestRequest{
		ExpectedRevision: request.Revision, Decision: decision, Principal: principal,
		AssignedAgentID: assignedAgentID, Message: message,
	}
	return func() tea.Msg {
		result, err := m.client.RespondAgentRequest(m.ctx, request.Scope, request.ID, payload)
		return agentRequestResponded{result: result, action: action, err: err}
	}
}

func (m *Model) submitAgentRequestCreation() tea.Cmd {
	source := m.selectedRun()
	prompt := strings.TrimSpace(m.editor.Value())
	if source == nil || m.busy || !m.supportsAgentRequest(kernelapi.OperationCreate) {
		return nil
	}
	if !runtime.CanStartAgentRequestFromRun(source) {
		m.status = "Start or resume active work before requesting collaboration from it."
		return nil
	}
	kind, recipient, goal, err := parseAgentRequestPrompt(prompt)
	if err != nil {
		m.status = err.Error()
		return nil
	}
	if m.pendingAgentRequestKey == "" || m.pendingAgentRequestPrompt != prompt || m.pendingAgentRequestSourceID != source.ID {
		m.pendingAgentRequestKey, m.pendingAgentRequestPrompt, m.pendingAgentRequestSourceID = uuid.NewString(), prompt, source.ID
	}
	key := m.pendingAgentRequestKey
	m.busy, m.err, m.status = true, nil, "Creating a durable collaboration request…"
	payload := kernelapi.CreateAgentRequestRequest{
		Scope: m.config.Scope, Kind: kind, Requester: m.localCollaborationParty(), Recipient: recipient,
		SourceRunID: source.ID, Goal: goal, IdempotencyKey: key,
	}
	return func() tea.Msg {
		result, createErr := m.client.CreateAgentRequest(m.ctx, payload, key)
		return agentRequestCreated{result: result, err: createErr}
	}
}

func parseAgentRequestPrompt(prompt string) (runtime.AgentRequestKind, runtime.CollaborationParty, string, error) {
	lines := strings.Split(strings.TrimSpace(prompt), "\n")
	if len(lines) < 2 {
		return "", runtime.CollaborationParty{}, "", errors.New("Put the recipient on the first line and the requested outcome below it.")
	}
	directive := strings.Fields(strings.TrimSpace(lines[0]))
	kind := runtime.AgentRequestKindRequest
	if len(directive) == 2 {
		switch runtime.AgentRequestKind(strings.ToLower(directive[0])) {
		case runtime.AgentRequestKindHandoff:
			kind = runtime.AgentRequestKindHandoff
			directive = directive[1:]
		case runtime.AgentRequestKindEscalation:
			kind = runtime.AgentRequestKindEscalation
			directive = directive[1:]
		}
	}
	if len(directive) != 1 {
		return "", runtime.CollaborationParty{}, "", errors.New("Use agent:<id>, team:<id>, handoff agent:<id>, or escalation team:<id> on the first line.")
	}
	identity := strings.SplitN(strings.TrimSpace(directive[0]), ":", 2)
	if len(identity) != 2 {
		return "", runtime.CollaborationParty{}, "", errors.New("Use agent:<id> or team:<id> for the recipient.")
	}
	recipient := runtime.CollaborationParty{Type: runtime.OwnerType(strings.ToLower(strings.TrimSpace(identity[0]))), ID: strings.TrimSpace(identity[1])}
	if err := recipient.Validate(); err != nil {
		return "", runtime.CollaborationParty{}, "", fmt.Errorf("recipient: %w", err)
	}
	goal := strings.TrimSpace(strings.Join(lines[1:], "\n"))
	if goal == "" {
		return "", runtime.CollaborationParty{}, "", errors.New("Describe the requested outcome below the recipient.")
	}
	return kind, recipient, goal, nil
}

func (m *Model) submitAgentRequestCompletion() tea.Cmd {
	request := m.selectedAgentRequestRecord()
	summary := strings.TrimSpace(m.editor.Value())
	if request == nil || m.busy || !m.canCompleteSelectedAgentRequest() {
		return nil
	}
	if summary == "" {
		m.status = "Summarize the completed outcome before submitting."
		return nil
	}
	if m.pendingRequestCompletionKey == "" || m.pendingRequestCompletionID != request.ID {
		m.pendingRequestCompletionKey, m.pendingRequestCompletionID = uuid.NewString(), request.ID
	}
	m.busy, m.err, m.status = true, nil, "Recording completion and resuming requesting work…"
	key := m.pendingRequestCompletionKey
	return func() tea.Msg {
		child, err := m.client.GetAgentRun(m.ctx, request.Scope, request.ChildRunID)
		if err != nil {
			return agentRequestCompleted{err: err}
		}
		payload := kernelapi.CompleteAgentRequestRequest{
			ExpectedRevision: request.Revision, ExpectedChildRevision: child.Revision,
			Principal: request.Recipient, Actor: request.Recipient, Summary: summary, IdempotencyKey: key,
		}
		result, err := m.client.CompleteAgentRequest(m.ctx, request.Scope, request.ID, payload, key)
		return agentRequestCompleted{result: result, err: err}
	}
}

func (m *Model) submitActionApproval(approve bool) tea.Cmd {
	approval := m.selectedActionApprovalRecord()
	reason := strings.TrimSpace(m.editor.Value())
	if approval == nil || m.busy || !m.canResolveSelectedActionApproval() {
		return nil
	}
	if reason == "" {
		m.status = "Record a reason for this permanent decision."
		return nil
	}
	action := "Rejecting action"
	if approve {
		action = "Approving action"
	}
	intent := fmt.Sprintf("%s\x00%d\x00%t\x00%s", approval.ID, approval.Revision, approve, reason)
	if m.pendingApprovalKey == "" || m.pendingApprovalIntent != intent {
		m.pendingApprovalKey, m.pendingApprovalIntent = uuid.NewString(), intent
	}
	m.busy, m.err, m.status = true, nil, action+"…"
	key := m.pendingApprovalKey
	payload := kernelapi.ResolveActionApprovalRequest{
		ExpectedRevision: approval.Revision, DecisionID: key,
		Decision:  map[bool]runtime.ApprovalDecision{true: runtime.ApprovalDecisionApprove, false: runtime.ApprovalDecisionReject}[approve],
		Principal: m.localApprovalPrincipal(), Reason: reason,
	}
	return func() tea.Msg {
		result, err := m.client.ResolveActionApproval(m.ctx, approval.Scope, approval.ID, payload, key)
		return actionApprovalResolved{result: result, action: action, err: err}
	}
}

func (m *Model) submitObjective() tea.Cmd {
	prompt := strings.TrimSpace(m.editor.Value())
	if !m.supportsObjective(kernelapi.OperationCreate) || m.busy || prompt == "" {
		if prompt == "" {
			m.status = "Describe the objective before adding it."
		}
		return nil
	}
	if m.pendingObjectiveKey == "" || m.pendingObjectivePrompt != prompt {
		m.pendingObjectiveKey, m.pendingObjectivePrompt = uuid.NewString(), prompt
	}
	key := m.pendingObjectiveKey
	m.busy = true
	m.err = nil
	m.status = "Adding objective to the durable portfolio…"
	request := kernelapi.CreateObjectiveRequest{
		Scope: m.config.Scope, Owner: m.config.Owner, Title: objectiveTitle(prompt), Goal: prompt, Status: runtime.ObjectiveStatusActive,
	}
	return func() tea.Msg {
		objective, err := m.client.CreateObjective(m.ctx, request, key)
		return objectiveCreated{objective: objective, err: err}
	}
}

func (m *Model) prepareSkillBindingComposer(binding *capability.Binding) {
	m.mode = modeSkillBindingUpsert
	m.editor.Reset()
	m.editor.Placeholder = "binding: reddit\nskill: reddit@1.0.0\nactions: search,post\nprompt: true\nrisk: external\ncredentials: reddit=reddit-oauth:credential-id\nreason: why this access is needed"
	if binding != nil {
		credentials := make([]string, 0, len(binding.Credentials))
		for name, reference := range binding.Credentials {
			credentials = append(credentials, name+"="+reference.Kind+":"+reference.ID)
		}
		sort.Strings(credentials)
		m.editor.SetValue(fmt.Sprintf("binding: %s\nskill: %s@%s\nsource: %s\nactions: %s\nprompt: %t\nrisk: %s\ncredentials: %s\nreason: update this exact binding", binding.ID, binding.SkillID, binding.SkillVersion, binding.SourceIdentity, strings.Join(binding.AllowedActions, ","), binding.EnablePrompt, binding.MaximumRisk, strings.Join(credentials, ",")))
	}
	m.focusComposerEditor()
}

func parseSkillBindingPrompt(value string, existing *capability.Binding) (*capability.Binding, string, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(value, "\n") {
		key, raw, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		fields[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(raw)
	}
	binding := &capability.Binding{}
	if existing != nil {
		*binding = *existing
		binding.AllowedActions = append([]string(nil), existing.AllowedActions...)
		binding.Credentials = mapsClone(existing.Credentials)
	}
	binding.ID = fields["binding"]
	identity := fields["skill"]
	skillID, version, ok := strings.Cut(identity, "@")
	if binding.ID == "" || !ok || strings.TrimSpace(skillID) == "" || strings.TrimSpace(version) == "" {
		return nil, "", errors.New("include binding and exact skill id@version")
	}
	binding.SkillID, binding.SkillVersion = strings.TrimSpace(skillID), strings.TrimSpace(version)
	binding.SourceIdentity = fields["source"]
	binding.AllowedActions = splitNonEmpty(fields["actions"])
	prompt, err := strconv.ParseBool(fields["prompt"])
	if err != nil {
		return nil, "", errors.New("prompt must be true or false")
	}
	binding.EnablePrompt = prompt
	binding.MaximumRisk = capability.RiskLevel(fields["risk"])
	switch binding.MaximumRisk {
	case capability.RiskLevelRead, capability.RiskLevelWrite, capability.RiskLevelExternal, capability.RiskLevelProduction, capability.RiskLevelDestructive:
	default:
		return nil, "", errors.New("risk must be read, write, external, production, or destructive")
	}
	binding.Credentials = map[string]capability.CredentialReference{}
	for _, item := range splitNonEmpty(fields["credentials"]) {
		name, reference, ok := strings.Cut(item, "=")
		kind, id, refOK := strings.Cut(reference, ":")
		if !ok || !refOK || strings.TrimSpace(name) == "" || strings.TrimSpace(kind) == "" || strings.TrimSpace(id) == "" {
			return nil, "", errors.New("credentials must be opaque name=kind:id references; never paste a secret")
		}
		binding.Credentials[strings.TrimSpace(name)] = capability.CredentialReference{Kind: strings.TrimSpace(kind), ID: strings.TrimSpace(id)}
	}
	reason := fields["reason"]
	if reason == "" {
		return nil, "", errors.New("include a durable reason")
	}
	binding.Disabled = false
	return binding, reason, nil
}

func splitNonEmpty(value string) []string {
	result := []string{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" && item != "none" {
			result = append(result, item)
		}
	}
	return result
}

func mapsClone[K comparable, V any](input map[K]V) map[K]V {
	result := make(map[K]V, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func (m *Model) submitSkillBindingUpsert() tea.Cmd {
	if m.busy || !m.supportsSkillBinding(kernelapi.OperationUpsert) {
		return nil
	}
	existing := m.selectedSkillBindingRecord()
	if existing != nil && !strings.Contains(m.editor.Value(), "binding: "+existing.ID) {
		existing = nil
	}
	binding, reason, err := parseSkillBindingPrompt(m.editor.Value(), existing)
	if err != nil {
		m.status = err.Error()
		return nil
	}
	binding.Scope = capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID}
	binding.DeploymentID = m.config.Owner.ID
	expected := int64(0)
	if existing != nil && existing.ID == binding.ID {
		expected = existing.Revision
	}
	m.busy, m.err, m.status = true, nil, "Applying the reviewed Skill authority…"
	request := skill.UpsertBindingRequest{Binding: binding, ExpectedRevision: expected, Actor: capability.BindingActor{Type: m.config.Actor.Type, ID: m.config.Actor.ID}, Reason: reason}
	return func() tea.Msg {
		result, changeErr := m.skillBindingClient.UpsertSkillBinding(m.ctx, m.skillBindingOwner(), request)
		if result == nil {
			return skillBindingChanged{action: "update failed", err: changeErr}
		}
		return skillBindingChanged{binding: result.Binding, action: "active", err: changeErr}
	}
}

func (m *Model) submitSkillBindingDisable() tea.Cmd {
	binding, reason := m.selectedSkillBindingRecord(), strings.TrimSpace(m.editor.Value())
	if binding == nil || reason == "" || m.busy || !m.supportsSkillBinding(kernelapi.OperationDisable) {
		m.status = "Record why this authority should be disabled."
		return nil
	}
	m.busy, m.err, m.status = true, nil, "Disabling this exact Skill authority…"
	request := skill.DisableBindingRequest{Scope: binding.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ExpectedRevision: binding.Revision, Actor: capability.BindingActor{Type: m.config.Actor.Type, ID: m.config.Actor.ID}, Reason: reason}
	return func() tea.Msg {
		result, changeErr := m.skillBindingClient.DisableSkillBinding(m.ctx, m.skillBindingOwner(), request)
		if result == nil {
			return skillBindingChanged{action: "disable failed", err: changeErr}
		}
		return skillBindingChanged{binding: result.Binding, action: "disabled", err: changeErr}
	}
}

func (m *Model) prepareSkillBindingUpgradeComposer() {
	binding := m.selectedSkillBindingRecord()
	if binding == nil || !m.supportsSkillBinding(kernelapi.OperationPlanUpgrade) {
		return
	}
	m.skillBindingUpgradePlan = nil
	m.mode = modeSkillBindingUpgradePlan
	m.editor.Reset()
	m.editor.SetValue("version: \nsource: " + binding.SourceIdentity)
	m.editor.Placeholder = "version: exact installed target version\nsource: optional exact source identity"
	m.focusComposerEditor()
	m.status = "Choose an exact installed Skill version. OpenSeal will calculate every durable reference affected before changing anything."
}

func (m *Model) submitSkillBindingUpgradePlan() tea.Cmd {
	binding := m.selectedSkillBindingRecord()
	if binding == nil || m.busy || !m.supportsSkillBinding(kernelapi.OperationPlanUpgrade) {
		return nil
	}
	fields := parseTUIKeyValueLines(m.editor.Value())
	version := strings.TrimSpace(fields["version"])
	if version == "" || version == binding.SkillVersion {
		m.status = "Enter a different exact installed target version."
		return nil
	}
	request := runtime.PlanSkillReferenceUpgradeRequest{
		Scope: runtime.Scope{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID}, DeploymentID: m.config.Owner.ID,
		BindingID: binding.ID, ToVersion: version, ToSourceIdentity: strings.TrimSpace(fields["source"]),
	}
	m.busy, m.err, m.status = true, nil, "Calculating the atomic Skill update impact…"
	return func() tea.Msg {
		plan, err := m.skillBindingClient.PlanSkillReferenceUpgrade(m.ctx, m.skillBindingOwner(), request)
		return skillBindingUpgradePlanned{plan: plan, err: err}
	}
}

func (m *Model) submitSkillBindingUpgradeApply() tea.Cmd {
	plan := m.skillBindingUpgradePlan
	if plan == nil || m.busy || !m.supportsSkillBinding(kernelapi.OperationApplyUpgrade) {
		return nil
	}
	fields := parseTUIKeyValueLines(m.editor.Value())
	reason := strings.TrimSpace(fields["reason"])
	approvalReason := strings.TrimSpace(fields["approval"])
	if reason == "" {
		m.status = "Record why this reviewed update should be applied."
		return nil
	}
	request := runtime.ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: m.config.Actor, Reason: reason}
	if plan.ApprovalRequired {
		if approvalReason == "" {
			m.status = "This update widens authority. Record the explicit approval reason."
			return nil
		}
		request.Approval = &runtime.SkillReferenceUpgradeApproval{Principal: m.config.Actor, Reason: approvalReason}
	}
	m.busy, m.err, m.status = true, nil, "Applying every reviewed Skill reference atomically…"
	return func() tea.Msg {
		receipt, err := m.skillBindingClient.ApplySkillReferenceUpgrade(m.ctx, m.skillBindingOwner(), request)
		return skillBindingUpgradeApplied{receipt: receipt, err: err}
	}
}

func parseTUIKeyValueLines(value string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(value, "\n") {
		key, raw, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, raw = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(raw)
		if key != "" && raw != "" {
			fields[key] = raw
		}
	}
	return fields
}

func (m *Model) submitObjectiveAmendment() tea.Cmd {
	objective := m.selectedObjectiveRecord()
	goal := strings.TrimSpace(m.editor.Value())
	if objective == nil || !m.supportsObjective(kernelapi.OperationUpdate) || m.busy || goal == "" {
		if goal == "" {
			m.status = "Describe the amended objective."
		}
		return nil
	}
	m.busy = true
	m.err = nil
	m.status = "Recording objective amendment…"
	request := kernelapi.UpdateObjectiveRequest{ExpectedRevision: objective.Revision, Goal: &goal}
	return func() tea.Msg {
		updated, err := m.client.UpdateObjective(m.ctx, m.config.Scope, objective.ID, request)
		return objectiveUpdated{objective: updated, err: err}
	}
}

func (m *Model) submitConversation() tea.Cmd {
	title := strings.TrimSpace(m.editor.Value())
	if !m.ready || m.conversationClient == nil || !m.supportsChannel(kernelapi.OperationCreate) || m.busy || title == "" {
		if title == "" {
			m.status = "Give the Team channel a clear name."
		}
		return nil
	}
	if m.pendingConversationKey == "" || m.pendingConversationTitle != title {
		m.pendingConversationKey = uuid.NewString()
		m.pendingConversationTitle = title
	}
	key := m.pendingConversationKey
	m.busy = true
	m.err = nil
	m.status = "Creating durable Team channel…"
	request := kernelapi.CreateConversationRequest{Scope: m.config.Scope, Owner: m.config.Owner, Title: title}
	return func() tea.Msg {
		conversation, err := m.conversationClient.CreateConversation(m.ctx, request, key)
		return conversationCreated{conversation: conversation, err: err}
	}
}

func (m *Model) submitChannelMessage() tea.Cmd {
	conversation := m.selectedConversationRecord()
	content := strings.TrimSpace(m.editor.Value())
	if conversation == nil || m.conversationClient == nil || !m.supportsChannel(kernelapi.OperationPost) || m.busy || content == "" {
		if content == "" {
			m.status = "Write a message before sending it."
		}
		return nil
	}
	if m.pendingMessageKey == "" || m.pendingMessageContent != content || m.pendingMessageChannelID != conversation.ID {
		m.pendingMessageKey = uuid.NewString()
		m.pendingMessageContent = content
		m.pendingMessageChannelID = conversation.ID
	}
	intent := runtime.MessageIntentUpdate
	requiresResponse := false
	if strings.HasSuffix(content, "?") {
		intent = runtime.MessageIntentQuestion
		requiresResponse = true
	}
	request := kernelapi.PostChannelMessageRequest{
		Scope: m.config.Scope, ExpectedRevision: conversation.Revision,
		Sender: m.operatorParticipant(), Intent: intent, Content: content,
		Audience:         runtime.ConversationAudience{Kind: runtime.ConversationAudienceChannel},
		RequiresResponse: requiresResponse,
	}
	key := m.pendingMessageKey
	m.busy = true
	m.err = nil
	m.status = "Posting to the Team channel…"
	return func() tea.Msg {
		result, err := m.conversationClient.PostChannelMessage(m.ctx, conversation.ID, request, key)
		return channelMessagePosted{result: result, err: err}
	}
}

func (m *Model) markSelectedConversationRead() tea.Cmd {
	conversation := m.selectedConversationRecord()
	if conversation == nil || conversation.LastSequence == 0 || m.conversationClient == nil || !m.supportsChannel(kernelapi.OperationRead) {
		return nil
	}
	conversationID, lastSequence := conversation.ID, conversation.LastSequence
	participant := m.operatorParticipant()
	conversationClient := m.conversationClient
	ctx, scope := m.ctx, m.config.Scope
	return func() tea.Msg {
		cursor, err := conversationClient.GetConversationCursor(ctx, scope, conversationID, participant)
		if err != nil && !isHTTPStatus(err, http.StatusNotFound) {
			return conversationCursorAdvanced{conversationID: conversationID, err: err}
		}
		expectedRevision := int64(0)
		if cursor != nil {
			expectedRevision = cursor.Revision
			if cursor.DeliveredSequence >= lastSequence && cursor.ReadSequence >= lastSequence {
				return conversationCursorAdvanced{conversationID: conversationID}
			}
		}
		_, _, err = conversationClient.AdvanceConversationCursor(ctx, conversationID, kernelapi.AdvanceConversationCursorRequest{
			Scope: scope, Participant: participant, ExpectedRevision: expectedRevision,
			DeliveredSequence: lastSequence, ReadSequence: lastSequence,
		})
		return conversationCursorAdvanced{conversationID: conversationID, err: err}
	}
}

func (m *Model) submitRun() tea.Cmd {
	goal := strings.TrimSpace(m.editor.Value())
	if !m.ready || !m.supportsRun(kernelapi.OperationCreate) || m.busy || goal == "" {
		if goal == "" {
			m.status = "Describe an outcome before starting work."
		}
		return nil
	}
	if m.pendingKey == "" || m.pendingGoal != goal {
		m.pendingKey = uuid.NewString()
		m.pendingGoal = goal
	}
	idempotencyKey := m.pendingKey
	m.busy = true
	m.err = nil
	m.status = "Creating durable work…"
	request := kernelapi.CreateAgentRunRequest{
		Scope: m.config.Scope, Owner: m.config.Owner, Goal: goal,
		Source: runtime.RunSourceManual, Actor: m.config.Actor,
	}
	if m.config.Owner.Type == runtime.OwnerTypeAgent {
		request.AssignedAgentID = m.config.Owner.ID
	}
	return func() tea.Msg {
		result, err := m.client.CreateAgentRun(m.ctx, request, idempotencyKey)
		return runCreated{result: result, err: err}
	}
}

func (m *Model) submitRunbookOperation() tea.Cmd {
	row := m.selectedActiveRunbookOperation()
	entry := m.agentDeployment
	if row == nil || entry == nil || entry.Deployment == nil || entry.Definition == nil ||
		!m.canStartActiveRunbookOperation() || m.busy {
		return nil
	}
	contract, ok := entry.Definition.Runbook.Interfaces[row.Focus.Entrypoint]
	if !ok {
		m.status = "This runbook entrypoint has no callable input contract."
		return nil
	}
	raw := strings.TrimSpace(m.editor.Value())
	if raw == "" {
		raw = "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var inputs map[string]interface{}
	if err := decoder.Decode(&inputs); err != nil {
		m.status = "Operation input must be one JSON object: " + err.Error()
		return nil
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		m.status = "Operation input must contain exactly one JSON object."
		return nil
	}
	if err := runbook.ValidateInterfaceInput(contract.InputSchema, inputs); err != nil {
		m.status = "Operation input does not match the active interface: " + err.Error()
		return nil
	}
	canonical, err := json.Marshal(inputs)
	if err != nil {
		m.status = "Operation input could not be normalized."
		return nil
	}
	intent := row.Focus.AgentID + "\x00" + row.Focus.RunbookID + "\x00" + row.Focus.RunbookVersion + "\x00" + row.Focus.Entrypoint + "\x00" + string(canonical)
	if m.pendingKey == "" || m.pendingGoal != intent {
		m.pendingKey, m.pendingGoal = uuid.NewString(), intent
	}
	m.busy, m.err = true, nil
	m.status = "Starting the exact deterministic operation…"
	deploymentID := entry.Deployment.ID
	request := kernelapi.CreateAgentRunRequest{
		Scope: m.config.Scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: deploymentID},
		AssignedAgentID: deploymentID, Entrypoint: row.Focus.Entrypoint,
		Goal:   fmt.Sprintf("Run %s · %s", row.Focus.AgentName, row.Focus.Entrypoint),
		Source: runtime.RunSourceManual, Context: inputs, Actor: m.config.Actor,
	}
	idempotencyKey := m.pendingKey
	return func() tea.Msg {
		result, createErr := m.client.CreateAgentRun(m.ctx, request, idempotencyKey)
		return runCreated{result: result, err: createErr}
	}
}

func (m *Model) submitGuidance() tea.Cmd {
	instruction := strings.TrimSpace(m.editor.Value())
	if instruction == "" {
		m.status = "Write the guidance you want the worker to receive."
		return nil
	}
	return m.commandSelected(runtime.AgentRunCommandIntervene, instruction)
}

func (m *Model) pauseOrResume() tea.Cmd {
	run := m.selectedRun()
	if run == nil {
		return nil
	}
	if run.Status == runtime.AgentRunStatusPaused {
		return m.commandSelected(runtime.AgentRunCommandResume, "")
	}
	return m.commandSelected(runtime.AgentRunCommandPause, "")
}

func (m *Model) commandSelected(kind runtime.AgentRunCommandKind, instruction string) tea.Cmd {
	run := m.selectedRun()
	if run == nil || m.busy || !m.commandAllowed(run, kind) {
		return nil
	}
	m.busy = true
	m.err = nil
	m.status = "Updating durable work…"
	request := kernelapi.AgentRunCommandRequest{
		ExpectedRevision: run.Revision, Kind: kind, Actor: m.config.Actor,
		Instruction: instruction,
	}
	return func() tea.Msg {
		result, err := m.client.CommandAgentRun(m.ctx, m.config.Scope, run.ID, request)
		return runCommanded{result: result, kind: kind, err: err}
	}
}

func (m *Model) poll() tea.Cmd {
	if m.config.PollInterval <= 0 {
		return nil
	}
	return tea.Tick(m.config.PollInterval, func(at time.Time) tea.Msg { return pollTick(at) })
}

func (m *Model) supportsRun(operation string) bool {
	return m.ready && m.runCapability.Supports(operation)
}

func (m *Model) defaultOperationalSection() panelSection {
	for _, candidate := range []struct {
		section   panelSection
		available bool
	}{
		{sectionAuthoring, m.agentBuilderCapability.Available},
		{sectionTeams, m.teamDefinitionCapability.Available},
		{sectionObjectives, m.objectiveCapability.Available},
		{sectionIntegrations, m.conversationGatewayCapability.Available},
		{sectionRuns, m.runCapability.Available},
		{sectionActivity, m.activityCapability.Available},
		{sectionRequests, m.requestCapability.Available},
		{sectionApprovals, m.approvalCapability.Available},
		{sectionChannels, m.channelCapability.Available},
		{sectionArtifacts, m.artifactCapability.Available},
	} {
		if candidate.available {
			return candidate.section
		}
	}
	return sectionActivity
}

// availableSections is the single navigation model for rendering and keyboard
// movement. Overview is always present; every other destination follows the
// server's capability document, preserving the host authority boundary.
func (m *Model) availableSections() []panelSection {
	sections := []panelSection{sectionOverview}
	for _, candidate := range []struct {
		section   panelSection
		available bool
	}{
		{sectionAuthoring, m.agentBuilderCapability.Available},
		{sectionReadiness, m.agentDefinitionCapability.Available},
		{sectionTeams, m.teamDefinitionCapability.Available},
		{sectionSkills, m.skillActionCapability.Available || m.skillBindingCapability.Available},
		{sectionRuns, m.runCapability.Available},
		{sectionChannels, m.channelCapability.Available},
		{sectionObjectives, m.objectiveCapability.Available},
		{sectionRequests, m.requestCapability.Available},
		{sectionApprovals, m.approvalCapability.Available},
		{sectionIntegrations, m.conversationGatewayCapability.Available},
		{sectionActivity, m.activityCapability.Available},
		{sectionArtifacts, m.artifactCapability.Available},
		{sectionBundles, m.workforceBundleCapability.Available},
	} {
		if candidate.available {
			sections = append(sections, candidate.section)
		}
	}
	return sections
}

func (m *Model) moveSection(delta int) {
	sections := m.availableSections()
	if len(sections) == 0 {
		return
	}
	current := 0
	for index, section := range sections {
		if section == m.section {
			current = index
			break
		}
	}
	m.section = sections[(current+delta+len(sections))%len(sections)]
}

func (m *Model) supportsAgentRequest(operation string) bool {
	return m.ready && m.requestCapability.Supports(operation)
}

func (m *Model) supportsActionApproval(operation string) bool {
	return m.ready && m.approvalCapability.Supports(operation)
}

func (m *Model) supportsAgentDefinition(operation string) bool {
	return m.ready && m.agentDefinitionCapability.Supports(operation)
}

func (m *Model) supportsTeamDefinition(operation string) bool {
	return m.ready && m.teamDefinitionCapability.Supports(operation)
}

func (m *Model) supportsObjective(operation string) bool {
	return m.ready && m.objectiveCapability.Supports(operation)
}

func (m *Model) supportsRunbook(operation string) bool {
	return m.ready && m.runbookCapability.Supports(operation)
}

func (m *Model) supportsSkillAction(operation string) bool {
	return m.ready && m.skillActionCapability.Supports(operation)
}

func (m *Model) supportsSkillBinding(operation string) bool {
	return m.ready && m.skillBindingClient != nil && m.skillBindingCapability.Supports(operation)
}

func (m *Model) supportsArtifact(operation string) bool {
	return m.ready && m.artifactCapability.Supports(operation)
}

func (m *Model) supportsChannel(operation string) bool {
	return m.ready && m.conversationClient != nil && m.channelCapability.Supports(operation)
}

func (m *Model) supportsAgentBuilder() bool {
	return m.ready && m.agentBuilderClient != nil && m.agentBuilderCapability.Supports(kernelapi.OperationCreate)
}

func (m *Model) commandAllowed(run *runtime.AgentRun, kind runtime.AgentRunCommandKind) bool {
	if run == nil || isTerminal(run.Status) {
		return false
	}
	switch kind {
	case runtime.AgentRunCommandPause:
		return run.Status != runtime.AgentRunStatusPaused && m.supportsRun(kernelapi.OperationPause)
	case runtime.AgentRunCommandResume:
		return run.Status == runtime.AgentRunStatusPaused && m.supportsRun(kernelapi.OperationResume)
	case runtime.AgentRunCommandCancel:
		return m.supportsRun(kernelapi.OperationCancel)
	case runtime.AgentRunCommandIntervene:
		return m.supportsRun(kernelapi.OperationIntervene)
	default:
		return false
	}
}

func (m *Model) pauseOrResumeAgentDeployment() tea.Cmd {
	entry := m.agentDeployment
	if entry == nil || entry.Deployment == nil || m.busy || !m.supportsAgentDefinition(kernelapi.OperationUpdate) {
		return nil
	}
	current := entry.Deployment
	nextStatus, verb := kernelagent.RolloutPaused, "Pause"
	if current.RolloutStatus == kernelagent.RolloutPaused {
		nextStatus, verb = kernelagent.RolloutActive, "Resume"
	} else if current.RolloutStatus != kernelagent.RolloutActive {
		m.status = "Only active or paused Agent deployments can be paused or resumed."
		return nil
	}
	updated := *current
	updated.RolloutStatus = nextStatus
	request := kernelapi.UpdateAgentDeploymentRequest{
		Deployment: &updated, ExpectedRevision: current.Revision,
		ActorType: m.config.Actor.Type, ActorID: m.config.Actor.ID, Reason: verb + " Agent from the terminal",
	}
	m.busy, m.err = true, nil
	m.status = verb + " Agent deployment…"
	return func() tea.Msg {
		result, err := m.client.UpdateAgentDeployment(m.ctx, current.ID, request)
		return agentDeploymentUpdated{result: result, err: err}
	}
}

func (m *Model) selectedRun() *runtime.AgentRun {
	if m.selected < 0 || m.selected >= len(m.runs) {
		return nil
	}
	return m.runs[m.selected]
}

func (m *Model) selectedAgentRequestRecord() *runtime.AgentRequest {
	if m.agentRequestSelected < 0 || m.agentRequestSelected >= len(m.agentRequests) {
		return nil
	}
	return m.agentRequests[m.agentRequestSelected]
}

func (m *Model) restoreAgentRequestSelection() {
	if len(m.agentRequests) == 0 {
		m.agentRequestSelected, m.selectedAgentRequest = 0, ""
		return
	}
	for index, request := range m.agentRequests {
		if request.ID == m.selectedAgentRequest {
			m.agentRequestSelected = index
			return
		}
	}
	m.agentRequestSelected = min(m.agentRequestSelected, len(m.agentRequests)-1)
	m.selectedAgentRequest = m.agentRequests[m.agentRequestSelected].ID
}

func (m *Model) moveAgentRequestSelection(delta int) {
	if len(m.agentRequests) == 0 {
		return
	}
	m.agentRequestSelected = max(0, min(len(m.agentRequests)-1, m.agentRequestSelected+delta))
	m.selectedAgentRequest = m.agentRequests[m.agentRequestSelected].ID
}

func (m *Model) selectedActionApprovalRecord() *runtime.ApprovalCheckpoint {
	if m.actionApprovalSelected < 0 || m.actionApprovalSelected >= len(m.actionApprovals) {
		return nil
	}
	return m.actionApprovals[m.actionApprovalSelected]
}

func (m *Model) restoreActionApprovalSelection() {
	if len(m.actionApprovals) == 0 {
		m.actionApprovalSelected, m.selectedActionApproval = 0, ""
		return
	}
	for index, approval := range m.actionApprovals {
		if approval.ID == m.selectedActionApproval {
			m.actionApprovalSelected = index
			return
		}
	}
	m.actionApprovalSelected = min(m.actionApprovalSelected, len(m.actionApprovals)-1)
	m.selectedActionApproval = m.actionApprovals[m.actionApprovalSelected].ID
}

func (m *Model) moveActionApprovalSelection(delta int) {
	if len(m.actionApprovals) == 0 {
		return
	}
	m.actionApprovalSelected = max(0, min(len(m.actionApprovals)-1, m.actionApprovalSelected+delta))
	m.selectedActionApproval = m.actionApprovals[m.actionApprovalSelected].ID
}

func (m *Model) canRespondToSelectedRequest(decision runtime.AgentRequestDecision) bool {
	request := m.selectedAgentRequestRecord()
	if request == nil || !m.supportsAgentRequest(kernelapi.OperationRespond) {
		return false
	}
	local := m.localCollaborationParty()
	switch decision {
	case runtime.AgentRequestDecisionProvideClarification:
		return request.Status == runtime.AgentRequestStatusClarificationRequested && request.Requester == local
	case runtime.AgentRequestDecisionAccept, runtime.AgentRequestDecisionReject, runtime.AgentRequestDecisionRequestClarification:
		return request.Status == runtime.AgentRequestStatusPending && request.Recipient == local
	default:
		return false
	}
}

func (m *Model) canCompleteSelectedAgentRequest() bool {
	request := m.selectedAgentRequestRecord()
	if request == nil || !m.supportsAgentRequest(kernelapi.OperationComplete) || request.Status != runtime.AgentRequestStatusAccepted || request.Recipient != m.localCollaborationParty() || request.ChildRunID == "" {
		return false
	}
	if len(request.AcceptanceCriteria) > 0 {
		return false
	}
	for _, requirement := range request.ArtifactRequirements {
		if requirement.Required {
			return false
		}
	}
	return true
}

func (m *Model) canResolveSelectedActionApproval() bool {
	approval := m.selectedActionApprovalRecord()
	if approval == nil || approval.Status != runtime.ApprovalStatusPending || !m.supportsActionApproval(kernelapi.OperationResolve) {
		return false
	}
	principal := m.localApprovalPrincipal()
	for _, eligible := range approval.EligibleApprovers {
		if eligible == principal {
			return true
		}
	}
	return false
}

func (m *Model) localCollaborationParty() runtime.CollaborationParty {
	return runtime.CollaborationParty{Type: m.config.Owner.Type, ID: m.config.Owner.ID}
}

func (m *Model) localApprovalPrincipal() runtime.ApprovalPrincipal {
	principal := runtime.ApprovalPrincipal{Type: strings.TrimSpace(m.config.Actor.Type), ID: strings.TrimSpace(m.config.Actor.ID)}
	if principal.Type == "" || principal.ID == "" {
		principal = runtime.ApprovalPrincipal{Type: string(m.config.Owner.Type), ID: m.config.Owner.ID}
	}
	return principal
}

func (m *Model) selectedObjectiveRecord() *runtime.Objective {
	if m.objectiveSelected < 0 || m.objectiveSelected >= len(m.objectives) {
		return nil
	}
	return m.objectives[m.objectiveSelected]
}

func (m *Model) selectedObjectiveRunbooks() []*runtime.RunbookActivation {
	values := make([]*runtime.RunbookActivation, 0)
	objective := m.selectedObjectiveRecord()
	if objective == nil {
		return values
	}
	for _, value := range m.runbooks {
		if value != nil && value.ObjectiveID == objective.ID {
			values = append(values, value)
		}
	}
	sort.SliceStable(values, func(left, right int) bool {
		if values[left].CreatedAt.Equal(values[right].CreatedAt) {
			return values[left].ID < values[right].ID
		}
		return values[left].CreatedAt.Before(values[right].CreatedAt)
	})
	return values
}

func (m *Model) selectedRunbookRecord() *runtime.RunbookActivation {
	values := m.selectedObjectiveRunbooks()
	if m.runbookSelected < 0 || m.runbookSelected >= len(values) {
		return nil
	}
	return values[m.runbookSelected]
}

func (m *Model) restoreRunbookSelection() {
	values := m.selectedObjectiveRunbooks()
	if len(values) == 0 {
		m.runbookSelected, m.selectedRunbook, m.runbookDetail = 0, "", nil
		m.runbookDetailExpanded = false
		return
	}
	for index, value := range values {
		if value.ID == m.selectedRunbook {
			m.runbookSelected = index
			return
		}
	}
	m.runbookSelected = min(m.runbookSelected, len(values)-1)
	m.selectedRunbook = values[m.runbookSelected].ID
	m.runbookDetail = nil
	m.runbookDetailExpanded = false
}

func (m *Model) moveRunbookSelection(delta int) {
	values := m.selectedObjectiveRunbooks()
	if len(values) == 0 {
		return
	}
	m.runbookSelected = max(0, min(len(values)-1, m.runbookSelected+delta))
	m.selectedRunbook = values[m.runbookSelected].ID
	m.runbookDetail = nil
	m.runbookDetailExpanded = false
}

func (m *Model) objectiveRecord(objectiveID string) *runtime.Objective {
	for _, objective := range m.objectives {
		if objective != nil && objective.ID == objectiveID {
			return objective
		}
	}
	return nil
}

func (m *Model) restoreObjectiveSelection() {
	if len(m.objectives) == 0 {
		m.objectiveSelected = 0
		m.selectedObjective = ""
		return
	}
	if m.selectedObjective != "" {
		for index, objective := range m.objectives {
			if objective.ID == m.selectedObjective {
				m.objectiveSelected = index
				return
			}
		}
	}
	m.objectiveSelected = min(m.objectiveSelected, len(m.objectives)-1)
	m.selectedObjective = m.objectives[m.objectiveSelected].ID
}

func (m *Model) moveObjectiveSelection(delta int) {
	if len(m.objectives) == 0 {
		return
	}
	m.objectiveSelected = max(0, min(len(m.objectives)-1, m.objectiveSelected+delta))
	m.selectedObjective = m.objectives[m.objectiveSelected].ID
	m.selectedRunbook, m.runbookDetail = "", nil
	m.runbookSelected = 0
	m.runbookDetailExpanded = false
	m.restoreRunbookSelection()
}

func (m *Model) restoreSelection() {
	if len(m.runs) == 0 {
		m.selected = 0
		m.selectedID = ""
		return
	}
	if m.selectedID != "" {
		for index, run := range m.runs {
			if run.ID == m.selectedID {
				m.selected = index
				return
			}
		}
	}
	m.selected = min(m.selected, len(m.runs)-1)
	m.selectedID = m.runs[m.selected].ID
}

func (m *Model) moveSelection(delta int) {
	if len(m.runs) == 0 {
		return
	}
	m.selected = max(0, min(len(m.runs)-1, m.selected+delta))
	m.selectedID = m.runs[m.selected].ID
}

func (m *Model) selectedArtifactRecord() *runtime.Artifact {
	if m.artifactSelected < 0 || m.artifactSelected >= len(m.artifacts) {
		return nil
	}
	return m.artifacts[m.artifactSelected]
}

func (m *Model) restoreArtifactSelection() {
	if len(m.artifacts) == 0 {
		m.artifactSelected = 0
		m.selectedArtifact = ""
		m.artifactExpanded = false
		return
	}
	if m.selectedArtifact != "" {
		for index, artifact := range m.artifacts {
			if artifactSelectionKey(artifact) == m.selectedArtifact {
				m.artifactSelected = index
				return
			}
		}
	}
	m.artifactSelected = min(m.artifactSelected, len(m.artifacts)-1)
	m.selectedArtifact = artifactSelectionKey(m.artifacts[m.artifactSelected])
	m.artifactExpanded = false
}

func (m *Model) moveArtifactSelection(delta int) {
	if len(m.artifacts) == 0 {
		return
	}
	m.artifactSelected = max(0, min(len(m.artifacts)-1, m.artifactSelected+delta))
	m.selectedArtifact = artifactSelectionKey(m.artifacts[m.artifactSelected])
	m.artifactExpanded = false
}

func (m *Model) selectedActivityRecord() *runtime.ActivityProjection {
	if m.activitySelected < 0 || m.activitySelected >= len(m.activity) {
		return nil
	}
	return &m.activity[m.activitySelected]
}

func (m *Model) restoreActivitySelection() {
	if len(m.activity) == 0 {
		m.activitySelected = 0
		m.selectedActivity = ""
		m.activityExpanded = false
		return
	}
	if m.selectedActivity != "" {
		for index := range m.activity {
			if m.activity[index].ID == m.selectedActivity {
				m.activitySelected = index
				return
			}
		}
	}
	m.activitySelected = min(m.activitySelected, len(m.activity)-1)
	m.selectedActivity = m.activity[m.activitySelected].ID
	m.activityExpanded = false
}

func (m *Model) moveActivitySelection(delta int) {
	if len(m.activity) == 0 {
		return
	}
	m.activitySelected = max(0, min(len(m.activity)-1, m.activitySelected+delta))
	m.selectedActivity = m.activity[m.activitySelected].ID
	m.activityExpanded = false
}

func (m *Model) movePanelSelection(delta int) {
	if m.section == sectionTeams {
		m.moveTeamDeploymentSelection(delta)
		return
	}
	if m.section == sectionObjectives {
		m.moveObjectiveSelection(delta)
		return
	}
	if m.section == sectionIntegrations {
		m.moveConversationGatewaySelection(delta)
		return
	}
	if m.section == sectionSkills {
		m.moveSkillBindingSelection(delta)
		return
	}
	if m.section == sectionRequests {
		m.moveAgentRequestSelection(delta)
		return
	}
	if m.section == sectionApprovals {
		m.moveActionApprovalSelection(delta)
		return
	}
	if m.section == sectionActivity {
		m.moveActivitySelection(delta)
		return
	}
	if m.section == sectionChannels {
		m.moveConversationSelection(delta)
		return
	}
	if m.section == sectionArtifacts {
		m.moveArtifactSelection(delta)
		return
	}
	m.moveSelection(delta)
}

func (m *Model) selectedTeamDeploymentRecord() *kernelapi.TeamDeploymentCatalogEntry {
	if m.teamDeploymentSelected < 0 || m.teamDeploymentSelected >= len(m.teamDeployments) {
		return nil
	}
	return &m.teamDeployments[m.teamDeploymentSelected]
}

func (m *Model) selectedAgentAmendmentRecord() *kernelagent.DefinitionAmendment {
	if m.agentAmendmentSelected < 0 || m.agentAmendmentSelected >= len(m.agentAmendments) {
		return nil
	}
	return m.agentAmendments[m.agentAmendmentSelected]
}

func (m *Model) restoreAgentAmendmentSelection() {
	if len(m.agentAmendments) == 0 {
		m.agentAmendmentSelected = 0
		m.selectedAgentAmendment = ""
		return
	}
	for index, amendment := range m.agentAmendments {
		if amendment != nil && amendment.ID == m.selectedAgentAmendment {
			m.agentAmendmentSelected = index
			return
		}
	}
	m.agentAmendmentSelected = min(m.agentAmendmentSelected, len(m.agentAmendments)-1)
	if amendment := m.agentAmendments[m.agentAmendmentSelected]; amendment != nil {
		m.selectedAgentAmendment = amendment.ID
	}
}

func (m *Model) moveAgentAmendmentSelection(delta int) {
	if len(m.agentAmendments) == 0 {
		return
	}
	m.agentAmendmentSelected = max(0, min(len(m.agentAmendments)-1, m.agentAmendmentSelected+delta))
	if amendment := m.agentAmendments[m.agentAmendmentSelected]; amendment != nil {
		m.selectedAgentAmendment = amendment.ID
	}
}

func (m *Model) canProposeAgentBehaviorAmendment() bool {
	entry := m.agentDeployment
	if entry == nil || entry.Definition == nil || m.agentLifecycleClient == nil || !m.supportsAgentDefinition(kernelapi.OperationProposeAmendment) {
		return false
	}
	for _, field := range entry.Definition.Amendments.AllowedFields {
		if field == "systemPrompt" || field == "personality" {
			return true
		}
	}
	return false
}

func (m *Model) canResolveSelectedAgentAmendment() bool {
	amendment := m.selectedAgentAmendmentRecord()
	entry := m.agentDeployment
	if amendment == nil || amendment.Status != kernelagent.AmendmentAwaitingApproval || entry == nil || entry.Definition == nil || m.agentLifecycleClient == nil || !m.supportsAgentDefinition(kernelapi.OperationResolveAmendment) {
		return false
	}
	principal := strings.TrimSpace(m.config.Actor.Type) + ":" + strings.TrimSpace(m.config.Actor.ID)
	for _, eligible := range entry.Definition.Amendments.ApproverPrincipals {
		if eligible == principal {
			return true
		}
	}
	return false
}

func (m *Model) canEvaluateSelectedAgentAmendment() bool {
	amendment := m.selectedAgentAmendmentRecord()
	return amendment != nil && amendment.Status == kernelagent.AmendmentEvaluating && m.agentLifecycleClient != nil && m.supportsAgentDefinition(kernelapi.OperationEvaluateAmendment)
}

func (m *Model) canActivateSelectedAgentAmendment() bool {
	amendment := m.selectedAgentAmendmentRecord()
	return amendment != nil && (amendment.Status == kernelagent.AmendmentReady || amendment.Status == kernelagent.AmendmentApproved) && m.agentLifecycleClient != nil && m.supportsAgentDefinition(kernelapi.OperationActivateAmendment)
}

func (m *Model) restoreTeamDeploymentSelection() {
	if len(m.teamDeployments) == 0 {
		m.teamDeploymentSelected = 0
		m.selectedTeamDeployment = ""
		return
	}
	for index := range m.teamDeployments {
		deployment := m.teamDeployments[index].Deployment
		if deployment != nil && deployment.ID == m.selectedTeamDeployment {
			m.teamDeploymentSelected = index
			return
		}
	}
	m.teamDeploymentSelected = min(m.teamDeploymentSelected, len(m.teamDeployments)-1)
	if deployment := m.teamDeployments[m.teamDeploymentSelected].Deployment; deployment != nil {
		m.selectedTeamDeployment = deployment.ID
	}
}

func (m *Model) moveTeamDeploymentSelection(delta int) {
	if len(m.teamDeployments) == 0 {
		return
	}
	m.teamDeploymentSelected = max(0, min(len(m.teamDeployments)-1, m.teamDeploymentSelected+delta))
	if deployment := m.teamDeployments[m.teamDeploymentSelected].Deployment; deployment != nil {
		if deployment.ID != m.selectedTeamDeployment {
			m.teamAmendments = nil
			m.selectedTeamAmendment = ""
			m.teamAmendmentSelected = 0
		}
		m.selectedTeamDeployment = deployment.ID
	}
}

func (m *Model) selectedTeamAmendmentRecord() *kernelteam.DefinitionAmendment {
	if m.teamAmendmentSelected < 0 || m.teamAmendmentSelected >= len(m.teamAmendments) {
		return nil
	}
	return m.teamAmendments[m.teamAmendmentSelected]
}

func (m *Model) restoreTeamAmendmentSelection() {
	if len(m.teamAmendments) == 0 {
		m.teamAmendmentSelected = 0
		m.selectedTeamAmendment = ""
		return
	}
	for index, amendment := range m.teamAmendments {
		if amendment != nil && amendment.ID == m.selectedTeamAmendment {
			m.teamAmendmentSelected = index
			return
		}
	}
	m.teamAmendmentSelected = min(m.teamAmendmentSelected, len(m.teamAmendments)-1)
	if amendment := m.teamAmendments[m.teamAmendmentSelected]; amendment != nil {
		m.selectedTeamAmendment = amendment.ID
	}
}

func (m *Model) moveTeamAmendmentSelection(delta int) {
	if len(m.teamAmendments) == 0 {
		return
	}
	m.teamAmendmentSelected = max(0, min(len(m.teamAmendments)-1, m.teamAmendmentSelected+delta))
	if amendment := m.teamAmendments[m.teamAmendmentSelected]; amendment != nil {
		m.selectedTeamAmendment = amendment.ID
	}
}

func (m *Model) canProposeTeamPurposeAmendment() bool {
	entry := m.selectedTeamDeploymentRecord()
	if entry == nil || entry.Definition == nil || !m.supportsTeamDefinition(kernelapi.OperationProposeAmendment) {
		return false
	}
	for _, field := range entry.Definition.Amendments.AllowedFields {
		if field == "purpose" {
			return true
		}
	}
	return false
}

func (m *Model) canResolveSelectedTeamAmendment() bool {
	amendment := m.selectedTeamAmendmentRecord()
	entry := m.selectedTeamDeploymentRecord()
	if amendment == nil || amendment.Status != kernelteam.AmendmentAwaitingApproval || entry == nil || entry.Definition == nil || !m.supportsTeamDefinition(kernelapi.OperationResolveAmendment) {
		return false
	}
	principal := strings.TrimSpace(m.config.Actor.Type) + ":" + strings.TrimSpace(m.config.Actor.ID)
	for _, eligible := range entry.Definition.Amendments.ApproverPrincipals {
		if eligible == principal {
			return true
		}
	}
	return false
}

func (m *Model) canEvaluateSelectedTeamAmendment() bool {
	amendment := m.selectedTeamAmendmentRecord()
	return amendment != nil && amendment.Status == kernelteam.AmendmentEvaluating && m.supportsTeamDefinition(kernelapi.OperationEvaluateAmendment)
}

func (m *Model) canActivateSelectedTeamAmendment() bool {
	amendment := m.selectedTeamAmendmentRecord()
	return amendment != nil && (amendment.Status == kernelteam.AmendmentReady || amendment.Status == kernelteam.AmendmentApproved) && m.supportsTeamDefinition(kernelapi.OperationActivateAmendment)
}

func (m *Model) pauseOrResumeTeamDeployment() tea.Cmd {
	entry := m.selectedTeamDeploymentRecord()
	if entry == nil || entry.Deployment == nil || m.busy || !m.supportsTeamDefinition(kernelapi.OperationUpdate) {
		return nil
	}
	current := entry.Deployment
	nextStatus := kernelteam.DeploymentPaused
	verb := "Pause"
	if current.Status == kernelteam.DeploymentPaused {
		nextStatus = kernelteam.DeploymentActive
		verb = "Resume"
	} else if current.Status != kernelteam.DeploymentActive {
		m.status = "Only active or paused Team deployments can be paused or resumed."
		return nil
	}
	updated := *current
	updated.Roster = append([]kernelteam.RosterAssignment(nil), current.Roster...)
	updated.Restrictions.AllowedSkillIDs = append([]string(nil), current.Restrictions.AllowedSkillIDs...)
	updated.Status = nextStatus
	m.busy = true
	m.err = nil
	m.status = verb + " Team deployment…"
	request := kernelapi.UpdateTeamDeploymentRequest{
		Deployment: &updated, ExpectedRevision: current.Revision,
		ActorType: m.config.Actor.Type, ActorID: m.config.Actor.ID,
		Reason: verb + " Team from the terminal",
	}
	return func() tea.Msg {
		result, err := m.client.UpdateTeamDeployment(m.ctx, current.ID, request)
		return teamDeploymentUpdated{result: result, err: err}
	}
}

func (m *Model) submitAgentAmendmentProposal() tea.Cmd {
	entry := m.agentDeployment
	input := strings.TrimSpace(m.editor.Value())
	if !m.canProposeAgentBehaviorAmendment() || entry == nil || entry.Deployment == nil || entry.Definition == nil || m.busy {
		return nil
	}
	parts := strings.SplitN(input, "\n", 3)
	if len(parts) != 3 {
		m.status = "Use three sections: systemPrompt|personality, concise rationale, then the new value."
		return nil
	}
	field, rationale, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
	allowed := false
	for _, candidate := range entry.Definition.Amendments.AllowedFields {
		if candidate == field && (field == "systemPrompt" || field == "personality") {
			allowed = true
			break
		}
	}
	if !allowed {
		m.status = "Choose an amendment field allowed by this Agent: systemPrompt or personality."
		return nil
	}
	if rationale == "" || value == "" {
		m.status = "Agent amendment rationale and new value are required."
		return nil
	}
	candidate := *entry.Definition
	current := candidate.SystemPrompt
	if field == "personality" {
		current = candidate.Personality
	}
	if value == strings.TrimSpace(current) {
		m.status = "The proposed value must change Agent behavior."
		return nil
	}
	digest := sha256.Sum256([]byte(entry.Definition.Digest + "\x00" + field + "\x00" + value))
	candidate.Version = fmt.Sprintf("amend-%x", digest[:8])
	if field == "systemPrompt" {
		candidate.SystemPrompt = value
	} else {
		candidate.Personality = value
	}
	candidate.Digest, candidate.CreatedAt = "", time.Time{}
	request := kernelagent.ProposeAmendmentRequest{
		Scope: entry.Deployment.Scope, DeploymentID: entry.Deployment.ID, Candidate: &candidate,
		ProposerType: m.config.Actor.Type, ProposerID: m.config.Actor.ID, Rationale: rationale,
		ExpectedDeploymentRevision: entry.Deployment.Revision,
	}
	m.busy, m.err = true, nil
	m.status = "Recording immutable Agent amendment candidate…"
	return func() tea.Msg {
		amendment, err := m.agentLifecycleClient.ProposeAgentDefinitionAmendment(m.ctx, request)
		return agentAmendmentChanged{amendment: amendment, action: "Agent amendment proposal", err: err}
	}
}

func (m *Model) submitAgentAmendmentEvaluation() tea.Cmd {
	amendment := m.selectedAgentAmendmentRecord()
	entry := m.agentDeployment
	if !m.canEvaluateSelectedAgentAmendment() || amendment == nil || entry == nil || entry.Deployment == nil || entry.Definition == nil || m.busy {
		return nil
	}
	evaluations, err := parseAmendmentEvaluations(m.editor.Value(), entry.Definition.Evaluations)
	if err != nil {
		m.status = err.Error()
		return nil
	}
	request := kernelagent.SubmitAmendmentEvaluationRequest{Scope: amendment.Scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision, Evaluations: evaluations}
	m.busy, m.err = true, nil
	m.status = "Recording revision-bound Agent evaluation…"
	return func() tea.Msg {
		updated, err := m.agentLifecycleClient.SubmitAgentDefinitionAmendmentEvaluation(m.ctx, entry.Deployment.ID, request)
		return agentAmendmentChanged{amendment: updated, action: "Agent evaluation", err: err}
	}
}

func (m *Model) submitAgentAmendmentDecision(approved bool) tea.Cmd {
	amendment := m.selectedAgentAmendmentRecord()
	entry := m.agentDeployment
	reason := strings.TrimSpace(m.editor.Value())
	if !m.canResolveSelectedAgentAmendment() || amendment == nil || entry == nil || entry.Deployment == nil || m.busy {
		return nil
	}
	if reason == "" {
		m.status = "Record a reason for the permanent Agent amendment decision."
		return nil
	}
	request := kernelagent.ResolveAmendmentRequest{
		Scope: amendment.Scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision, Approved: approved,
		ActorType: m.config.Actor.Type, ActorID: m.config.Actor.ID, Reason: reason,
	}
	action := "Agent amendment approval"
	if !approved {
		action = "Agent amendment rejection"
	}
	m.busy, m.err = true, nil
	m.status = "Recording revision-bound Agent decision…"
	return func() tea.Msg {
		updated, err := m.agentLifecycleClient.ResolveAgentDefinitionAmendment(m.ctx, entry.Deployment.ID, request)
		return agentAmendmentChanged{amendment: updated, action: action, err: err}
	}
}

func (m *Model) submitAgentAmendmentActivation() tea.Cmd {
	amendment := m.selectedAgentAmendmentRecord()
	entry := m.agentDeployment
	reason := strings.TrimSpace(m.editor.Value())
	if !m.canActivateSelectedAgentAmendment() || amendment == nil || entry == nil || entry.Deployment == nil || m.busy {
		return nil
	}
	if reason == "" {
		m.status = "Record why this exact Agent definition should become active."
		return nil
	}
	request := kernelapi.ActivateAgentDefinitionAmendmentRequest{
		Scope: amendment.Scope, ExpectedRevision: amendment.Revision,
		ActorType: m.config.Actor.Type, ActorID: m.config.Actor.ID, Reason: reason,
	}
	m.busy, m.err = true, nil
	m.status = "Atomically activating reviewed Agent definition…"
	return func() tea.Msg {
		result, err := m.agentLifecycleClient.ActivateAgentDefinitionAmendment(m.ctx, entry.Deployment.ID, amendment.ID, request)
		if result == nil {
			return agentAmendmentChanged{action: "Agent amendment activation", err: err}
		}
		return agentAmendmentChanged{amendment: result.Amendment, deployment: result.Deployment, action: "Agent amendment activation", err: err}
	}
}

func (m *Model) submitTeamAmendmentProposal() tea.Cmd {
	entry := m.selectedTeamDeploymentRecord()
	prompt := strings.TrimSpace(m.editor.Value())
	if !m.canProposeTeamPurposeAmendment() || entry == nil || entry.Deployment == nil || entry.Definition == nil || m.busy {
		return nil
	}
	rationale, purpose, ok := strings.Cut(prompt, "\n")
	rationale, purpose = strings.TrimSpace(rationale), strings.TrimSpace(purpose)
	if !ok || rationale == "" || purpose == "" {
		m.status = "Use the first line for rationale and the remaining lines for the new Team purpose."
		return nil
	}
	if purpose == strings.TrimSpace(entry.Definition.Purpose) {
		m.status = "The proposed purpose must change Team behavior."
		return nil
	}
	candidate := *entry.Definition
	digest := sha256.Sum256([]byte(entry.Definition.Digest + "\x00" + purpose))
	candidate.Version = fmt.Sprintf("amend-%x", digest[:8])
	candidate.Purpose, candidate.Digest, candidate.CreatedAt = purpose, "", time.Time{}
	request := kernelteam.ProposeAmendmentRequest{
		Scope: entry.Deployment.Scope, DeploymentID: entry.Deployment.ID, Candidate: &candidate,
		ProposerType: m.config.Actor.Type, ProposerID: m.config.Actor.ID, Rationale: rationale,
	}
	m.busy, m.err = true, nil
	m.status = "Recording immutable Team amendment candidate…"
	return func() tea.Msg {
		amendment, err := m.client.ProposeTeamDefinitionAmendment(m.ctx, request)
		return teamAmendmentChanged{amendment: amendment, action: "Team amendment proposal", err: err}
	}
}

func (m *Model) submitTeamAmendmentEvaluation() tea.Cmd {
	amendment := m.selectedTeamAmendmentRecord()
	entry := m.selectedTeamDeploymentRecord()
	if !m.canEvaluateSelectedTeamAmendment() || amendment == nil || entry == nil || entry.Deployment == nil || entry.Definition == nil || m.busy {
		return nil
	}
	evaluations, err := parseAmendmentEvaluations(m.editor.Value(), entry.Definition.Evaluations)
	if err != nil {
		m.status = err.Error()
		return nil
	}
	request := kernelteam.SubmitAmendmentEvaluationRequest{
		Scope: amendment.Scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision, Evaluations: evaluations,
	}
	m.busy, m.err = true, nil
	m.status = "Recording revision-bound Team evaluation…"
	return func() tea.Msg {
		updated, err := m.client.SubmitTeamDefinitionAmendmentEvaluation(m.ctx, entry.Deployment.ID, request)
		return teamAmendmentChanged{amendment: updated, action: "Team evaluation", err: err}
	}
}

func (m *Model) submitTeamAmendmentDecision(approved bool) tea.Cmd {
	amendment := m.selectedTeamAmendmentRecord()
	entry := m.selectedTeamDeploymentRecord()
	reason := strings.TrimSpace(m.editor.Value())
	if !m.canResolveSelectedTeamAmendment() || amendment == nil || entry == nil || entry.Deployment == nil || m.busy {
		return nil
	}
	if reason == "" {
		m.status = "Record a reason for the permanent Team amendment decision."
		return nil
	}
	request := kernelteam.ResolveAmendmentRequest{
		Scope: amendment.Scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision, Approved: approved,
		ActorType: m.config.Actor.Type, ActorID: m.config.Actor.ID, Reason: reason,
	}
	action := "Team amendment approval"
	if !approved {
		action = "Team amendment rejection"
	}
	m.busy, m.err = true, nil
	m.status = "Recording revision-bound Team decision…"
	return func() tea.Msg {
		updated, err := m.client.ResolveTeamDefinitionAmendment(m.ctx, entry.Deployment.ID, request)
		return teamAmendmentChanged{amendment: updated, action: action, err: err}
	}
}

func (m *Model) submitTeamAmendmentActivation() tea.Cmd {
	amendment := m.selectedTeamAmendmentRecord()
	entry := m.selectedTeamDeploymentRecord()
	reason := strings.TrimSpace(m.editor.Value())
	if !m.canActivateSelectedTeamAmendment() || amendment == nil || entry == nil || entry.Deployment == nil || m.busy {
		return nil
	}
	if reason == "" {
		m.status = "Record why this exact Team definition should become active."
		return nil
	}
	request := kernelapi.ActivateTeamDefinitionAmendmentRequest{
		Scope: amendment.Scope, ExpectedRevision: amendment.Revision,
		ActorType: m.config.Actor.Type, ActorID: m.config.Actor.ID, Reason: reason,
	}
	m.busy, m.err = true, nil
	m.status = "Atomically activating reviewed Team definition…"
	return func() tea.Msg {
		result, err := m.client.ActivateTeamDefinitionAmendment(m.ctx, entry.Deployment.ID, amendment.ID, request)
		if result == nil {
			return teamAmendmentChanged{action: "Team amendment activation", err: err}
		}
		return teamAmendmentChanged{amendment: result.Amendment, deployment: result.Deployment, action: "Team amendment activation", err: err}
	}
}

func parseAmendmentEvaluations(input string, criteria []workforce.EvaluationCriterion) ([]workforce.AmendmentEvaluation, error) {
	byID := make(map[string]workforce.AmendmentEvaluation, len(criteria))
	for _, raw := range strings.Split(strings.TrimSpace(input), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		criterionID, result, ok := strings.Cut(line, "=")
		outcome, summary, hasSummary := strings.Cut(result, ":")
		criterionID, outcome, summary = strings.TrimSpace(criterionID), strings.ToLower(strings.TrimSpace(outcome)), strings.TrimSpace(summary)
		if !ok || !hasSummary || criterionID == "" || summary == "" || (outcome != "pass" && outcome != "fail") {
			return nil, errors.New("Use one line per criterion: criterion-id=pass|fail: concise evidence summary")
		}
		if _, duplicate := byID[criterionID]; duplicate {
			return nil, fmt.Errorf("evaluation criterion %q was entered more than once", criterionID)
		}
		byID[criterionID] = workforce.AmendmentEvaluation{CriterionID: criterionID, Passed: outcome == "pass", Summary: summary}
	}
	result := make([]workforce.AmendmentEvaluation, 0, len(criteria))
	for _, criterion := range criteria {
		evaluation, ok := byID[criterion.ID]
		if !ok {
			return nil, fmt.Errorf("evaluation result is required for %q", criterion.ID)
		}
		result = append(result, evaluation)
		delete(byID, criterion.ID)
	}
	if len(byID) > 0 {
		return nil, errors.New("evaluation contains a criterion not declared by this definition")
	}
	return result, nil
}

func agentEvaluationPlaceholder(entry *kernelapi.AgentDeploymentCatalogEntry) string {
	if entry == nil || entry.Definition == nil || len(entry.Definition.Evaluations) == 0 {
		return "criterion-id=pass|fail: concise evidence summary"
	}
	lines := make([]string, 0, len(entry.Definition.Evaluations)+1)
	lines = append(lines, "One line per required criterion:")
	for _, criterion := range entry.Definition.Evaluations {
		lines = append(lines, criterion.ID+"=pass|fail: concise evidence summary")
	}
	return strings.Join(lines, "\n")
}

func teamEvaluationPlaceholder(entry *kernelapi.TeamDeploymentCatalogEntry) string {
	if entry == nil || entry.Definition == nil || len(entry.Definition.Evaluations) == 0 {
		return "criterion-id=pass|fail: concise evidence summary"
	}
	lines := make([]string, 0, len(entry.Definition.Evaluations)+1)
	lines = append(lines, "One line per required criterion:")
	for _, criterion := range entry.Definition.Evaluations {
		lines = append(lines, criterion.ID+"=pass|fail: concise evidence summary")
	}
	return strings.Join(lines, "\n")
}

func (m *Model) selectedSkillBindingRecord() *capability.Binding {
	if m.skillBindingSelected < 0 || m.skillBindingSelected >= len(m.skillBindings) {
		return nil
	}
	return m.skillBindings[m.skillBindingSelected]
}

func (m *Model) restoreSkillBindingSelection() {
	if len(m.skillBindings) == 0 {
		m.skillBindingSelected, m.selectedSkillBinding = 0, ""
		return
	}
	for index, binding := range m.skillBindings {
		if binding.ID == m.selectedSkillBinding {
			m.skillBindingSelected = index
			return
		}
	}
	m.skillBindingSelected = min(m.skillBindingSelected, len(m.skillBindings)-1)
	m.selectedSkillBinding = m.skillBindings[m.skillBindingSelected].ID
}

func (m *Model) moveSkillBindingSelection(delta int) {
	if len(m.skillBindings) == 0 {
		return
	}
	m.skillBindingSelected = max(0, min(len(m.skillBindings)-1, m.skillBindingSelected+delta))
	m.selectedSkillBinding = m.skillBindings[m.skillBindingSelected].ID
}
func (m *Model) selectedConversationRecord() *runtime.Conversation {
	if m.conversationSelected < 0 || m.conversationSelected >= len(m.conversations) {
		return nil
	}
	return m.conversations[m.conversationSelected]
}

func (m *Model) restoreConversationSelection() {
	if len(m.conversations) == 0 {
		m.conversationSelected = 0
		m.selectedConversation = ""
		m.channelMessages, m.channelRounds, m.channelPresence = nil, nil, nil
		m.channelAuditExpanded = false
		return
	}
	if m.selectedConversation != "" {
		for index, conversation := range m.conversations {
			if conversation.ID == m.selectedConversation {
				m.conversationSelected = index
				return
			}
		}
	}
	m.conversationSelected = min(m.conversationSelected, len(m.conversations)-1)
	m.selectedConversation = m.conversations[m.conversationSelected].ID
	m.channelAuditExpanded = false
}

func (m *Model) moveConversationSelection(delta int) {
	if len(m.conversations) == 0 {
		return
	}
	m.conversationSelected = max(0, min(len(m.conversations)-1, m.conversationSelected+delta))
	m.selectedConversation = m.conversations[m.conversationSelected].ID
	m.channelMessages, m.channelRounds, m.channelPresence = nil, nil, nil
	m.channelAuditExpanded = false
}

func (m *Model) downloadSelectedArtifact() tea.Cmd {
	artifact := m.selectedArtifactRecord()
	if artifact == nil || artifact.ContentAvailability != runtime.ArtifactContentAvailable || m.busy || !m.supportsArtifact(kernelapi.OperationDownload) {
		return nil
	}
	m.busy = true
	m.err = nil
	m.status = "Downloading and verifying artifact…"
	return func() tea.Msg {
		path, err := downloadArtifact(m.ctx, m.client, m.config.Scope, artifact, m.config.DownloadDir)
		return artifactDownloaded{path: path, err: err}
	}
}

func artifactSelectionKey(artifact *runtime.Artifact) string {
	if artifact == nil {
		return ""
	}
	return fmt.Sprintf("%s@%d", artifact.ID, artifact.Version)
}

func (m *Model) focusPanelList() {
	m.focus = focusPanel
	m.editor.Blur()
}

func (m *Model) focusComposerEditor() {
	m.focus = focusComposer
	m.editor.Focus()
}

func (m *Model) activeRunbookOperations() []runbookOperationRow {
	if m == nil || m.agentDeployment == nil || m.agentDeployment.Deployment == nil ||
		m.agentDeployment.Definition == nil || m.agentDeployment.Definition.Runbook == nil {
		return nil
	}
	definition := m.agentDeployment.Definition
	deployment := m.agentDeployment.Deployment
	if definition.ID != deployment.DefinitionID || definition.Version != deployment.ActiveVersion {
		return nil
	}
	if diagnostics := runbook.Validate(definition.Runbook); len(diagnostics) > 0 {
		return nil
	}
	entrypoints := make([]string, 0, len(definition.Runbook.Interfaces))
	for entrypoint := range definition.Runbook.Interfaces {
		if _, ok := definition.Runbook.Entrypoints[entrypoint]; ok {
			entrypoints = append(entrypoints, entrypoint)
		}
	}
	sort.Strings(entrypoints)
	rows := make([]runbookOperationRow, 0, len(entrypoints))
	for _, entrypoint := range entrypoints {
		rows = append(rows, runbookOperationRow{
			Focus: runbookOperationFocus{
				AgentID: definition.ID, AgentName: definition.DisplayName,
				RunbookID: definition.Runbook.ID, RunbookVersion: definition.Runbook.Version,
				Entrypoint: entrypoint,
			},
			Definition: definition,
			FirstStep:  definition.Runbook.Entrypoints[entrypoint],
		})
	}
	return rows
}

func (m *Model) selectedActiveRunbookOperation() *runbookOperationRow {
	rows := m.activeRunbookOperations()
	if len(rows) == 0 {
		return nil
	}
	m.activeRunbookSelected = max(0, min(len(rows)-1, m.activeRunbookSelected))
	row := rows[m.activeRunbookSelected]
	return &row
}

func (m *Model) moveActiveRunbookSelection(delta int) {
	rows := m.activeRunbookOperations()
	if len(rows) == 0 {
		m.activeRunbookSelected = 0
		return
	}
	m.activeRunbookSelected = max(0, min(len(rows)-1, m.activeRunbookSelected+delta))
}

func (m *Model) canStartActiveRunbookOperation() bool {
	row := m.selectedActiveRunbookOperation()
	return row != nil && m.agentDeployment != nil && m.agentDeployment.Deployment != nil &&
		m.agentDeployment.Deployment.RolloutStatus == kernelagent.RolloutActive &&
		m.supportsRun(kernelapi.OperationCreate)
}

func (m *Model) prepareActiveRunbookOperation() {
	row := m.selectedActiveRunbookOperation()
	if row == nil || !m.canStartActiveRunbookOperation() {
		return
	}
	contract := row.Definition.Runbook.Interfaces[row.Focus.Entrypoint]
	m.mode = modeRunbookOperationStart
	m.editor.Reset()
	if runbookInputAllowsEmpty(contract.InputSchema) {
		m.editor.SetValue("{}")
	}
	m.editor.Placeholder = runbookInputTemplate(contract.InputSchema)
	m.status = fmt.Sprintf("Starting %s · %s from the active definition.", row.Focus.AgentName, row.Focus.Entrypoint)
	m.focusComposerEditor()
}

func runbookInputAllowsEmpty(schema map[string]interface{}) bool {
	return len(schemaRequiredFields(schema)) == 0
}

func runbookInputTemplate(schema map[string]interface{}) string {
	properties, _ := schema["properties"].(map[string]interface{})
	if len(properties) == 0 {
		return "{}"
	}
	required := schemaRequiredFields(schema)
	example := make(map[string]interface{}, len(required))
	for _, name := range required {
		definition, _ := properties[name].(map[string]interface{})
		switch definition["type"] {
		case "boolean":
			example[name] = false
		case "integer", "number":
			example[name] = 0
		case "array":
			example[name] = []interface{}{}
		case "object":
			example[name] = map[string]interface{}{}
		default:
			example[name] = "<required>"
		}
	}
	encoded, _ := json.MarshalIndent(example, "", "  ")
	return string(encoded)
}

func schemaRequiredFields(schema map[string]interface{}) []string {
	fields := make([]string, 0)
	switch required := schema["required"].(type) {
	case []string:
		fields = append(fields, required...)
	case []interface{}:
		for _, value := range required {
			if field, ok := value.(string); ok {
				fields = append(fields, field)
			}
		}
	}
	sort.Strings(fields)
	return fields
}

func (m *Model) prepareRequestComposer(mode editorMode, placeholder string) {
	m.mode = mode
	m.editor.Reset()
	m.editor.Placeholder = placeholder
	m.focusComposerEditor()
}

func (m *Model) prepareAgentAmendmentComposer(mode editorMode, placeholder string) {
	m.mode = mode
	m.editor.Reset()
	m.editor.Placeholder = placeholder
	m.focusComposerEditor()
}

func (m *Model) prepareTeamAmendmentComposer(mode editorMode, placeholder string) {
	m.mode = mode
	m.editor.Reset()
	m.editor.Placeholder = placeholder
	m.focusComposerEditor()
}

func (m *Model) prepareComposerForSection() {
	switch {
	case m.section == sectionOverview && m.supportsAgentBuilder():
		m.section = sectionAuthoring
		m.mode = modeAgentBuilderPrompt
		m.editor.Placeholder = agentBuilderPromptPlaceholder
		m.focusComposerEditor()
	case m.section == sectionBundles && m.workforceBundleCapability.Supports(kernelapi.OperationInspect):
		m.mode = modeWorkforceBundleInspect
		m.editor.Placeholder = "Path to a signed workforce bundle…"
		m.focusComposerEditor()
	case m.section == sectionReadiness && m.canProposeAgentBehaviorAmendment():
		m.prepareAgentAmendmentComposer(modeAgentAmendmentPropose, "systemPrompt|personality\nConcise rationale\nNew immutable value")
	case m.section == sectionTeams && m.canProposeTeamPurposeAmendment():
		m.prepareTeamAmendmentComposer(modeTeamAmendmentPropose, "First line: concise rationale\nRemaining lines: the Team's new purpose")
	case m.section == sectionAuthoring && m.supportsAgentBuilder():
		m.mode = modeAgentBuilderPrompt
		m.editor.Placeholder = agentBuilderPromptPlaceholder
		m.focusComposerEditor()
	case m.section == sectionObjectives && m.supportsObjective(kernelapi.OperationCreate):
		m.mode = modeObjectiveCreate
		m.editor.Placeholder = "Describe the objective and desired outcome…"
		m.focusComposerEditor()
	case m.section == sectionIntegrations && m.canCreateConversationGateway():
		m.prepareConversationGatewayCreation()
	case m.section == sectionSkills && m.supportsSkillBinding(kernelapi.OperationUpsert):
		m.prepareSkillBindingComposer(nil)
	case m.section == sectionChannels && m.selectedConversationRecord() != nil && m.supportsChannel(kernelapi.OperationPost):
		m.mode = modeChannelPost
		m.editor.Placeholder = "Share an update or ask a question…"
		m.focusComposerEditor()
	case m.section == sectionChannels && m.supportsChannel(kernelapi.OperationCreate):
		m.mode = modeChannelCreate
		m.editor.Placeholder = "Name the Team channel…"
		m.focusComposerEditor()
	case m.section == sectionRequests && m.canRespondToSelectedRequest(runtime.AgentRequestDecisionProvideClarification):
		m.mode = modeRequestProvideClarification
		m.editor.Placeholder = "Provide the clarification requested by the recipient…"
		m.focusComposerEditor()
	case m.section == sectionRequests && m.canCompleteSelectedAgentRequest():
		m.mode = modeRequestComplete
		m.editor.Placeholder = "Summarize the completed outcome…"
		m.focusComposerEditor()
	case m.section == sectionRequests && m.canCreateAgentRequestFromSelectedRun():
		m.mode = modeRequestCreate
		m.editor.Placeholder = "First line: agent:researcher or handoff team:marketing\nRemaining lines: requested outcome"
		m.focusComposerEditor()
	case m.section == sectionApprovals && m.canResolveSelectedActionApproval():
		m.mode = modeApprovalApprove
		m.editor.Placeholder = "Record why this exact action is safe to approve…"
		m.focusComposerEditor()
	case m.supportsRun(kernelapi.OperationCreate):
		m.mode = modeCreate
		m.editor.Placeholder = "Describe the outcome you want…"
		m.focusComposerEditor()
	}
}

func (m *Model) resetComposerMode() {
	if m.section == sectionOverview {
		if m.supportsAgentBuilder() {
			m.mode = modeAgentBuilderPrompt
			m.editor.Placeholder = agentBuilderPromptPlaceholder
		} else {
			m.mode = modeCreate
			m.editor.Placeholder = "Choose a destination to get started."
		}
		return
	}
	if m.section == sectionBundles {
		m.mode = modeWorkforceBundleInspect
		m.editor.Placeholder = "Path to a signed workforce bundle…"
		return
	}
	if m.section == sectionReadiness {
		m.mode = modeCreate
		m.editor.Placeholder = "Select an Agent amendment to inspect its governed lifecycle."
		return
	}
	if m.section == sectionTeams {
		m.mode = modeCreate
		m.editor.Placeholder = "Select a Team amendment to inspect its governed lifecycle."
		return
	}
	if m.section == sectionAuthoring {
		m.mode = modeAgentBuilderPrompt
		m.editor.Placeholder = agentBuilderPromptPlaceholder
		return
	}
	if m.section == sectionObjectives {
		m.mode = modeObjectiveCreate
		m.editor.Placeholder = "Describe the objective and desired outcome…"
		return
	}
	if m.section == sectionIntegrations {
		m.mode = modeCreate
		m.editor.Placeholder = "Select a connection, or press n to add one in a paused state."
		return
	}
	if m.section == sectionSkills {
		if m.supportsSkillBinding(kernelapi.OperationUpsert) {
			m.mode = modeSkillBindingUpsert
			m.editor.Placeholder = "Describe the exact Skill authority…"
		}
		return
	}
	if m.section == sectionChannels && m.selectedConversationRecord() != nil {
		m.mode = modeChannelPost
		m.editor.Placeholder = "Share an update or ask a question…"
		return
	}
	if m.section == sectionRequests {
		if m.canCreateAgentRequestFromSelectedRun() {
			m.mode = modeRequestCreate
			m.editor.Placeholder = "First line: agent:researcher or handoff team:marketing\nRemaining lines: requested outcome"
		} else {
			m.mode = modeCreate
			m.editor.Placeholder = "Select a request to inspect its available actions."
		}
		return
	}
	if m.section == sectionApprovals {
		m.mode = modeCreate
		m.editor.Placeholder = "Select an approval to inspect its policy and evidence."
		return
	}
	m.mode = modeCreate
	m.editor.Placeholder = "Describe the outcome you want…"
}

func (m *Model) canCreateAgentRequestFromSelectedRun() bool {
	return m.supportsAgentRequest(kernelapi.OperationCreate) && runtime.CanStartAgentRequestFromRun(m.selectedRun())
}

func agentRequestDecisionLabel(decision runtime.AgentRequestDecision) string {
	switch decision {
	case runtime.AgentRequestDecisionAccept:
		return "Accepting request"
	case runtime.AgentRequestDecisionReject:
		return "Rejecting request"
	case runtime.AgentRequestDecisionRequestClarification:
		return "Requesting clarification"
	case runtime.AgentRequestDecisionProvideClarification:
		return "Providing clarification"
	default:
		return "Updating request"
	}
}

func objectiveTitle(prompt string) string {
	title := strings.TrimSpace(strings.SplitN(prompt, "\n", 2)[0])
	if len(title) > 120 {
		title = strings.TrimSpace(title[:120])
	}
	return title
}

func (m *Model) operatorParticipant() runtime.ConversationParticipant {
	participantType := runtime.ConversationParticipantUser
	switch strings.ToLower(strings.TrimSpace(m.config.Actor.Type)) {
	case "agent":
		participantType = runtime.ConversationParticipantAgent
	case "team":
		participantType = runtime.ConversationParticipantTeam
	case "service", "system":
		participantType = runtime.ConversationParticipantService
	}
	id := strings.TrimSpace(m.config.Actor.ID)
	if id == "" {
		id = "local"
	}
	return runtime.ConversationParticipant{Type: participantType, ID: id}
}

func conversationClient(kernelClient client.KernelClient) client.ConversationClient {
	conversationClient, _ := kernelClient.(client.ConversationClient)
	return conversationClient
}

func agentLifecycleClient(kernelClient client.KernelClient) client.AgentDefinitionLifecycleClient {
	value, _ := kernelClient.(client.AgentDefinitionLifecycleClient)
	return value
}

func agentCapabilityClient(kernelClient client.KernelClient) client.AgentDefinitionCapabilityClient {
	value, _ := kernelClient.(client.AgentDefinitionCapabilityClient)
	return value
}

func skillBindingClient(kernelClient client.KernelClient) client.SkillBindingClient {
	value, _ := kernelClient.(client.SkillBindingClient)
	return value
}

func agentBuilderClient(kernelClient client.KernelClient) agentBuilderConversationClient {
	value, _ := kernelClient.(agentBuilderConversationClient)
	return value
}

func workforceBundleClient(kernelClient client.KernelClient) client.WorkforceBundleClient {
	value, _ := kernelClient.(client.WorkforceBundleClient)
	return value
}

func isHTTPStatus(err error, status int) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

func isCursorRefreshConflict(err error) bool {
	return isHTTPStatus(err, http.StatusConflict)
}

func isTerminal(status runtime.AgentRunStatus) bool {
	return status == runtime.AgentRunStatusCompleted || status == runtime.AgentRunStatusFailed || status == runtime.AgentRunStatusCanceled
}

func commandSuccessMessage(kind runtime.AgentRunCommandKind) string {
	switch kind {
	case runtime.AgentRunCommandPause:
		return "Work paused safely."
	case runtime.AgentRunCommandResume:
		return "Work resumed."
	case runtime.AgentRunCommandCancel:
		return "Work stopped."
	case runtime.AgentRunCommandIntervene:
		return "Guidance delivered and recorded in the audit trail."
	default:
		return "Work updated."
	}
}

var _ tea.Model = (*Model)(nil)
