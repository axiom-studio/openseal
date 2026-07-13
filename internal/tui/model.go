// Package tui implements OpenSeal's prompt-first terminal client. It owns no
// durable state: every action is discovered from and sent to the public kernel
// API used by other embedding surfaces.
package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
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
	sectionAuthoring panelSection = iota
	sectionReadiness
	sectionObjectives
	sectionInitiatives
	sectionSkills
	sectionRuns
	sectionRequests
	sectionApprovals
	sectionArtifacts
	sectionChannels
)

type editorMode int

const (
	modeCreate editorMode = iota
	modeWorkforceAuthoring
	modeGuide
	modeChannelCreate
	modeChannelPost
	modeObjectiveCreate
	modeObjectiveEdit
	modeInitiativeCreate
	modeInitiativeEdit
	modeSkillInstall
	modeSkillPin
	modeSkillRemove
	modeWorkforceApprove
	modeWorkforceReject
	modeWorkforceApply
	modeWorkforceRetry
	modeRequestCreate
	modeRequestAccept
	modeRequestReject
	modeRequestClarify
	modeRequestProvideClarification
	modeRequestComplete
	modeApprovalApprove
	modeApprovalReject
)

type Model struct {
	ctx                         context.Context
	client                      client.KernelClient
	conversationClient          client.ConversationClient
	clawHubClient               client.ClawHubClient
	config                      Config
	editor                      textarea.Model
	focus                       focusArea
	section                     panelSection
	mode                        editorMode
	width                       int
	height                      int
	loading                     bool
	busy                        bool
	ready                       bool
	unavailable                 string
	err                         error
	status                      string
	runCapability               kernelapi.Capability
	requestCapability           kernelapi.Capability
	approvalCapability          kernelapi.Capability
	objectiveCapability         kernelapi.Capability
	initiativeCapability        kernelapi.Capability
	sourceMonitorCapability     kernelapi.Capability
	clawHubCapability           kernelapi.Capability
	artifactCapability          kernelapi.Capability
	channelCapability           kernelapi.Capability
	authoringCapability         kernelapi.Capability
	agentDefinitionCapability   kernelapi.Capability
	authoringResult             *authoring.CompileResult
	authoringChangeSet          *authoring.ChangeSet
	authoringAmendment          bool
	authoringApprovalSelected   int
	runs                        []*runtime.AgentRun
	agentRequests               []*runtime.AgentRequest
	agentRequestSelected        int
	selectedAgentRequest        string
	actionApprovals             []*runtime.ApprovalCheckpoint
	actionApprovalSelected      int
	selectedActionApproval      string
	compilations                []*kernelagent.DefinitionCompilation
	objectives                  []*runtime.Objective
	objectiveSelected           int
	selectedObjective           string
	initiatives                 []*runtime.Initiative
	initiativeSelected          int
	selectedInitiative          string
	sourceMonitorStatuses       map[string]sourceMonitorStatus
	clawHubSkills               []clawhub.InstalledState
	clawHubSelected             int
	selectedClawHub             string
	selected                    int
	selectedID                  string
	artifacts                   []*runtime.Artifact
	artifactSelected            int
	selectedArtifact            string
	artifactExpanded            bool
	conversations               []*runtime.Conversation
	conversationSelected        int
	selectedConversation        string
	channelMessages             []*runtime.ChannelMessage
	channelRounds               []*runtime.ParticipationRoundResult
	channelPresence             []*runtime.ConversationPresence
	channelAuditExpanded        bool
	pendingKey                  string
	pendingGoal                 string
	pendingAuthoringKey         string
	pendingAuthoringPrompt      string
	pendingAuthoringParentID    string
	pendingGovernanceKey        string
	pendingGovernanceIntent     string
	pendingObjectiveKey         string
	pendingObjectivePrompt      string
	pendingInitiativeKey        string
	pendingInitiativePrompt     string
	pendingClawHubPrompt        string
	pendingConversationKey      string
	pendingConversationTitle    string
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

type workforceCompiled struct {
	result    *authoring.CompileResult
	changeSet *authoring.ChangeSet
	mode      authoring.Mode
	err       error
}

type workforceGoverned struct {
	changeSet *authoring.ChangeSet
	action    string
	err       error
}

type workforceLoaded struct {
	changeSet *authoring.ChangeSet
	err       error
}

type runsLoaded struct {
	runs []*runtime.AgentRun
	err  error
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

type compilationsLoaded struct {
	compilations []*kernelagent.DefinitionCompilation
	err          error
}

type objectivesLoaded struct {
	objectives []*runtime.Objective
	err        error
}

type objectiveCreated struct {
	objective *runtime.Objective
	err       error
}

type objectiveUpdated struct {
	objective *runtime.Objective
	err       error
}

type initiativesLoaded struct {
	initiatives []*runtime.Initiative
	err         error
}

type sourceMonitorStatus struct {
	checkpoint   *runtime.SourceMonitorCheckpoint
	observations []*runtime.SourceObservation
	err          error
}

type sourceMonitorsLoaded struct {
	statuses map[string]sourceMonitorStatus
}
type initiativeCreated struct {
	initiative *runtime.Initiative
	err        error
}
type initiativeUpdated struct {
	initiative *runtime.Initiative
	err        error
}

type clawHubSkillsLoaded struct {
	skills []clawhub.InstalledState
	err    error
}
type clawHubLifecycleCompleted struct {
	result    *clawhub.LifecycleResult
	batch     *clawhub.LifecycleBatchResult
	operation clawhub.LifecycleOperation
	err       error
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
		focus: focusComposer, section: sectionAuthoring, mode: modeWorkforceAuthoring, width: 100, height: 30,
		conversationClient: conversationClient(kernelClient),
		clawHubClient:      clawHubClient(kernelClient),
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
		requestCapability, hasRequests := msg.document.Find(kernelapi.AgentRequestsCapabilityID, kernelapi.AgentRequestsCapabilityVersion)
		approvalCapability, hasApprovals := msg.document.Find(kernelapi.ActionApprovalsCapabilityID, kernelapi.ActionApprovalsCapabilityVersion)
		objectiveCapability, hasObjectives := msg.document.Find(kernelapi.ObjectivesCapabilityID, kernelapi.ObjectivesCapabilityVersion)
		initiativeCapability, hasInitiatives := msg.document.Find(kernelapi.InitiativesCapabilityID, kernelapi.InitiativesCapabilityVersion)
		sourceMonitorCapability, _ := msg.document.Find(kernelapi.SourceMonitorsCapabilityID, kernelapi.SourceMonitorsCapabilityVersion)
		clawHubCapability, hasClawHub := msg.document.Find(kernelapi.ClawHubLifecycleCapabilityID, kernelapi.ClawHubLifecycleCapabilityVersion)
		artifactCapability, hasArtifacts := msg.document.Find(kernelapi.ArtifactsCapabilityID, kernelapi.ArtifactsCapabilityVersion)
		channelCapability, hasChannels := msg.document.Find(kernelapi.ChannelsCapabilityID, kernelapi.ChannelsCapabilityVersion)
		authoringCapability, hasAuthoring := msg.document.Find(kernelapi.WorkforceAuthoringCapabilityID, kernelapi.WorkforceAuthoringCapabilityVersion)
		agentDefinitionCapability, hasAgentDefinitions := msg.document.Find(kernelapi.AgentDefinitionsCapabilityID, kernelapi.AgentDefinitionsCapabilityVersion)
		m.runCapability = runCapability
		m.requestCapability = requestCapability
		m.approvalCapability = approvalCapability
		m.objectiveCapability = objectiveCapability
		m.initiativeCapability = initiativeCapability
		m.sourceMonitorCapability = sourceMonitorCapability
		m.clawHubCapability = clawHubCapability
		m.artifactCapability = artifactCapability
		m.channelCapability = channelCapability
		m.authoringCapability = authoringCapability
		m.agentDefinitionCapability = agentDefinitionCapability
		if authoringCapability.Context == nil || len(authoringCapability.Context.EligibleApprovalRequirements) == 0 {
			m.authoringApprovalSelected = 0
		} else {
			m.authoringApprovalSelected = min(m.authoringApprovalSelected, len(authoringCapability.Context.EligibleApprovalRequirements)-1)
		}
		if !hasRuns || !runCapability.Available {
			m.runCapability = kernelapi.Capability{}
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
		if !hasInitiatives || !initiativeCapability.Available {
			m.initiativeCapability = kernelapi.Capability{}
		}
		if !hasClawHub || !clawHubCapability.Available || m.clawHubClient == nil {
			m.clawHubCapability = kernelapi.Capability{}
		}
		if !hasArtifacts || !artifactCapability.Available {
			m.artifactCapability = kernelapi.Capability{}
		}
		if !hasChannels || !channelCapability.Available || m.conversationClient == nil {
			m.channelCapability = kernelapi.Capability{}
		}
		if !hasAuthoring || !authoringCapability.Available {
			m.authoringCapability = kernelapi.Capability{}
		}
		if !hasAgentDefinitions || !agentDefinitionCapability.Available || m.config.Owner.Type != runtime.OwnerTypeAgent {
			m.agentDefinitionCapability = kernelapi.Capability{}
		}
		if !m.objectiveCapability.Available && !m.initiativeCapability.Available && !m.clawHubCapability.Available && !m.runCapability.Available && !m.requestCapability.Available && !m.approvalCapability.Available && !m.artifactCapability.Available && !m.channelCapability.Available && !m.authoringCapability.Available && !m.agentDefinitionCapability.Available {
			m.unavailable = "This server does not advertise workforce authoring, objectives, Initiatives, canonical work, requests, approvals, Team channels, or artifact evidence."
			m.ready = false
			return m, nil
		}
		m.ready = true
		m.unavailable = ""
		m.err = nil
		if m.authoringCapability.Available {
			m.section = sectionAuthoring
			m.mode = modeWorkforceAuthoring
			if m.authoringResult != nil {
				m.editor.Placeholder = "Describe what should change…"
			} else {
				m.editor.Placeholder = "Describe the Agents and Team you need…"
			}
		} else if m.objectiveCapability.Available {
			m.section = sectionObjectives
			m.mode = modeObjectiveCreate
			m.editor.Placeholder = "Describe the objective and desired outcome…"
		} else if m.initiativeCapability.Available {
			m.section = sectionInitiatives
			m.mode = modeInitiativeCreate
			m.editor.Placeholder = "Describe the Initiative outcome…"
		} else if m.clawHubCapability.Available {
			m.section = sectionSkills
			m.mode = modeSkillInstall
			m.editor.Placeholder = "Enter @owner/skill to install…"
		} else if !m.objectiveCapability.Available && m.runCapability.Available {
			m.section = sectionRuns
			m.mode = modeCreate
		} else if m.requestCapability.Available {
			m.section = sectionRequests
			m.focusPanelList()
		} else if m.approvalCapability.Available {
			m.section = sectionApprovals
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
		}
		return m, tea.Batch(m.loadCompilations(), m.loadObjectives(), m.loadInitiatives(), m.loadClawHubSkills(), m.loadRuns(), m.loadAgentRequests(), m.loadActionApprovals(), m.loadArtifacts(), m.loadConversations())
	case workforceCompiled:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Authoring failed. Your prompt is preserved for retry."
			return m, nil
		}
		m.err = nil
		m.authoringResult = msg.result
		m.authoringChangeSet = msg.changeSet
		if msg.changeSet != nil && msg.changeSet.Status == authoring.ChangeSetEvaluating && strings.TrimSpace(msg.changeSet.CandidateDigest) == "" {
			m.authoringResult = nil
		}
		m.authoringAmendment = msg.mode == authoring.ModeAmend
		m.pendingAuthoringKey, m.pendingAuthoringPrompt, m.pendingAuthoringParentID = "", "", ""
		m.editor.Reset()
		m.editor.Placeholder = "Describe what should change…"
		if msg.changeSet != nil {
			if msg.changeSet.Status == authoring.ChangeSetEvaluating {
				m.status = "Proposal queued. OpenSeal is generating it durably; you may safely leave."
			} else {
				m.status = "Workforce change set saved for governed review. Nothing has been activated."
			}
		} else {
			m.status = "Workforce candidate compiled. Nothing has been activated."
		}
		m.section = sectionAuthoring
		m.focusPanelList()
		return m, m.loadCapabilities()
	case workforceGoverned:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = msg.action + " failed. Your reason and retry identity are preserved."
			return m, nil
		}
		m.err = nil
		m.authoringChangeSet = msg.changeSet
		if msg.changeSet != nil && (msg.changeSet.Status != authoring.ChangeSetEvaluating || strings.TrimSpace(msg.changeSet.CandidateDigest) != "") {
			m.authoringResult = &msg.changeSet.Result
		} else {
			m.authoringResult = nil
		}
		m.pendingGovernanceKey, m.pendingGovernanceIntent = "", ""
		m.editor.Reset()
		m.resetComposerMode()
		m.status = msg.action + " recorded in the durable workforce audit."
		m.focusPanelList()
		return m, m.loadCapabilities()
	case workforceLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.authoringChangeSet = msg.changeSet
		if msg.changeSet != nil && (msg.changeSet.Status != authoring.ChangeSetEvaluating || strings.TrimSpace(msg.changeSet.CandidateDigest) != "") {
			m.authoringResult = &msg.changeSet.Result
		} else {
			m.authoringResult = nil
		}
		return m, m.loadCapabilities()
	case objectivesLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.objectives = msg.objectives
		m.restoreObjectiveSelection()
		return m, nil
	case initiativesLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err, m.initiatives = nil, msg.initiatives
		m.restoreInitiativeSelection()
		return m, m.loadSourceMonitors()
	case sourceMonitorsLoaded:
		m.sourceMonitorStatuses = msg.statuses
		return m, nil
	case clawHubSkillsLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.clawHubSkills = msg.skills
		m.restoreClawHubSelection()
		return m, nil
	case clawHubLifecycleCompleted:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Skill lifecycle operation failed. The draft is preserved for retry."
			return m, m.loadClawHubSkills()
		}
		m.err = nil
		m.editor.Reset()
		m.pendingClawHubPrompt = ""
		m.resetComposerMode()
		m.focusPanelList()
		if msg.batch != nil {
			changed := 0
			for _, item := range msg.batch.Results {
				if item.Changed {
					changed++
				}
			}
			m.status = fmt.Sprintf("Skill catalog checked · %d changed · %d total.", changed, len(msg.batch.Results))
		} else if msg.result != nil {
			m.selectedClawHub = msg.result.SourceIdentity
			m.status = fmt.Sprintf("Skill %s · %s.", msg.operation, msg.result.Outcome)
		}
		return m, m.loadClawHubSkills()
	case runsLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.runs = msg.runs
		m.restoreSelection()
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
		return m, tea.Batch(m.loadActionApprovals(), m.loadRuns())
	case compilationsLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.compilations = msg.compilations
		return m, nil
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
	case initiativeCreated:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Initiative creation failed. Your prompt is preserved for retry."
			return m, nil
		}
		m.err = nil
		m.pendingInitiativeKey, m.pendingInitiativePrompt = "", ""
		m.editor.Reset()
		m.selectedInitiative = msg.initiative.ID
		m.status = "Initiative added to the durable portfolio."
		m.section = sectionInitiatives
		m.focusPanelList()
		return m, m.loadInitiatives()
	case initiativeUpdated:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "The Initiative changed elsewhere. Refresh and try again."
			return m, m.loadInitiatives()
		}
		m.err = nil
		m.editor.Reset()
		m.selectedInitiative = msg.initiative.ID
		m.status = "Initiative revision recorded."
		m.resetComposerMode()
		m.focusPanelList()
		return m, m.loadInitiatives()
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
			commands = append(commands, m.loadCompilations(), m.loadObjectives(), m.loadInitiatives(), m.loadClawHubSkills(), m.loadRuns(), m.loadAgentRequests(), m.loadActionApprovals(), m.loadArtifacts(), m.loadConversations())
			if m.authoringChangeSet != nil {
				commands = append(commands, m.loadWorkforceChangeSet())
			}
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
	switch key {
	case "ctrl+c":
		return m, tea.Quit
	case "tab", "ctrl+i":
		if m.focus == focusComposer {
			m.focusPanelList()
		} else {
			m.prepareComposerForSection()
		}
		return m, nil
	case "esc":
		if m.mode != modeCreate {
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
			case modeChannelCreate:
				return m, m.submitConversation()
			case modeChannelPost:
				return m, m.submitChannelMessage()
			case modeObjectiveCreate:
				return m, m.submitObjective()
			case modeObjectiveEdit:
				return m, m.submitObjectiveAmendment()
			case modeInitiativeCreate:
				return m, m.submitInitiative()
			case modeInitiativeEdit:
				return m, m.submitInitiativeAmendment()
			case modeSkillInstall:
				return m, m.submitClawHubInstall()
			case modeSkillPin:
				return m, m.submitClawHubPin()
			case modeSkillRemove:
				return m, m.submitClawHubRemoval()
			case modeWorkforceAuthoring:
				return m, m.submitWorkforceAuthoring()
			case modeWorkforceApprove:
				return m, m.submitWorkforceApproval(true)
			case modeWorkforceReject:
				return m, m.submitWorkforceApproval(false)
			case modeWorkforceApply:
				return m, m.submitWorkforceApply()
			case modeWorkforceRetry:
				return m, m.submitWorkforceRetry()
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
			default:
				return m, m.submitRun()
			}
		}
	}

	if m.focus == focusPanel {
		switch key {
		case "up", "k":
			if m.section == sectionAuthoring && m.canResolveWorkforceApproval() {
				m.moveWorkforceApprovalSelection(-1)
			} else {
				m.movePanelSelection(-1)
			}
			if m.section == sectionChannels {
				return m, m.loadSelectedConversation()
			}
		case "down", "j":
			if m.section == sectionAuthoring && m.canResolveWorkforceApproval() {
				m.moveWorkforceApprovalSelection(1)
			} else {
				m.movePanelSelection(1)
			}
			if m.section == sectionChannels {
				return m, m.loadSelectedConversation()
			}
		case "w":
			if m.runCapability.Available {
				m.section = sectionRuns
			}
		case "R":
			if m.requestCapability.Available {
				m.section = sectionRequests
			}
		case "A":
			if m.approvalCapability.Available {
				m.section = sectionApprovals
			}
		case "f":
			if m.authoringCapability.Available {
				m.section = sectionAuthoring
			}
		case "h":
			if m.agentDefinitionCapability.Available {
				m.section = sectionReadiness
				return m, m.loadCompilations()
			}
		case "o":
			if m.objectiveCapability.Available {
				m.section = sectionObjectives
			}
		case "i":
			if m.initiativeCapability.Available {
				m.section = sectionInitiatives
			}
		case "s":
			if m.clawHubCapability.Available {
				m.section = sectionSkills
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
			if m.section == sectionRequests && m.supportsAgentRequest(kernelapi.OperationCreate) && m.selectedRun() != nil {
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
			} else if m.section == sectionInitiatives && m.supportsInitiative(kernelapi.OperationCreate) {
				m.mode = modeInitiativeCreate
				m.editor.Reset()
				m.editor.Placeholder = "Describe the Initiative outcome…"
				m.focusComposerEditor()
			} else if m.section == sectionSkills && m.supportsClawHub(clawhub.LifecycleInstall) {
				m.mode = modeSkillInstall
				m.editor.Reset()
				m.editor.Placeholder = "Enter @owner/skill to install…"
				m.focusComposerEditor()
			} else if m.section == sectionRuns && m.supportsRun(kernelapi.OperationCreate) {
				m.mode = modeCreate
				m.editor.Placeholder = "Describe the outcome you want…"
				m.focusComposerEditor()
			}
		case "r":
			if m.section == sectionAuthoring && m.canRetryWorkforce() {
				m.prepareWorkforceGovernanceComposer(modeWorkforceRetry, "Why should generation be retried?…")
				return m, nil
			}
			return m, m.loadPanel()
		case "m":
			if m.section == sectionChannels && m.selectedConversationRecord() != nil && m.supportsChannel(kernelapi.OperationPost) {
				m.mode = modeChannelPost
				m.editor.Reset()
				m.editor.Placeholder = "Share an update or ask a question…"
				m.focusComposerEditor()
			}
		case "p":
			if m.section == sectionRuns {
				return m, m.pauseOrResume()
			} else if m.section == sectionInitiatives {
				return m, m.pauseOrResumeInitiative()
			} else if m.section == sectionSkills {
				return m, m.pinOrUnpinClawHub()
			}
		case "l":
			if m.section == sectionInitiatives {
				return m, m.toggleSelectedObjectiveLink()
			}
		case "x":
			if m.section == sectionRuns {
				return m, m.commandSelected(runtime.AgentRunCommandCancel, "")
			} else if m.section == sectionRequests && m.canRespondToSelectedRequest(runtime.AgentRequestDecisionReject) {
				m.prepareRequestComposer(modeRequestReject, "Explain why this request cannot be accepted…")
			} else if m.section == sectionApprovals && m.canResolveSelectedActionApproval() {
				m.prepareRequestComposer(modeApprovalReject, "Explain why this action must not proceed…")
			} else if m.section == sectionAuthoring && m.canResolveWorkforceApproval() {
				m.prepareWorkforceGovernanceComposer(modeWorkforceReject, "Explain why this proposal must be rejected…")
			} else if m.section == sectionSkills && m.selectedClawHubRecord() != nil && m.supportsClawHub(clawhub.LifecycleUninstall) {
				m.mode = modeSkillRemove
				m.editor.Reset()
				m.editor.Placeholder = "Type REMOVE to confirm…"
				m.focusComposerEditor()
			}
		case "y":
			if m.section == sectionAuthoring && m.canResolveWorkforceApproval() {
				m.prepareWorkforceGovernanceComposer(modeWorkforceApprove, "Record why this requirement is satisfied…")
			} else if m.section == sectionRequests && m.canRespondToSelectedRequest(runtime.AgentRequestDecisionAccept) {
				placeholder := "Optionally record a concise acceptance note…"
				if request := m.selectedAgentRequestRecord(); request != nil && request.Recipient.Type == runtime.OwnerTypeTeam {
					placeholder = "First line: assigned Agent ID\nOptional remaining lines: acceptance note"
				}
				m.prepareRequestComposer(modeRequestAccept, placeholder)
			} else if m.section == sectionApprovals && m.canResolveSelectedActionApproval() {
				m.prepareRequestComposer(modeApprovalApprove, "Record why this exact action is safe to approve…")
			}
		case "?":
			if m.section == sectionRequests && m.canRespondToSelectedRequest(runtime.AgentRequestDecisionRequestClarification) {
				m.prepareRequestComposer(modeRequestClarify, "Ask the requester for the missing information…")
			}
		case "M":
			if m.section == sectionRequests && m.canRespondToSelectedRequest(runtime.AgentRequestDecisionProvideClarification) {
				m.prepareRequestComposer(modeRequestProvideClarification, "Provide the clarification requested by the recipient…")
			}
		case "g":
			if m.section == sectionRuns {
				if run := m.selectedRun(); run != nil && m.supportsRun(kernelapi.OperationIntervene) && !isTerminal(run.Status) {
					m.mode = modeGuide
					m.editor.Reset()
					m.editor.Placeholder = "Give concise guidance without replacing the objective…"
					m.focusComposerEditor()
				}
			}
		case "e", "enter":
			if m.section == sectionAuthoring && m.canApplyWorkforce() {
				m.prepareWorkforceGovernanceComposer(modeWorkforceApply, "Why should this reviewed workforce be created now?…")
			} else if m.section == sectionObjectives && m.selectedObjectiveRecord() != nil && m.supportsObjective(kernelapi.OperationUpdate) {
				m.mode = modeObjectiveEdit
				m.editor.Reset()
				m.editor.Placeholder = "Describe the amended objective…"
				m.focusComposerEditor()
			} else if m.section == sectionInitiatives && m.selectedInitiativeRecord() != nil && m.supportsInitiative(kernelapi.OperationPatch) {
				m.mode = modeInitiativeEdit
				m.editor.Reset()
				m.editor.Placeholder = "Describe the amended Initiative purpose…"
				m.focusComposerEditor()
			} else if m.section == sectionRequests && m.canCompleteSelectedAgentRequest() {
				m.prepareRequestComposer(modeRequestComplete, "Summarize the completed outcome…")
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
			if m.section == sectionSkills {
				return m, m.updateSelectedClawHub()
			}
		case "U":
			if m.section == sectionSkills {
				return m, m.updateAllClawHub()
			}
		case "v":
			if m.section == sectionSkills {
				return m, m.verifySelectedClawHub()
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
		if m.authoringChangeSet != nil {
			document, err = m.client.WorkforceChangeSetCapabilities(m.ctx, m.authoringChangeSet.Scope, m.authoringChangeSet.ID)
		} else {
			document, err = m.client.Capabilities(m.ctx)
		}
		return capabilitiesLoaded{document: document, err: err}
	}
}

func (m *Model) loadWorkforceChangeSet() tea.Cmd {
	if m.authoringChangeSet == nil {
		return nil
	}
	m.loading = true
	scope, id := m.authoringChangeSet.Scope, m.authoringChangeSet.ID
	return func() tea.Msg {
		changeSet, err := m.client.GetWorkforceChangeSet(m.ctx, scope, id)
		return workforceLoaded{changeSet: changeSet, err: err}
	}
}

func (m *Model) submitWorkforceAuthoring() tea.Cmd {
	prompt := strings.TrimSpace(m.editor.Value())
	if !m.supportsWorkforceAuthoring() || m.busy || prompt == "" {
		if prompt == "" {
			m.status = "Describe the workforce before compiling it."
		}
		return nil
	}
	m.busy = true
	m.err = nil
	m.status = "Saving a reviewable Agent and Team change set…"
	if m.supportsAuthoring(kernelapi.OperationPropose) {
		parentID := ""
		if m.authoringChangeSet != nil {
			parentID = m.authoringChangeSet.ID
		}
		if m.pendingAuthoringKey == "" || m.pendingAuthoringPrompt != prompt || m.pendingAuthoringParentID != parentID {
			m.pendingAuthoringKey = uuid.NewString()
			m.pendingAuthoringPrompt = prompt
			m.pendingAuthoringParentID = parentID
		}
		request := authoring.CreateChangeSetRequest{
			Scope:    capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID},
			ParentID: parentID, Prompt: prompt, Catalog: authoring.CapabilityCatalog{},
			Actor: authoring.ChangeSetActor{Type: m.config.Actor.Type, ID: m.config.Actor.ID},
		}
		idempotencyKey := m.pendingAuthoringKey
		return func() tea.Msg {
			changeSet, err := m.client.CreateWorkforceChangeSet(m.ctx, request, idempotencyKey)
			if err != nil {
				return workforceCompiled{err: err}
			}
			if changeSet == nil {
				return workforceCompiled{err: errors.New("workforce authoring returned no change set")}
			}
			return workforceCompiled{result: &changeSet.Result, changeSet: changeSet, mode: changeSet.Mode}
		}
	}
	request := authoring.GenerateRequest{Mode: authoring.ModeCreate, Prompt: prompt, Catalog: authoring.CapabilityCatalog{}}
	if m.authoringResult != nil {
		request.Mode = authoring.ModeAmend
		existing := m.authoringResult.Candidate
		request.Existing = &existing
	}
	return func() tea.Msg {
		result, err := m.client.CompileWorkforce(m.ctx, request)
		return workforceCompiled{result: result, mode: request.Mode, err: err}
	}
}

func (m *Model) submitWorkforceApproval(approved bool) tea.Cmd {
	reason := strings.TrimSpace(m.editor.Value())
	reference, ok := m.selectedWorkforceApprovalRequirement()
	if !ok || m.busy || reason == "" {
		if reason == "" {
			m.status = "Record a reason for the permanent approval audit."
		}
		return nil
	}
	changeSet := m.authoringChangeSet
	intent := fmt.Sprintf("approval\x00%s\x00%d\x00%s\x00%s\x00%s\x00%t\x00%s", changeSet.ID, changeSet.Revision, reference.EvaluationID, reference.PolicyID, reference.Role, approved, reason)
	if m.pendingGovernanceKey == "" || m.pendingGovernanceIntent != intent {
		m.pendingGovernanceKey, m.pendingGovernanceIntent = uuid.NewString(), intent
	}
	request := authoring.ResolveChangeSetApprovalRequest{
		Scope: changeSet.Scope, ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision,
		EvaluationID: reference.EvaluationID, PolicyID: reference.PolicyID, Role: reference.Role,
		Approved: approved, Reason: reason, Actor: authoring.ChangeSetActor{Type: m.config.Actor.Type, ID: m.config.Actor.ID},
	}
	key := m.pendingGovernanceKey
	m.busy, m.err = true, nil
	action := "Approval"
	if !approved {
		action = "Rejection"
	}
	m.status = action + " is being recorded…"
	return func() tea.Msg {
		result, err := m.client.ResolveWorkforceChangeSetApproval(m.ctx, request, key)
		return workforceGoverned{changeSet: result, action: action, err: err}
	}
}

func (m *Model) submitWorkforceApply() tea.Cmd {
	reason := strings.TrimSpace(m.editor.Value())
	if !m.canApplyWorkforce() || m.busy || reason == "" {
		if reason == "" {
			m.status = "Record why this reviewed workforce should be created."
		}
		return nil
	}
	changeSet := m.authoringChangeSet
	intent := fmt.Sprintf("apply\x00%s\x00%d\x00%s\x00%s", changeSet.ID, changeSet.Revision, changeSet.CandidateDigest, reason)
	if m.pendingGovernanceKey == "" || m.pendingGovernanceIntent != intent {
		m.pendingGovernanceKey, m.pendingGovernanceIntent = uuid.NewString(), intent
	}
	request := authoring.ApplyChangeSetRequest{
		Scope: changeSet.Scope, ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision,
		CandidateDigest: changeSet.CandidateDigest, Reason: reason,
		Actor: authoring.ChangeSetActor{Type: m.config.Actor.Type, ID: m.config.Actor.ID},
	}
	key := m.pendingGovernanceKey
	m.busy, m.err = true, nil
	m.status = "Creating the reviewed workforce atomically…"
	return func() tea.Msg {
		result, err := m.client.ApplyWorkforceChangeSet(m.ctx, request, key)
		return workforceGoverned{changeSet: result, action: "Workforce Apply", err: err}
	}
}

func (m *Model) submitWorkforceRetry() tea.Cmd {
	reason := strings.TrimSpace(m.editor.Value())
	if !m.canRetryWorkforce() || m.busy || reason == "" {
		if reason == "" {
			m.status = "Record why generation should be retried."
		}
		return nil
	}
	changeSet := m.authoringChangeSet
	intent := fmt.Sprintf("retry\x00%s\x00%d\x00%s", changeSet.ID, changeSet.Revision, reason)
	if m.pendingGovernanceKey == "" || m.pendingGovernanceIntent != intent {
		m.pendingGovernanceKey, m.pendingGovernanceIntent = uuid.NewString(), intent
	}
	request := authoring.RetryChangeSetGenerationRequest{Scope: changeSet.Scope, ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision, Reason: reason}
	key := m.pendingGovernanceKey
	m.busy, m.err, m.status = true, nil, "Retrying generation as durable work…"
	return func() tea.Msg {
		result, err := m.client.RetryWorkforceChangeSetGeneration(m.ctx, request, key)
		return workforceGoverned{changeSet: result, action: "Generation retry", err: err}
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
	return func() tea.Msg {
		approvals, err := m.client.ListActionApprovals(m.ctx, runtime.ApprovalFilter{
			Scope: m.config.Scope, Owner: &owner, Limit: 100,
		})
		return actionApprovalsLoaded{approvals: approvals, err: err}
	}
}

func (m *Model) loadCompilations() tea.Cmd {
	if !m.supportsAgentDefinition(kernelapi.OperationListCompilations) || m.config.Owner.Type != runtime.OwnerTypeAgent {
		return nil
	}
	m.loading = true
	return func() tea.Msg {
		values, err := m.client.ListAgentDefinitionCompilations(m.ctx, capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID}, m.config.Owner.ID)
		return compilationsLoaded{compilations: values, err: err}
	}
}

func (m *Model) loadObjectives() tea.Cmd {
	if !m.supportsObjective(kernelapi.OperationList) {
		return nil
	}
	m.loading = true
	return func() tea.Msg {
		objectives, err := m.client.ListObjectives(m.ctx, runtime.ObjectiveFilter{Scope: m.config.Scope, Owner: &m.config.Owner, Limit: 100})
		return objectivesLoaded{objectives: objectives, err: err}
	}
}

func (m *Model) loadInitiatives() tea.Cmd {
	if !m.supportsInitiative(kernelapi.OperationList) {
		return nil
	}
	m.loading = true
	return func() tea.Msg {
		initiatives, err := m.client.ListInitiatives(m.ctx, runtime.InitiativeFilter{Scope: m.config.Scope, Owner: &m.config.Owner, Limit: 100})
		return initiativesLoaded{initiatives, err}
	}
}

func (m *Model) loadSourceMonitors() tea.Cmd {
	if !m.supportsSourceMonitor(kernelapi.OperationGetCheckpoint) || !m.supportsSourceMonitor(kernelapi.OperationListObservations) {
		return nil
	}
	initiatives := append([]*runtime.Initiative(nil), m.initiatives...)
	return func() tea.Msg {
		statuses := make(map[string]sourceMonitorStatus)
		for _, initiative := range initiatives {
			if initiative == nil {
				continue
			}
			for _, monitor := range initiative.SourceMonitors {
				key := sourceMonitorStatusKey(initiative.ID, monitor.ID)
				checkpoint, checkpointErr := m.client.GetSourceMonitorCheckpoint(m.ctx, initiative.Scope, initiative.ID, monitor.ID)
				observations, observationsErr := m.client.ListSourceObservations(m.ctx, runtime.SourceObservationFilter{
					Scope: initiative.Scope, InitiativeID: initiative.ID, MonitorID: monitor.ID, Limit: 5,
				})
				status := sourceMonitorStatus{checkpoint: checkpoint, observations: observations}
				if checkpointErr != nil && !errors.Is(checkpointErr, runtime.ErrSourceObservationNotFound) {
					status.err = checkpointErr
				}
				if observationsErr != nil {
					status.err = observationsErr
				}
				statuses[key] = status
			}
		}
		return sourceMonitorsLoaded{statuses: statuses}
	}
}

func sourceMonitorStatusKey(initiativeID, monitorID string) string {
	return initiativeID + "\x00" + monitorID
}

func (m *Model) loadClawHubSkills() tea.Cmd {
	if !m.supportsClawHub(clawhub.LifecycleInspectInstalled) {
		return nil
	}
	m.loading = true
	return func() tea.Msg {
		skills, err := m.clawHubClient.ListInstalledClawHubSkills(m.ctx)
		return clawHubSkillsLoaded{skills, err}
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
	return func() tea.Msg {
		conversations, err := m.conversationClient.ListConversations(m.ctx, runtime.ConversationFilter{
			Scope: m.config.Scope, Owner: &m.config.Owner, Limit: 100,
		})
		return conversationsLoaded{conversations: conversations, err: err}
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
	if m.section == sectionAuthoring && m.authoringChangeSet != nil {
		return m.loadWorkforceChangeSet()
	}
	if m.section == sectionReadiness {
		return m.loadCompilations()
	}
	if m.section == sectionObjectives {
		return m.loadObjectives()
	}
	if m.section == sectionInitiatives {
		return m.loadInitiatives()
	}
	if m.section == sectionSkills {
		return m.loadClawHubSkills()
	}
	if m.section == sectionRequests {
		return m.loadAgentRequests()
	}
	if m.section == sectionApprovals {
		return m.loadActionApprovals()
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
	if len(directive) == 2 && strings.EqualFold(directive[0], string(runtime.AgentRequestKindHandoff)) {
		kind = runtime.AgentRequestKindHandoff
		directive = directive[1:]
	}
	if len(directive) != 1 {
		return "", runtime.CollaborationParty{}, "", errors.New("Use agent:<id>, team:<id>, or handoff agent:<id> on the first line.")
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
		ExpectedRevision: approval.Revision, DecisionID: key, Approve: approve,
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

func (m *Model) submitInitiative() tea.Cmd {
	prompt := strings.TrimSpace(m.editor.Value())
	objective := m.selectedObjectiveRecord()
	if !m.supportsInitiative(kernelapi.OperationCreate) || m.busy || prompt == "" {
		if prompt == "" {
			m.status = "Describe the Initiative before adding it."
		}
		return nil
	}
	if objective == nil {
		m.status = "Select or create an Objective before composing an Initiative."
		return nil
	}
	if m.pendingInitiativeKey == "" || m.pendingInitiativePrompt != prompt {
		m.pendingInitiativeKey, m.pendingInitiativePrompt = uuid.NewString(), prompt
	}
	m.busy, m.err, m.status = true, nil, "Adding Initiative to the durable portfolio…"
	request := kernelapi.CreateInitiativeRequest{Scope: m.config.Scope, Owner: m.config.Owner, Title: objectiveTitle(prompt), Purpose: prompt, Status: runtime.InitiativeStatusActive, ObjectiveRefs: []string{objective.ID}}
	key := m.pendingInitiativeKey
	return func() tea.Msg {
		initiative, err := m.client.CreateInitiative(m.ctx, request, key)
		return initiativeCreated{initiative, err}
	}
}

func (m *Model) submitInitiativeAmendment() tea.Cmd {
	initiative := m.selectedInitiativeRecord()
	purpose := strings.TrimSpace(m.editor.Value())
	if initiative == nil || !m.supportsInitiative(kernelapi.OperationPatch) || m.busy || purpose == "" {
		if purpose == "" {
			m.status = "Describe the amended Initiative purpose."
		}
		return nil
	}
	m.busy, m.err, m.status = true, nil, "Recording Initiative revision…"
	request := kernelapi.UpdateInitiativeRequest{ExpectedRevision: initiative.Revision, Purpose: &purpose}
	return func() tea.Msg {
		updated, err := m.client.PatchInitiative(m.ctx, m.config.Scope, initiative.ID, request)
		return initiativeUpdated{updated, err}
	}
}

func (m *Model) pauseOrResumeInitiative() tea.Cmd {
	initiative := m.selectedInitiativeRecord()
	if initiative == nil || m.busy || !m.supportsInitiative(kernelapi.OperationPatch) {
		return nil
	}
	status := runtime.InitiativeStatusPaused
	if initiative.Status == runtime.InitiativeStatusPaused {
		status = runtime.InitiativeStatusActive
	}
	m.busy, m.err, m.status = true, nil, "Updating Initiative lifecycle…"
	request := kernelapi.UpdateInitiativeRequest{ExpectedRevision: initiative.Revision, Status: &status}
	return func() tea.Msg {
		updated, err := m.client.PatchInitiative(m.ctx, m.config.Scope, initiative.ID, request)
		return initiativeUpdated{updated, err}
	}
}

func (m *Model) toggleSelectedObjectiveLink() tea.Cmd {
	initiative, objective := m.selectedInitiativeRecord(), m.selectedObjectiveRecord()
	if initiative == nil || objective == nil || m.busy || !m.supportsInitiative(kernelapi.OperationPatch) {
		if objective == nil {
			m.status = "Select an Objective before linking it to this Initiative."
		}
		return nil
	}
	refs, found := append([]string(nil), initiative.ObjectiveRefs...), -1
	for index, id := range refs {
		if id == objective.ID {
			found = index
			break
		}
	}
	if found >= 0 {
		if len(refs) == 1 {
			m.status = "An Initiative must retain at least one Objective."
			return nil
		}
		refs = append(refs[:found], refs[found+1:]...)
		m.status = "Removing Objective from Initiative…"
	} else {
		refs = append(refs, objective.ID)
		m.status = "Linking Objective to Initiative…"
	}
	m.busy, m.err = true, nil
	request := kernelapi.UpdateInitiativeRequest{ExpectedRevision: initiative.Revision, ObjectiveRefs: &refs}
	return func() tea.Msg {
		updated, err := m.client.PatchInitiative(m.ctx, m.config.Scope, initiative.ID, request)
		return initiativeUpdated{updated, err}
	}
}

func (m *Model) submitClawHubInstall() tea.Cmd {
	prompt := strings.TrimSpace(m.editor.Value())
	if prompt == "" || m.busy || !m.supportsClawHub(clawhub.LifecycleInstall) {
		if prompt == "" {
			m.status = "Enter an owner-qualified ClawHub reference."
		}
		return nil
	}
	reference, err := clawhub.ParseSkillReference(prompt)
	if err != nil {
		m.status = "Use a valid ClawHub reference such as @owner/skill."
		return nil
	}
	m.busy = true
	m.err = nil
	m.pendingClawHubPrompt = prompt
	m.status = "Verifying, compiling, and installing Skill…"
	return func() tea.Msg {
		result, installErr := m.clawHubClient.InstallClawHubSkill(m.ctx, reference, kernelapi.ClawHubVersionRequest{})
		return clawHubLifecycleCompleted{result: result, operation: clawhub.LifecycleInstall, err: installErr}
	}
}
func (m *Model) submitClawHubPin() tea.Cmd {
	skill := m.selectedClawHubRecord()
	reason := strings.TrimSpace(m.editor.Value())
	if skill == nil || reason == "" || m.busy || !m.supportsClawHub(clawhub.LifecyclePin) {
		if reason == "" {
			m.status = "Record why this exact version must stay fixed."
		}
		return nil
	}
	m.busy = true
	m.err = nil
	m.pendingClawHubPrompt = reason
	reference := skill.Reference.String()
	return func() tea.Msg {
		result, err := m.clawHubClient.PinClawHubSkill(m.ctx, reference, reason)
		return clawHubLifecycleCompleted{result: result, operation: clawhub.LifecyclePin, err: err}
	}
}
func (m *Model) submitClawHubRemoval() tea.Cmd {
	skill := m.selectedClawHubRecord()
	if skill == nil || strings.TrimSpace(m.editor.Value()) != "REMOVE" || m.busy || !m.supportsClawHub(clawhub.LifecycleUninstall) {
		m.status = "Type REMOVE exactly to confirm this governed uninstall."
		return nil
	}
	m.busy = true
	m.err = nil
	reference := skill.Reference.String()
	return func() tea.Msg {
		result, err := m.clawHubClient.UninstallClawHubSkill(m.ctx, reference)
		return clawHubLifecycleCompleted{result: result, operation: clawhub.LifecycleUninstall, err: err}
	}
}
func (m *Model) pinOrUnpinClawHub() tea.Cmd {
	skill := m.selectedClawHubRecord()
	if skill == nil {
		return nil
	}
	if skill.Pinned && m.supportsClawHub(clawhub.LifecycleUnpin) {
		m.busy = true
		reference := skill.Reference.String()
		return func() tea.Msg {
			result, err := m.clawHubClient.UnpinClawHubSkill(m.ctx, reference)
			return clawHubLifecycleCompleted{result: result, operation: clawhub.LifecycleUnpin, err: err}
		}
	}
	if !skill.Pinned && m.supportsClawHub(clawhub.LifecyclePin) {
		m.mode = modeSkillPin
		m.editor.Reset()
		m.editor.Placeholder = "Why must this version stay fixed?…"
		m.focusComposerEditor()
	}
	return nil
}
func (m *Model) updateSelectedClawHub() tea.Cmd {
	skill := m.selectedClawHubRecord()
	if skill == nil || skill.Pinned || skill.LocallyModified || m.busy || !m.supportsClawHub(clawhub.LifecycleUpdate) {
		return nil
	}
	m.busy = true
	reference := skill.Reference.String()
	return func() tea.Msg {
		result, err := m.clawHubClient.UpdateClawHubSkill(m.ctx, reference)
		return clawHubLifecycleCompleted{result: result, operation: clawhub.LifecycleUpdate, err: err}
	}
}
func (m *Model) updateAllClawHub() tea.Cmd {
	if m.busy || !m.supportsClawHub(clawhub.LifecycleUpdateAll) {
		return nil
	}
	m.busy = true
	return func() tea.Msg {
		result, err := m.clawHubClient.UpdateAllClawHubSkills(m.ctx)
		return clawHubLifecycleCompleted{batch: result, operation: clawhub.LifecycleUpdateAll, err: err}
	}
}
func (m *Model) verifySelectedClawHub() tea.Cmd {
	skill := m.selectedClawHubRecord()
	if skill == nil || m.busy || !m.supportsClawHub(clawhub.LifecycleVerifyInstalled) {
		return nil
	}
	m.busy = true
	reference := skill.Reference.String()
	return func() tea.Msg {
		_, err := m.clawHubClient.VerifyInstalledClawHubSkill(m.ctx, reference)
		result := &clawhub.LifecycleResult{APIVersion: clawhub.LifecycleAPIVersion, Operation: clawhub.LifecycleVerifyInstalled, SourceIdentity: skill.SourceIdentity, Reference: skill.Reference, Version: skill.Version, Outcome: clawhub.LifecycleOutcomeVerified}
		return clawHubLifecycleCompleted{result: result, operation: clawhub.LifecycleVerifyInstalled, err: err}
	}
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

func (m *Model) supportsAgentRequest(operation string) bool {
	return m.ready && m.requestCapability.Supports(operation)
}

func (m *Model) supportsActionApproval(operation string) bool {
	return m.ready && m.approvalCapability.Supports(operation)
}

func (m *Model) supportsAgentDefinition(operation string) bool {
	return m.ready && m.agentDefinitionCapability.Supports(operation)
}

func (m *Model) supportsObjective(operation string) bool {
	return m.ready && m.objectiveCapability.Supports(operation)
}

func (m *Model) supportsInitiative(operation string) bool {
	return m.ready && m.initiativeCapability.Supports(operation)
}

func (m *Model) supportsSourceMonitor(operation string) bool {
	return m.ready && m.sourceMonitorCapability.Supports(operation)
}

func (m *Model) supportsClawHub(operation clawhub.LifecycleOperation) bool {
	return m.ready && m.clawHubClient != nil && m.clawHubCapability.Supports(string(operation))
}

func (m *Model) supportsArtifact(operation string) bool {
	return m.ready && m.artifactCapability.Supports(operation)
}

func (m *Model) supportsChannel(operation string) bool {
	return m.ready && m.conversationClient != nil && m.channelCapability.Supports(operation)
}

func (m *Model) supportsAuthoring(operation string) bool {
	return m.ready && m.authoringCapability.Supports(operation)
}

func (m *Model) supportsWorkforceAuthoring() bool {
	return m.supportsAuthoring(kernelapi.OperationPropose) || m.supportsAuthoring(kernelapi.OperationCompile)
}

func (m *Model) selectedWorkforceApprovalRequirement() (kernelapi.ApprovalRequirementReference, bool) {
	if !m.canResolveWorkforceApproval() {
		return kernelapi.ApprovalRequirementReference{}, false
	}
	return m.authoringCapability.Context.EligibleApprovalRequirements[m.authoringApprovalSelected], true
}

func (m *Model) moveWorkforceApprovalSelection(delta int) {
	if m.authoringCapability.Context == nil {
		return
	}
	count := len(m.authoringCapability.Context.EligibleApprovalRequirements)
	if count == 0 {
		m.authoringApprovalSelected = 0
		return
	}
	m.authoringApprovalSelected = (m.authoringApprovalSelected + delta + count) % count
}

func (m *Model) canResolveWorkforceApproval() bool {
	return m.authoringChangeSet != nil && m.authoringChangeSet.Status == authoring.ChangeSetAwaitingApproval &&
		m.authoringCapability.Context != nil && m.authoringCapability.Context.ChangeSetID == m.authoringChangeSet.ID &&
		m.authoringCapability.Context.Revision == m.authoringChangeSet.Revision && m.supportsAuthoring(kernelapi.OperationApprove) &&
		len(m.authoringCapability.Context.EligibleApprovalRequirements) > 0
}

func (m *Model) canApplyWorkforce() bool {
	return m.authoringChangeSet != nil && m.authoringChangeSet.Status == authoring.ChangeSetReady &&
		m.authoringCapability.Context != nil && m.authoringCapability.Context.ChangeSetID == m.authoringChangeSet.ID &&
		m.authoringCapability.Context.Revision == m.authoringChangeSet.Revision && m.supportsAuthoring(kernelapi.OperationApply)
}

func (m *Model) canRetryWorkforce() bool {
	return m.authoringChangeSet != nil && m.authoringChangeSet.Status == authoring.ChangeSetFailed &&
		m.authoringCapability.Context != nil && m.authoringCapability.Context.ChangeSetID == m.authoringChangeSet.ID &&
		m.authoringCapability.Context.Revision == m.authoringChangeSet.Revision && m.supportsAuthoring(kernelapi.OperationRetry)
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

func (m *Model) movePanelSelection(delta int) {
	if m.section == sectionObjectives {
		m.moveObjectiveSelection(delta)
		return
	}
	if m.section == sectionInitiatives {
		m.moveInitiativeSelection(delta)
		return
	}
	if m.section == sectionSkills {
		m.moveClawHubSelection(delta)
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

func (m *Model) selectedInitiativeRecord() *runtime.Initiative {
	if m.initiativeSelected < 0 || m.initiativeSelected >= len(m.initiatives) {
		return nil
	}
	return m.initiatives[m.initiativeSelected]
}

func (m *Model) selectedClawHubRecord() *clawhub.InstalledState {
	if m.clawHubSelected < 0 || m.clawHubSelected >= len(m.clawHubSkills) {
		return nil
	}
	return &m.clawHubSkills[m.clawHubSelected]
}
func (m *Model) restoreClawHubSelection() {
	if len(m.clawHubSkills) == 0 {
		m.clawHubSelected = 0
		m.selectedClawHub = ""
		return
	}
	for index := range m.clawHubSkills {
		if m.clawHubSkills[index].SourceIdentity == m.selectedClawHub {
			m.clawHubSelected = index
			return
		}
	}
	m.clawHubSelected = min(m.clawHubSelected, len(m.clawHubSkills)-1)
	m.selectedClawHub = m.clawHubSkills[m.clawHubSelected].SourceIdentity
}
func (m *Model) moveClawHubSelection(delta int) {
	if len(m.clawHubSkills) == 0 {
		return
	}
	m.clawHubSelected = max(0, min(len(m.clawHubSkills)-1, m.clawHubSelected+delta))
	m.selectedClawHub = m.clawHubSkills[m.clawHubSelected].SourceIdentity
}
func (m *Model) restoreInitiativeSelection() {
	if len(m.initiatives) == 0 {
		m.initiativeSelected = 0
		m.selectedInitiative = ""
		return
	}
	for i, v := range m.initiatives {
		if v.ID == m.selectedInitiative {
			m.initiativeSelected = i
			return
		}
	}
	m.initiativeSelected = min(m.initiativeSelected, len(m.initiatives)-1)
	m.selectedInitiative = m.initiatives[m.initiativeSelected].ID
}
func (m *Model) moveInitiativeSelection(delta int) {
	if len(m.initiatives) == 0 {
		return
	}
	m.initiativeSelected = max(0, min(len(m.initiatives)-1, m.initiativeSelected+delta))
	m.selectedInitiative = m.initiatives[m.initiativeSelected].ID
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
	if artifact == nil || m.busy || !m.supportsArtifact(kernelapi.OperationDownload) {
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

func (m *Model) prepareWorkforceGovernanceComposer(mode editorMode, placeholder string) {
	m.mode = mode
	m.editor.Reset()
	m.editor.Placeholder = placeholder
	m.focusComposerEditor()
}

func (m *Model) prepareRequestComposer(mode editorMode, placeholder string) {
	m.mode = mode
	m.editor.Reset()
	m.editor.Placeholder = placeholder
	m.focusComposerEditor()
}

func (m *Model) prepareComposerForSection() {
	switch {
	case m.section == sectionAuthoring && m.supportsWorkforceAuthoring():
		m.mode = modeWorkforceAuthoring
		m.editor.Placeholder = "Describe the Agents and Team you need…"
		m.focusComposerEditor()
	case m.section == sectionObjectives && m.supportsObjective(kernelapi.OperationCreate):
		m.mode = modeObjectiveCreate
		m.editor.Placeholder = "Describe the objective and desired outcome…"
		m.focusComposerEditor()
	case m.section == sectionInitiatives && m.supportsInitiative(kernelapi.OperationCreate):
		m.mode = modeInitiativeCreate
		m.editor.Placeholder = "Describe the Initiative outcome…"
		m.focusComposerEditor()
	case m.section == sectionSkills && m.supportsClawHub(clawhub.LifecycleInstall):
		m.mode = modeSkillInstall
		m.editor.Placeholder = "Enter @owner/skill to install…"
		m.focusComposerEditor()
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
	case m.section == sectionRequests && m.supportsAgentRequest(kernelapi.OperationCreate) && m.selectedRun() != nil:
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
	if m.section == sectionAuthoring {
		m.mode = modeWorkforceAuthoring
		m.editor.Placeholder = "Describe the Agents and Team you need…"
		return
	}
	if m.section == sectionObjectives {
		m.mode = modeObjectiveCreate
		m.editor.Placeholder = "Describe the objective and desired outcome…"
		return
	}
	if m.section == sectionInitiatives {
		m.mode = modeInitiativeCreate
		m.editor.Placeholder = "Describe the Initiative outcome…"
		return
	}
	if m.section == sectionSkills {
		m.mode = modeSkillInstall
		m.editor.Placeholder = "Enter @owner/skill to install…"
		return
	}
	if m.section == sectionChannels && m.selectedConversationRecord() != nil {
		m.mode = modeChannelPost
		m.editor.Placeholder = "Share an update or ask a question…"
		return
	}
	if m.section == sectionRequests {
		if m.supportsAgentRequest(kernelapi.OperationCreate) && m.selectedRun() != nil {
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

func clawHubClient(kernelClient client.KernelClient) client.ClawHubClient {
	value, _ := kernelClient.(client.ClawHubClient)
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
