// Package tui implements OpenSeal's prompt-first terminal client. It owns no
// durable state: every action is discovered from and sent to the public kernel
// API used by other embedding surfaces.
package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
)

type Model struct {
	ctx                       context.Context
	client                    client.KernelClient
	conversationClient        client.ConversationClient
	clawHubClient             client.ClawHubClient
	config                    Config
	editor                    textarea.Model
	focus                     focusArea
	section                   panelSection
	mode                      editorMode
	width                     int
	height                    int
	loading                   bool
	busy                      bool
	ready                     bool
	unavailable               string
	err                       error
	status                    string
	runCapability             kernelapi.Capability
	objectiveCapability       kernelapi.Capability
	initiativeCapability      kernelapi.Capability
	sourceMonitorCapability   kernelapi.Capability
	clawHubCapability         kernelapi.Capability
	artifactCapability        kernelapi.Capability
	channelCapability         kernelapi.Capability
	authoringCapability       kernelapi.Capability
	agentDefinitionCapability kernelapi.Capability
	authoringResult           *authoring.CompileResult
	authoringChangeSet        *authoring.ChangeSet
	authoringAmendment        bool
	authoringApprovalSelected int
	runs                      []*runtime.AgentRun
	compilations              []*kernelagent.DefinitionCompilation
	objectives                []*runtime.Objective
	objectiveSelected         int
	selectedObjective         string
	initiatives               []*runtime.Initiative
	initiativeSelected        int
	selectedInitiative        string
	sourceMonitorStatuses     map[string]sourceMonitorStatus
	clawHubSkills             []clawhub.InstalledState
	clawHubSelected           int
	selectedClawHub           string
	selected                  int
	selectedID                string
	artifacts                 []*runtime.Artifact
	artifactSelected          int
	selectedArtifact          string
	artifactExpanded          bool
	conversations             []*runtime.Conversation
	conversationSelected      int
	selectedConversation      string
	channelMessages           []*runtime.ChannelMessage
	channelRounds             []*runtime.ParticipationRoundResult
	channelPresence           []*runtime.ConversationPresence
	channelAuditExpanded      bool
	pendingKey                string
	pendingGoal               string
	pendingAuthoringKey       string
	pendingAuthoringPrompt    string
	pendingAuthoringParentID  string
	pendingGovernanceKey      string
	pendingGovernanceIntent   string
	pendingObjectiveKey       string
	pendingObjectivePrompt    string
	pendingInitiativeKey      string
	pendingInitiativePrompt   string
	pendingClawHubPrompt      string
	pendingConversationKey    string
	pendingConversationTitle  string
	pendingMessageKey         string
	pendingMessageContent     string
	pendingMessageChannelID   string
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
		objectiveCapability, hasObjectives := msg.document.Find(kernelapi.ObjectivesCapabilityID, kernelapi.ObjectivesCapabilityVersion)
		initiativeCapability, hasInitiatives := msg.document.Find(kernelapi.InitiativesCapabilityID, kernelapi.InitiativesCapabilityVersion)
		sourceMonitorCapability, _ := msg.document.Find(kernelapi.SourceMonitorsCapabilityID, kernelapi.SourceMonitorsCapabilityVersion)
		clawHubCapability, hasClawHub := msg.document.Find(kernelapi.ClawHubLifecycleCapabilityID, kernelapi.ClawHubLifecycleCapabilityVersion)
		artifactCapability, hasArtifacts := msg.document.Find(kernelapi.ArtifactsCapabilityID, kernelapi.ArtifactsCapabilityVersion)
		channelCapability, hasChannels := msg.document.Find(kernelapi.ChannelsCapabilityID, kernelapi.ChannelsCapabilityVersion)
		authoringCapability, hasAuthoring := msg.document.Find(kernelapi.WorkforceAuthoringCapabilityID, kernelapi.WorkforceAuthoringCapabilityVersion)
		agentDefinitionCapability, hasAgentDefinitions := msg.document.Find(kernelapi.AgentDefinitionsCapabilityID, kernelapi.AgentDefinitionsCapabilityVersion)
		m.runCapability = runCapability
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
		if !m.objectiveCapability.Available && !m.initiativeCapability.Available && !m.clawHubCapability.Available && !m.runCapability.Available && !m.artifactCapability.Available && !m.channelCapability.Available && !m.authoringCapability.Available && !m.agentDefinitionCapability.Available {
			m.unavailable = "This server does not advertise workforce authoring, objectives, Initiatives, canonical work, Team channels, or artifact evidence."
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
		return m, tea.Batch(m.loadCompilations(), m.loadObjectives(), m.loadInitiatives(), m.loadClawHubSkills(), m.loadRuns(), m.loadArtifacts(), m.loadConversations())
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
			commands = append(commands, m.loadCompilations(), m.loadObjectives(), m.loadInitiatives(), m.loadClawHubSkills(), m.loadRuns(), m.loadArtifacts(), m.loadConversations())
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
			if m.section == sectionChannels && m.supportsChannel(kernelapi.OperationCreate) {
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
	return func() tea.Msg {
		runs, err := m.client.ListAgentRuns(m.ctx, runtime.AgentRunFilter{
			Scope: m.config.Scope, Owner: &m.config.Owner, Limit: 100,
		})
		return runsLoaded{runs: runs, err: err}
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
	if m.section == sectionChannels {
		return m.loadConversations()
	}
	if m.section == sectionArtifacts {
		return m.loadArtifacts()
	}
	return m.loadRuns()
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
	m.mode = modeCreate
	m.editor.Placeholder = "Describe the outcome you want…"
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
