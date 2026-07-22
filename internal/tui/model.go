// Package tui implements OpenSeal's prompt-first terminal client. It owns no
// durable state: every action is discovered from and sent to the public kernel
// API used by other embedding surfaces.
package tui

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	"github.com/axiom-studio/openseal/pkg/source"
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
	sectionTeams
	sectionObjectives
	sectionInitiatives
	sectionOutreach
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
	modeWorkforceAuthoring
	modeWorkforceRefinement
	modeGuide
	modeChannelCreate
	modeChannelPost
	modeObjectiveCreate
	modeObjectiveEdit
	modeInitiativeCreate
	modeInitiativeEdit
	modeOutreachCreate
	modeSkillInstall
	modeSkillPin
	modeSkillRemove
	modeSkillBindingUpsert
	modeSkillBindingDisable
	modeSourcePolicyRegister
	modeSourcePolicyActivate
	modeSourcePolicyRevoke
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
	ctx                         context.Context
	client                      client.KernelClient
	agentCapabilityClient       client.AgentDefinitionCapabilityClient
	agentLifecycleClient        client.AgentDefinitionLifecycleClient
	conversationClient          client.ConversationClient
	clawHubClient               client.ClawHubClient
	skillBindingClient          client.SkillBindingClient
	sourcePolicyClient          client.SourcePolicyLifecycleClient
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
	outreachCapability          kernelapi.Capability
	sourceMonitorCapability     kernelapi.Capability
	activityCapability          kernelapi.Capability
	clawHubCapability           kernelapi.Capability
	skillActionCapability       kernelapi.Capability
	skillBindingCapability      kernelapi.Capability
	artifactCapability          kernelapi.Capability
	channelCapability           kernelapi.Capability
	authoringCapability         kernelapi.Capability
	agentDefinitionCapability   kernelapi.Capability
	teamDefinitionCapability    kernelapi.Capability
	sourcePolicyCapability      kernelapi.Capability
	authoringResult             *authoring.CompileResult
	authoringChangeSet          *authoring.ChangeSet
	authoringAmendment          bool
	authoringApprovalSelected   int
	authoringCredentialSelected int
	authoringCredentialChoices  map[string]int
	authoringConfigSelected     int
	authoringConfigChoices      map[string]int
	runs                        []*runtime.AgentRun
	evidenceExpanded            bool
	evidenceObservationSelected int
	groundingExpanded           bool
	groundingPageSelected       int
	agentRequests               []*runtime.AgentRequest
	agentRequestSelected        int
	selectedAgentRequest        string
	actionApprovals             []*runtime.ApprovalCheckpoint
	actionApprovalSelected      int
	selectedActionApproval      string
	activity                    []runtime.ActivityProjection
	activitySelected            int
	selectedActivity            string
	activityExpanded            bool
	activityNextCursor          string
	activityHasMore             bool
	compilations                []*kernelagent.DefinitionCompilation
	agentDeployment             *kernelapi.AgentDeploymentCatalogEntry
	agentAmendments             []*kernelagent.DefinitionAmendment
	agentAmendmentSelected      int
	selectedAgentAmendment      string
	teamDeployments             []kernelapi.TeamDeploymentCatalogEntry
	teamDeploymentSelected      int
	selectedTeamDeployment      string
	teamAmendments              []*kernelteam.DefinitionAmendment
	teamAmendmentSelected       int
	selectedTeamAmendment       string
	objectives                  []*runtime.Objective
	objectiveSelected           int
	selectedObjective           string
	initiatives                 []*runtime.Initiative
	initiativeSelected          int
	selectedInitiative          string
	outreachThreads             []*runtime.OutreachThread
	outreachSelected            int
	selectedOutreach            string
	outreachObservations        []*runtime.SourceObservation
	outreachObservationSelected int
	outreachActions             []capability.ModelAction
	outreachActionSelected      int
	sourceMonitorStatuses       map[string]sourceMonitorStatus
	initiativeActivity          map[string][]runtime.ActivityProjection
	initiativeActivityErrors    map[string]error
	clawHubSkills               []clawhub.InstalledState
	skillActions                []capability.ModelAction
	skillBindings               []*capability.Binding
	sourcePolicies              []*source.Lifecycle
	skillBindingSelected        int
	selectedSkillBinding        string
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
	pendingRefinementKey        string
	pendingRefinementIntent     string
	activeRefinementQuestionID  string
	pendingObjectiveKey         string
	pendingObjectivePrompt      string
	pendingInitiativeKey        string
	pendingInitiativePrompt     string
	pendingOutreachKey          string
	pendingOutreachPrompt       string
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
	checkpoint         *runtime.SourceMonitorCheckpoint
	observations       []*runtime.SourceObservation
	policyDecision     *source.PolicyDecision
	policyAuthorizedAt time.Time
	policyErr          error
	err                error
}

type sourceMonitorsLoaded struct {
	statuses       map[string]sourceMonitorStatus
	activity       map[string][]runtime.ActivityProjection
	activityErrors map[string]error
}
type initiativeCreated struct {
	initiative *runtime.Initiative
	err        error
}
type initiativeUpdated struct {
	initiative *runtime.Initiative
	err        error
}

type outreachLoaded struct {
	initiativeID string
	threads      []*runtime.OutreachThread
	observations []*runtime.SourceObservation
	actions      []capability.ModelAction
	err          error
}

type outreachCreated struct {
	thread *runtime.OutreachThread
	err    error
}

type outreachDeliveryCreated struct {
	run      *runtime.AgentRun
	threadID string
	err      error
}

type outreachRunLoaded struct {
	run *runtime.AgentRun
	err error
}

type clawHubSkillsLoaded struct {
	skills []clawhub.InstalledState
	err    error
}
type skillActionsLoaded struct {
	actions []capability.ModelAction
	err     error
}
type skillBindingsLoaded struct {
	bindings []*capability.Binding
	err      error
}

type sourcePoliciesLoaded struct {
	policies []*source.Lifecycle
	err      error
}
type sourcePolicyChanged struct {
	action  string
	version *source.PolicyVersion
	result  *source.LifecycleResult
	err     error
}
type skillBindingChanged struct {
	binding *capability.Binding
	action  string
	err     error
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
		agentLifecycleClient:  agentLifecycleClient(kernelClient),
		agentCapabilityClient: agentCapabilityClient(kernelClient),
		conversationClient:    conversationClient(kernelClient),
		clawHubClient:         clawHubClient(kernelClient),
		skillBindingClient:    skillBindingClient(kernelClient),
		sourcePolicyClient:    sourcePolicyClient(kernelClient),
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
		outreachCapability, hasOutreach := msg.document.Find(kernelapi.OutreachCapabilityID, kernelapi.OutreachCapabilityVersion)
		sourceMonitorCapability, _ := msg.document.Find(kernelapi.SourceMonitorsCapabilityID, kernelapi.SourceMonitorsCapabilityVersion)
		activityCapability, hasActivity := msg.document.Find(kernelapi.ActivityCapabilityID, kernelapi.ActivityCapabilityVersion)
		clawHubCapability, hasClawHub := msg.document.Find(kernelapi.ClawHubLifecycleCapabilityID, kernelapi.ClawHubLifecycleCapabilityVersion)
		skillActionCapability, hasSkillActions := msg.document.Find(kernelapi.SkillActionsCapabilityID, kernelapi.SkillActionsCapabilityVersion)
		skillBindingCapability, hasSkillBindings := msg.document.Find(kernelapi.SkillBindingsCapabilityID, kernelapi.SkillBindingsCapabilityVersion)
		artifactCapability, hasArtifacts := msg.document.Find(kernelapi.ArtifactsCapabilityID, kernelapi.ArtifactsCapabilityVersion)
		channelCapability, hasChannels := msg.document.Find(kernelapi.ChannelsCapabilityID, kernelapi.ChannelsCapabilityVersion)
		authoringCapability, hasAuthoring := msg.document.Find(kernelapi.WorkforceAuthoringCapabilityID, kernelapi.WorkforceAuthoringCapabilityVersion)
		agentDefinitionCapability, hasAgentDefinitions := msg.document.Find(kernelapi.AgentDefinitionsCapabilityID, kernelapi.AgentDefinitionsCapabilityVersion)
		teamDefinitionCapability, hasTeamDefinitions := msg.document.Find(kernelapi.TeamDefinitionsCapabilityID, kernelapi.TeamDefinitionsCapabilityVersion)
		sourcePolicyCapability, hasSourcePolicies := msg.document.Find(kernelapi.SourcePoliciesCapabilityID, kernelapi.SourcePoliciesCapabilityVersion)
		m.runCapability = runCapability
		m.requestCapability = requestCapability
		m.approvalCapability = approvalCapability
		m.objectiveCapability = objectiveCapability
		m.initiativeCapability = initiativeCapability
		m.outreachCapability = outreachCapability
		m.sourceMonitorCapability = sourceMonitorCapability
		m.activityCapability = activityCapability
		m.clawHubCapability = clawHubCapability
		m.skillActionCapability = skillActionCapability
		m.skillBindingCapability = skillBindingCapability
		m.artifactCapability = artifactCapability
		m.channelCapability = channelCapability
		m.authoringCapability = authoringCapability
		m.agentDefinitionCapability = agentDefinitionCapability
		m.teamDefinitionCapability = teamDefinitionCapability
		m.sourcePolicyCapability = sourcePolicyCapability
		m.syncWorkforceCredentialChoices()
		m.syncWorkforceBindingConfigurationChoices()
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
		if !hasOutreach || !outreachCapability.Available || !outreachCapability.Supports(kernelapi.OperationList) || !m.initiativeCapability.Available {
			m.outreachCapability = kernelapi.Capability{}
		}
		if !hasActivity || !activityCapability.Available {
			m.activityCapability = kernelapi.Capability{}
		}
		if !hasClawHub || !clawHubCapability.Available || m.clawHubClient == nil {
			m.clawHubCapability = kernelapi.Capability{}
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
		if !hasAuthoring || !authoringCapability.Available {
			m.authoringCapability = kernelapi.Capability{}
		}
		if !hasAgentDefinitions || !agentDefinitionCapability.Available || m.config.Owner.Type != runtime.OwnerTypeAgent {
			m.agentDefinitionCapability = kernelapi.Capability{}
		}
		if !hasTeamDefinitions || !teamDefinitionCapability.Available {
			m.teamDefinitionCapability = kernelapi.Capability{}
		}
		if !hasSourcePolicies || !sourcePolicyCapability.Available || m.sourcePolicyClient == nil || !sourcePolicyCapability.Supports(kernelapi.OperationList) {
			m.sourcePolicyCapability = kernelapi.Capability{}
		}
		if !m.objectiveCapability.Available && !m.initiativeCapability.Available && !m.outreachCapability.Available && !m.clawHubCapability.Available && !m.skillActionCapability.Available && !m.skillBindingCapability.Available && !m.sourcePolicyCapability.Available && !m.runCapability.Available && !m.requestCapability.Available && !m.approvalCapability.Available && !m.activityCapability.Available && !m.artifactCapability.Available && !m.channelCapability.Available && !m.authoringCapability.Available && !m.agentDefinitionCapability.Available && !m.teamDefinitionCapability.Available {
			m.unavailable = "This server does not advertise workforce authoring, objectives, Initiatives, canonical work, requests, approvals, activity, Team channels, or artifact evidence."
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
		} else if m.clawHubCapability.Available || m.skillActionCapability.Available || m.skillBindingCapability.Available || m.sourcePolicyCapability.Available {
			m.section = sectionSkills
			if m.clawHubCapability.Available {
				m.mode = modeSkillInstall
				m.editor.Placeholder = "Enter @owner/skill to install…"
			} else {
				m.focusPanelList()
			}
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
		m.activateReadyRefinement()
		return m, tea.Batch(m.loadCompilations(), m.loadTeamDeployments(), m.loadObjectives(), m.loadInitiatives(), m.loadOutreach(), m.loadClawHubSkills(), m.loadSkillBindings(), m.loadSkillActions(), m.loadSourcePolicies(), m.loadRuns(), m.loadAgentRequests(), m.loadActionApprovals(), m.loadActivity(false), m.loadArtifacts(), m.loadConversations())
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
		m.pendingRefinementKey, m.pendingRefinementIntent, m.activeRefinementQuestionID = "", "", ""
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
		return m, tea.Batch(m.loadSourceMonitors(), m.loadOutreach())
	case sourceMonitorsLoaded:
		m.sourceMonitorStatuses = msg.statuses
		m.initiativeActivity = msg.activity
		m.initiativeActivityErrors = msg.activityErrors
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
	case sourcePoliciesLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err, m.sourcePolicies = nil, msg.policies
		return m, nil
	case sourcePolicyChanged:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Source policy change failed. The reviewed draft is preserved for retry."
			return m, m.loadSourcePolicies()
		}
		m.err = nil
		m.editor.Reset()
		m.resetComposerMode()
		m.focusPanelList()
		if msg.result != nil && msg.result.Lifecycle != nil {
			m.status = fmt.Sprintf("Source policy %s · %s@%s · revision %d.", msg.action, msg.result.Lifecycle.PolicyID, msg.result.Lifecycle.ActiveVersion, msg.result.Lifecycle.Revision)
		} else if msg.version != nil && msg.version.Policy != nil {
			m.status = fmt.Sprintf("Source policy version registered · %s@%s. Activate it separately after review.", msg.version.Policy.ID, msg.version.Policy.Version)
		}
		return m, m.loadSourcePolicies()
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
	case outreachLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		if initiative := m.selectedInitiativeRecord(); initiative == nil || initiative.ID != msg.initiativeID {
			return m, nil
		}
		m.err = nil
		m.outreachThreads = msg.threads
		m.outreachObservations = msg.observations
		m.outreachActions = msg.actions
		m.restoreOutreachSelection()
		m.outreachObservationSelected = min(m.outreachObservationSelected, max(0, len(m.outreachObservations)-1))
		m.outreachActionSelected = min(m.outreachActionSelected, max(0, len(m.outreachActions)-1))
		return m, nil
	case outreachCreated:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Outreach draft failed. Your reviewed text is preserved for retry."
			return m, nil
		}
		m.err = nil
		m.pendingOutreachKey, m.pendingOutreachPrompt = "", ""
		m.editor.Reset()
		m.selectedOutreach = msg.thread.ID
		m.status = "Evidence-linked outreach draft saved. Nothing has been sent."
		m.resetComposerMode()
		m.focusPanelList()
		return m, m.loadOutreach()
	case outreachDeliveryCreated:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err
			m.status = "Delivery Run was not created. The draft remains unchanged."
			return m, m.loadOutreach()
		}
		m.err = nil
		m.selectedOutreach = msg.threadID
		m.status = "Governed delivery Run created · " + msg.run.ID + "."
		return m, tea.Batch(m.loadOutreach(), m.loadRuns(), m.loadActionApprovals())
	case outreachRunLoaded:
		m.loading = false
		if msg.err != nil || msg.run == nil {
			if msg.err == nil {
				msg.err = errors.New("kernel returned an empty outreach Run")
			}
			m.err = msg.err
			m.status = "The linked delivery Run could not be loaded."
			return m, nil
		}
		m.err = nil
		m.runs = upsertRun(m.runs, msg.run)
		m.selectedID = msg.run.ID
		m.restoreSelection()
		m.section = sectionRuns
		m.status = "Inspecting governed outreach Run · " + msg.run.ID + "."
		return m, nil
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
			commands = append(commands, m.loadCompilations(), m.loadTeamDeployments(), m.loadObjectives(), m.loadInitiatives(), m.loadOutreach(), m.loadClawHubSkills(), m.loadSourcePolicies(), m.loadRuns(), m.loadAgentRequests(), m.loadActionApprovals(), m.loadActivity(false), m.loadArtifacts(), m.loadConversations())
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
			case modeOutreachCreate:
				return m, m.submitOutreachDraft()
			case modeSkillInstall:
				return m, m.submitClawHubInstall()
			case modeSkillPin:
				return m, m.submitClawHubPin()
			case modeSkillRemove:
				return m, m.submitClawHubRemoval()
			case modeSkillBindingUpsert:
				return m, m.submitSkillBindingUpsert()
			case modeSkillBindingDisable:
				return m, m.submitSkillBindingDisable()
			case modeSourcePolicyRegister:
				return m, m.submitSourcePolicyRegister()
			case modeSourcePolicyActivate:
				return m, m.submitSourcePolicyActivate()
			case modeSourcePolicyRevoke:
				return m, m.submitSourcePolicyRevoke()
			case modeWorkforceAuthoring:
				return m, m.submitWorkforceAuthoring()
			case modeWorkforceRefinement:
				return m, m.submitWorkforceRefinement()
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
	}

	if m.focus == focusPanel {
		switch key {
		case "up", "k":
			if m.section == sectionAuthoring && m.canResolveWorkforceApproval() {
				m.moveWorkforceApprovalSelection(-1)
			} else if m.section == sectionAuthoring && m.canPlaceWorkforceBindingConfigurations() {
				m.moveWorkforceBindingConfigurationSelection(-1)
			} else if m.section == sectionAuthoring && m.canPlaceWorkforceCredentials() {
				m.moveWorkforceCredentialSelection(-1)
			} else {
				m.movePanelSelection(-1)
			}
			if m.section == sectionChannels {
				return m, m.loadSelectedConversation()
			} else if m.section == sectionTeams {
				return m, m.loadTeamAmendments()
			}
		case "down", "j":
			if m.section == sectionAuthoring && m.canResolveWorkforceApproval() {
				m.moveWorkforceApprovalSelection(1)
			} else if m.section == sectionAuthoring && m.canPlaceWorkforceBindingConfigurations() {
				m.moveWorkforceBindingConfigurationSelection(1)
			} else if m.section == sectionAuthoring && m.canPlaceWorkforceCredentials() {
				m.moveWorkforceCredentialSelection(1)
			} else {
				m.movePanelSelection(1)
			}
			if m.section == sectionChannels {
				return m, m.loadSelectedConversation()
			} else if m.section == sectionTeams {
				return m, m.loadTeamAmendments()
			}
		case "w":
			if m.runCapability.Available {
				m.section = sectionRuns
				m.resetEvidenceInspection()
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
			if m.authoringCapability.Available {
				m.section = sectionAuthoring
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
				m.resetEvidenceInspection()
				return m, tea.Batch(m.loadObjectives(), m.loadRuns())
			}
		case "i":
			if m.initiativeCapability.Available {
				m.section = sectionInitiatives
				m.resetEvidenceInspection()
				return m, tea.Batch(m.loadInitiatives(), m.loadRuns())
			}
		case "O":
			if m.outreachCapability.Available {
				m.section = sectionOutreach
				return m, m.loadOutreach()
			}
		case "s":
			if m.clawHubCapability.Available || m.skillActionCapability.Available || m.skillBindingCapability.Available || m.sourcePolicyCapability.Available {
				m.section = sectionSkills
				return m, tea.Batch(m.loadClawHubSkills(), m.loadSkillBindings(), m.loadSkillActions(), m.loadSourcePolicies())
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
			if m.section == sectionRequests && m.canCreateAgentRequestFromSelectedRun() {
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
			} else if m.section == sectionOutreach && m.canCreateOutreachDraft() {
				m.prepareOutreachComposer()
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
		case "[":
			if m.section == sectionAuthoring && m.canPlaceWorkforceBindingConfigurations() {
				m.moveWorkforceBindingConfigurationChoice(-1)
			} else if m.section == sectionAuthoring && m.canPlaceWorkforceCredentials() {
				m.moveWorkforceCredentialChoice(-1)
			} else if m.section == sectionReadiness {
				m.moveAgentAmendmentSelection(-1)
			} else if m.section == sectionTeams {
				m.moveTeamAmendmentSelection(-1)
			} else if m.section == sectionOutreach {
				m.moveOutreachObservation(-1)
				return m, m.loadOutreach()
			} else if m.section == sectionSkills {
				m.moveClawHubSelection(-1)
			} else if m.section == sectionRuns || m.section == sectionObjectives || m.section == sectionInitiatives {
				m.moveEvidenceObservation(-1)
			}
		case "]":
			if m.section == sectionAuthoring && m.canPlaceWorkforceBindingConfigurations() {
				m.moveWorkforceBindingConfigurationChoice(1)
			} else if m.section == sectionAuthoring && m.canPlaceWorkforceCredentials() {
				m.moveWorkforceCredentialChoice(1)
			} else if m.section == sectionReadiness {
				m.moveAgentAmendmentSelection(1)
			} else if m.section == sectionTeams {
				m.moveTeamAmendmentSelection(1)
			} else if m.section == sectionOutreach {
				m.moveOutreachObservation(1)
				return m, m.loadOutreach()
			} else if m.section == sectionSkills {
				m.moveClawHubSelection(1)
			} else if m.section == sectionRuns || m.section == sectionObjectives || m.section == sectionInitiatives {
				m.moveEvidenceObservation(1)
			}
		case "{":
			if m.section == sectionOutreach {
				m.moveOutreachAction(-1)
			} else if m.section == sectionRuns || m.section == sectionObjectives || m.section == sectionInitiatives {
				m.moveGroundingPage(-1)
			}
		case "}":
			if m.section == sectionOutreach {
				m.moveOutreachAction(1)
			} else if m.section == sectionRuns || m.section == sectionObjectives || m.section == sectionInitiatives {
				m.moveGroundingPage(1)
			}
		case "b":
			if m.section == sectionAuthoring && m.canPlaceWorkforceBindingConfigurations() {
				return m, m.submitWorkforceBindingConfigurationPlacement()
			} else if m.section == sectionAuthoring && m.canPlaceWorkforceCredentials() {
				return m, m.submitWorkforceCredentialPlacement()
			} else if m.section == sectionSkills && m.supportsSkillBinding(kernelapi.OperationUpsert) {
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
			} else if m.section == sectionReadiness {
				return m, m.pauseOrResumeAgentDeployment()
			} else if m.section == sectionTeams {
				return m, m.pauseOrResumeTeamDeployment()
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
			} else if m.section == sectionReadiness && m.canResolveSelectedAgentAmendment() {
				m.prepareAgentAmendmentComposer(modeAgentAmendmentReject, "Record why this exact Agent amendment must not proceed…")
			} else if m.section == sectionTeams && m.canResolveSelectedTeamAmendment() {
				m.prepareTeamAmendmentComposer(modeTeamAmendmentReject, "Record why this exact Team amendment must not proceed…")
			} else if m.section == sectionSkills && m.selectedSkillBindingRecord() != nil && !m.selectedSkillBindingRecord().Disabled && m.supportsSkillBinding(kernelapi.OperationDisable) {
				m.mode = modeSkillBindingDisable
				m.editor.Reset()
				m.editor.Placeholder = "Why should this binding be disabled?"
				m.focusComposerEditor()
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
			} else if m.section == sectionReadiness && m.canResolveSelectedAgentAmendment() {
				m.prepareAgentAmendmentComposer(modeAgentAmendmentApprove, "Record why this exact Agent amendment is approved…")
			} else if m.section == sectionTeams && m.canResolveSelectedTeamAmendment() {
				m.prepareTeamAmendmentComposer(modeTeamAmendmentApprove, "Record why this exact Team amendment is approved…")
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
			if m.section == sectionSkills && m.selectedSkillBindingRecord() != nil && m.supportsSkillBinding(kernelapi.OperationUpsert) {
				m.prepareSkillBindingComposer(m.selectedSkillBindingRecord())
			} else if m.section == sectionReadiness && m.canEvaluateSelectedAgentAmendment() {
				m.prepareAgentAmendmentComposer(modeAgentAmendmentEvaluate, agentEvaluationPlaceholder(m.agentDeployment))
			} else if m.section == sectionTeams && m.canEvaluateSelectedTeamAmendment() {
				m.prepareTeamAmendmentComposer(modeTeamAmendmentEvaluate, teamEvaluationPlaceholder(m.selectedTeamDeploymentRecord()))
			} else if m.section == sectionAuthoring && m.canApplyWorkforce() {
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
			} else if m.section == sectionOutreach {
				return m, m.inspectSelectedOutreachRun()
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
		case "D":
			if m.section == sectionOutreach {
				return m, m.deliverSelectedOutreachDraft()
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
			if m.section == sectionReadiness && m.canActivateSelectedAgentAmendment() {
				m.prepareAgentAmendmentComposer(modeAgentAmendmentActivate, "Record why this reviewed Agent definition should become active…")
			} else if m.section == sectionTeams && m.canActivateSelectedTeamAmendment() {
				m.prepareTeamAmendmentComposer(modeTeamAmendmentActivate, "Record why this reviewed Team definition should become active…")
			} else if m.section == sectionSkills {
				return m, m.verifySelectedClawHub()
			} else if m.section == sectionRuns || m.section == sectionObjectives || m.section == sectionInitiatives {
				if lineage := m.selectedEvidenceLineage(); lineage != nil && lineage.err == nil {
					m.evidenceExpanded = !m.evidenceExpanded
					m.evidenceObservationSelected = 0
				}
			}
		case "V":
			if m.section == sectionRuns || m.section == sectionObjectives || m.section == sectionInitiatives {
				if grounding := m.selectedEvidenceGrounding(); grounding != nil && grounding.err == nil {
					m.groundingExpanded = !m.groundingExpanded
					m.groundingPageSelected = 0
				}
			}
		case "P":
			if m.section == sectionSkills && m.sourcePolicyCapability.Supports(kernelapi.OperationRegister) {
				m.prepareSourcePolicyComposer(modeSourcePolicyRegister, "id: public-forums\nversion: 2026-07-22\nsources: forums.example|/feeds|GET\nmax-items: 20\nretention-days: 30\napproval: source-policy-review\nreason: bounded read access")
			}
		case "Y":
			if m.section == sectionSkills && m.sourcePolicyCapability.Supports(kernelapi.OperationActivate) {
				m.prepareSourcePolicyComposer(modeSourcePolicyActivate, "policy: public-forums\nversion: 2026-07-22\nrevision: 0\nreason: reviewed and approved")
			}
		case "X":
			if m.section == sectionSkills && m.sourcePolicyCapability.Supports(kernelapi.OperationRevoke) {
				m.prepareSourcePolicyComposer(modeSourcePolicyRevoke, "policy: public-forums\nrevision: 1\nreason: authority no longer required")
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
		} else if m.config.Owner.Type == runtime.OwnerTypeAgent && m.agentCapabilityClient != nil {
			document, err = m.agentCapabilityClient.AgentDefinitionCapabilities(m.ctx, capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID}, m.config.Owner.ID)
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

func (m *Model) submitWorkforceRefinement() tea.Cmd {
	question := m.readyRefinement()
	input := strings.TrimSpace(m.editor.Value())
	if question == nil || m.busy || input == "" {
		if input == "" {
			m.status = "Answer the current workforce question before continuing."
		}
		return nil
	}
	value, err := m.parseRefinementAnswer(*question, input)
	if err != nil {
		m.err = err
		m.status = "That answer does not match the requested format."
		return nil
	}
	changeSet := m.authoringChangeSet
	intent := fmt.Sprintf("refine\x00%s\x00%d\x00%s\x00%s", changeSet.ID, changeSet.Revision, question.ID, input)
	if m.pendingRefinementKey == "" || m.pendingRefinementIntent != intent {
		m.pendingRefinementKey, m.pendingRefinementIntent = uuid.NewString(), intent
	}
	request := authoring.AnswerChangeSetRefinementRequest{
		Scope: changeSet.Scope, ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision,
		QuestionID: question.ID, Value: value, Source: authoring.RefinementAnswerSourceUser,
		Actor: authoring.ChangeSetActor{Type: m.config.Actor.Type, ID: m.config.Actor.ID},
	}
	key := m.pendingRefinementKey
	m.busy, m.err, m.status = true, nil, "Saving the answer and regenerating the proposal…"
	return func() tea.Msg {
		result, err := m.client.AnswerWorkforceChangeSetRefinement(m.ctx, request, key)
		return workforceGoverned{changeSet: result, action: "Refinement answer", err: err}
	}
}

func (m *Model) parseRefinementAnswer(question authoring.RefinementQuestion, input string) (authoring.RefinementAnswerValue, error) {
	value, err := parseRefinementAnswer(question, input)
	if err != nil || value.CredentialReference == nil {
		return value, err
	}
	if m.authoringCapability.Context == nil {
		return authoring.RefinementAnswerValue{}, errors.New("no authorized credential bindings were advertised")
	}
	for _, binding := range m.authoringCapability.Context.CredentialBindings {
		if binding.Reference == *value.CredentialReference {
			return value, nil
		}
	}
	return authoring.RefinementAnswerValue{}, errors.New("credential reference is not advertised for this proposal")
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

func (m *Model) submitWorkforceCredentialPlacement() tea.Cmd {
	rows := m.workforceCredentialRows()
	if !m.canPlaceWorkforceCredentials() || m.busy || len(rows) == 0 {
		return nil
	}
	credentialReferences := make(map[string]map[string]capability.CredentialReference, len(m.authoringChangeSet.Placement.CredentialReferences))
	for agentID, references := range m.authoringChangeSet.Placement.CredentialReferences {
		credentialReferences[agentID] = make(map[string]capability.CredentialReference, len(references))
		for kind, reference := range references {
			credentialReferences[agentID][kind] = reference
		}
	}
	labels := make([]string, 0, len(rows))
	for _, row := range rows {
		if len(row.Choices) == 0 {
			m.status = fmt.Sprintf("No authorized %s credential is available for %s.", row.Kind, row.AgentName)
			return nil
		}
		selected := m.authoringCredentialChoices[row.Key]
		if selected < 0 || selected >= len(row.Choices) {
			selected = 0
		}
		choice := row.Choices[selected]
		if credentialReferences[row.AgentID] == nil {
			credentialReferences[row.AgentID] = make(map[string]capability.CredentialReference)
		}
		credentialReferences[row.AgentID][row.Kind] = choice.Reference
		labels = append(labels, row.AgentName+" / "+row.Kind+" → "+choice.DisplayName)
	}
	placement := m.authoringChangeSet.Placement
	placement.CredentialReferences = credentialReferences
	intent := fmt.Sprintf("credentials\x00%s\x00%d\x00%s", m.authoringChangeSet.ID, m.authoringChangeSet.Revision, strings.Join(labels, "\x00"))
	if m.pendingGovernanceKey == "" || m.pendingGovernanceIntent != intent {
		m.pendingGovernanceKey, m.pendingGovernanceIntent = uuid.NewString(), intent
	}
	request := authoring.UpdateChangeSetPlacementRequest{
		Scope: m.authoringChangeSet.Scope, ChangeSetID: m.authoringChangeSet.ID, ExpectedRevision: m.authoringChangeSet.Revision,
		Placement: placement, Reason: "Selected authorized credentials: " + strings.Join(labels, "; "),
	}
	key := m.pendingGovernanceKey
	m.busy, m.err, m.status = true, nil, "Saving authorized credential placement…"
	return func() tea.Msg {
		result, err := m.client.UpdateWorkforceChangeSetPlacement(m.ctx, request, key)
		return workforceGoverned{changeSet: result, action: "Credential placement", err: err}
	}
}

func (m *Model) submitWorkforceBindingConfigurationPlacement() tea.Cmd {
	rows := m.workforceBindingConfigurationRows()
	if !m.canPlaceWorkforceBindingConfigurations() || m.busy || len(rows) == 0 {
		return nil
	}
	bindingConfigs := cloneWorkforceBindingConfigs(m.authoringChangeSet.Placement.BindingConfigs)
	labels := make([]string, 0, len(rows))
	for _, row := range rows {
		selected := m.authoringConfigChoices[row.Key]
		if selected < 0 || selected >= len(row.Field.Options) {
			selected = 0
		}
		option := row.Field.Options[selected]
		value, _, err := option.Value.Value()
		if err != nil {
			m.err, m.status = err, "The server advertised an invalid binding configuration choice."
			return nil
		}
		if bindingConfigs[row.AgentID] == nil {
			bindingConfigs[row.AgentID] = make(map[string]map[string]interface{})
		}
		if bindingConfigs[row.AgentID][row.Field.CatalogSkillID] == nil {
			bindingConfigs[row.AgentID][row.Field.CatalogSkillID] = make(map[string]interface{})
		}
		bindingConfigs[row.AgentID][row.Field.CatalogSkillID][row.Field.Key] = value
		labels = append(labels, row.AgentName+" / "+row.Field.Prompt+" → "+option.Label)
	}
	placement := m.authoringChangeSet.Placement
	placement.BindingConfigs = bindingConfigs
	intent := fmt.Sprintf("binding-config\x00%s\x00%d\x00%s", m.authoringChangeSet.ID, m.authoringChangeSet.Revision, strings.Join(labels, "\x00"))
	if m.pendingGovernanceKey == "" || m.pendingGovernanceIntent != intent {
		m.pendingGovernanceKey, m.pendingGovernanceIntent = uuid.NewString(), intent
	}
	request := authoring.UpdateChangeSetPlacementRequest{
		Scope: m.authoringChangeSet.Scope, ChangeSetID: m.authoringChangeSet.ID, ExpectedRevision: m.authoringChangeSet.Revision,
		Placement: placement, Reason: "Selected authorized Skill configuration: " + strings.Join(labels, "; "),
	}
	key := m.pendingGovernanceKey
	m.busy, m.err, m.status = true, nil, "Saving reviewed Skill configuration…"
	return func() tea.Msg {
		result, err := m.client.UpdateWorkforceChangeSetPlacement(m.ctx, request, key)
		return workforceGoverned{changeSet: result, action: "Skill configuration placement", err: err}
	}
}

func cloneWorkforceBindingConfigs(value map[string]map[string]map[string]interface{}) map[string]map[string]map[string]interface{} {
	result := make(map[string]map[string]map[string]interface{}, len(value))
	for agentID, skills := range value {
		result[agentID] = make(map[string]map[string]interface{}, len(skills))
		for skillID, config := range skills {
			result[agentID][skillID] = make(map[string]interface{}, len(config))
			for key, item := range config {
				result[agentID][skillID][key] = item
			}
		}
	}
	return result
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
	return func() tea.Msg {
		approvals, err := m.client.ListActionApprovals(m.ctx, runtime.ApprovalFilter{
			Scope: m.config.Scope, Owner: &owner, Limit: 100,
		})
		return actionApprovalsLoaded{approvals: approvals, err: err}
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
	monitorAvailable := m.supportsSourceMonitor(kernelapi.OperationGetCheckpoint) && m.supportsSourceMonitor(kernelapi.OperationListObservations)
	activityAvailable := m.activityCapability.Supports(kernelapi.OperationList)
	if !monitorAvailable && !activityAvailable {
		return nil
	}
	initiatives := append([]*runtime.Initiative(nil), m.initiatives...)
	return func() tea.Msg {
		statuses := make(map[string]sourceMonitorStatus)
		activity := make(map[string][]runtime.ActivityProjection)
		activityErrors := make(map[string]error)
		for _, initiative := range initiatives {
			if initiative == nil {
				continue
			}
			if activityAvailable {
				page, activityErr := m.client.ListActivity(m.ctx, runtime.ActivityFeedRequest{
					Scope: initiative.Scope, InitiativeID: initiative.ID, Limit: 5,
				})
				if activityErr != nil {
					activityErrors[initiative.ID] = activityErr
				} else if page != nil {
					activity[initiative.ID] = page.Items
				}
			}
			if !monitorAvailable {
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
				if checkpoint != nil && strings.TrimSpace(checkpoint.LastRunID) != "" && m.activityCapability.Supports(kernelapi.OperationList) {
					page, activityErr := m.client.ListActivity(m.ctx, runtime.ActivityFeedRequest{
						Scope: initiative.Scope, RunID: checkpoint.LastRunID, EventTypes: []string{"source_policy.authorized"}, Limit: 25, IncludeDetails: true,
					})
					if activityErr != nil {
						status.policyErr = activityErr
					} else {
						status.policyDecision, status.policyAuthorizedAt = sourcePolicyDecisionForMonitor(page, monitor.ID)
					}
				}
				statuses[key] = status
			}
		}
		return sourceMonitorsLoaded{statuses: statuses, activity: activity, activityErrors: activityErrors}
	}
}

func sourcePolicyDecisionForMonitor(page *runtime.ActivityFeedPage, monitorID string) (*source.PolicyDecision, time.Time) {
	if page == nil {
		return nil, time.Time{}
	}
	for _, event := range page.Items {
		if event.EventType != "source_policy.authorized" || stringPayload(event.Payload, "monitorId") != monitorID {
			continue
		}
		maximumItems, ok := integerPayload(event.Payload, "maximumItems")
		if !ok {
			continue
		}
		decision := &source.PolicyDecision{
			PolicyID: stringPayload(event.Payload, "policyId"), PolicyVersion: stringPayload(event.Payload, "policyVersion"),
			SourceHost: stringPayload(event.Payload, "sourceHost"), PathPrefix: stringPayload(event.Payload, "pathPrefix"), MaximumItems: maximumItems,
		}
		if decision.PolicyID == "" || decision.PolicyVersion == "" || decision.SourceHost == "" || decision.PathPrefix == "" || decision.MaximumItems < 1 {
			continue
		}
		return decision, event.CreatedAt
	}
	return nil, time.Time{}
}

func stringPayload(payload map[string]interface{}, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func integerPayload(payload map[string]interface{}, key string) (int, bool) {
	switch value := payload[key].(type) {
	case int:
		return value, true
	case int64:
		return int(value), int64(int(value)) == value
	case float64:
		return int(value), value == float64(int(value))
	default:
		return 0, false
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

func (m *Model) loadSourcePolicies() tea.Cmd {
	if !m.sourcePolicyCapability.Available || m.sourcePolicyClient == nil || !m.sourcePolicyCapability.Supports(kernelapi.OperationList) {
		return nil
	}
	m.loading = true
	return func() tea.Msg {
		result, err := m.sourcePolicyClient.ListSourcePolicies(m.ctx, capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID})
		if err != nil {
			return sourcePoliciesLoaded{err: err}
		}
		return sourcePoliciesLoaded{policies: result.Items}
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
	if m.section == sectionTeams {
		return m.loadTeamDeployments()
	}
	if m.section == sectionObjectives {
		return tea.Batch(m.loadObjectives(), m.loadRuns())
	}
	if m.section == sectionInitiatives {
		return tea.Batch(m.loadInitiatives(), m.loadRuns())
	}
	if m.section == sectionOutreach {
		return m.loadOutreach()
	}
	if m.section == sectionSkills {
		return tea.Batch(m.loadClawHubSkills(), m.loadSkillBindings(), m.loadSkillActions(), m.loadSourcePolicies())
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

func parseSourcePolicyFields(value string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(value, "\n") {
		key, raw, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(raw) != "" {
			fields[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(raw)
		}
	}
	return fields
}

func parseSourcePolicyRegistration(value string, scope capability.ScopeReference) (source.RegisterVersionRequest, error) {
	fields := parseSourcePolicyFields(value)
	maximumItems, err := strconv.Atoi(fields["max-items"])
	if err != nil {
		return source.RegisterVersionRequest{}, errors.New("max-items must be a positive integer")
	}
	retentionDays, err := strconv.Atoi(fields["retention-days"])
	if err != nil {
		return source.RegisterVersionRequest{}, errors.New("retention-days must be an integer")
	}
	policy := source.Policy{ID: fields["id"], Version: fields["version"], Enabled: true, MaximumItems: maximumItems, RetentionDays: retentionDays, ApprovalPolicy: fields["approval"]}
	for _, rawSource := range strings.Split(fields["sources"], ";") {
		parts := strings.Split(rawSource, "|")
		if len(parts) != 3 {
			return source.RegisterVersionRequest{}, errors.New("sources must use host|paths|methods; separate sources with semicolons")
		}
		policy.Sources = append(policy.Sources, source.PolicySource{Host: strings.TrimSpace(parts[0]), PathPrefixes: splitNonEmpty(parts[1]), Methods: splitNonEmpty(parts[2])})
	}
	reason := fields["reason"]
	request := source.RegisterVersionRequest{Scope: scope, Policy: policy, ActorType: "operator", ActorID: "tui", Reason: reason}
	if reason == "" {
		return source.RegisterVersionRequest{}, errors.New("reason is required")
	}
	return request, nil
}

func parseSourcePolicyActivation(value string, scope capability.ScopeReference) (source.ActivateRequest, error) {
	fields := parseSourcePolicyFields(value)
	revision, err := strconv.ParseInt(fields["revision"], 10, 64)
	if err != nil {
		return source.ActivateRequest{}, errors.New("revision must be zero for first activation or the current lifecycle revision")
	}
	request := source.ActivateRequest{Scope: scope, PolicyID: fields["policy"], Version: fields["version"], ExpectedRevision: revision, ActorType: "operator", ActorID: "tui", Reason: fields["reason"]}
	if request.PolicyID == "" || request.Version == "" || request.Reason == "" {
		return source.ActivateRequest{}, errors.New("policy, version, revision, and reason are required")
	}
	return request, nil
}

func parseSourcePolicyRevocation(value string, scope capability.ScopeReference) (source.RevokeRequest, error) {
	fields := parseSourcePolicyFields(value)
	revision, err := strconv.ParseInt(fields["revision"], 10, 64)
	if err != nil {
		return source.RevokeRequest{}, errors.New("revision must be the current positive lifecycle revision")
	}
	request := source.RevokeRequest{Scope: scope, PolicyID: fields["policy"], ExpectedRevision: revision, ActorType: "operator", ActorID: "tui", Reason: fields["reason"]}
	if request.PolicyID == "" || request.Reason == "" {
		return source.RevokeRequest{}, errors.New("policy, revision, and reason are required")
	}
	return request, nil
}

func (m *Model) submitSourcePolicyRegister() tea.Cmd {
	if m.busy || m.sourcePolicyClient == nil || !m.sourcePolicyCapability.Supports(kernelapi.OperationRegister) {
		return nil
	}
	scope := capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID}
	request, err := parseSourcePolicyRegistration(m.editor.Value(), scope)
	if err != nil {
		m.status = err.Error()
		return nil
	}
	request.ActorType, request.ActorID = m.config.Actor.Type, m.config.Actor.ID
	m.busy, m.err, m.status = true, nil, "Registering immutable source policy version…"
	return func() tea.Msg {
		version, registerErr := m.sourcePolicyClient.RegisterSourcePolicyVersion(m.ctx, request)
		return sourcePolicyChanged{action: "registered", version: version, err: registerErr}
	}
}

func (m *Model) submitSourcePolicyActivate() tea.Cmd {
	if m.busy || m.sourcePolicyClient == nil || !m.sourcePolicyCapability.Supports(kernelapi.OperationActivate) {
		return nil
	}
	scope := capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID}
	request, err := parseSourcePolicyActivation(m.editor.Value(), scope)
	if err != nil {
		m.status = err.Error()
		return nil
	}
	request.ActorType, request.ActorID = m.config.Actor.Type, m.config.Actor.ID
	m.busy, m.err, m.status = true, nil, "Activating reviewed source authority…"
	return func() tea.Msg {
		result, activateErr := m.sourcePolicyClient.ActivateSourcePolicy(m.ctx, request.PolicyID, request)
		return sourcePolicyChanged{action: "active", result: result, err: activateErr}
	}
}

func (m *Model) submitSourcePolicyRevoke() tea.Cmd {
	if m.busy || m.sourcePolicyClient == nil || !m.sourcePolicyCapability.Supports(kernelapi.OperationRevoke) {
		return nil
	}
	scope := capability.ScopeReference{Kind: m.config.Scope.Kind, ID: m.config.Scope.ID}
	request, err := parseSourcePolicyRevocation(m.editor.Value(), scope)
	if err != nil {
		m.status = err.Error()
		return nil
	}
	request.ActorType, request.ActorID = m.config.Actor.Type, m.config.Actor.ID
	m.busy, m.err, m.status = true, nil, "Revoking source authority…"
	return func() tea.Msg {
		result, revokeErr := m.sourcePolicyClient.RevokeSourcePolicy(m.ctx, request.PolicyID, request)
		return sourcePolicyChanged{action: "revoked", result: result, err: revokeErr}
	}
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

func (m *Model) supportsOutreach(operation string) bool {
	return m.ready && m.outreachCapability.Supports(operation)
}

func (m *Model) defaultOperationalSection() panelSection {
	for _, candidate := range []struct {
		section   panelSection
		available bool
	}{
		{sectionAuthoring, m.authoringCapability.Available},
		{sectionTeams, m.teamDefinitionCapability.Available},
		{sectionObjectives, m.objectiveCapability.Available},
		{sectionInitiatives, m.initiativeCapability.Available},
		{sectionOutreach, m.outreachCapability.Available},
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

func (m *Model) supportsInitiative(operation string) bool {
	return m.ready && m.initiativeCapability.Supports(operation)
}

func (m *Model) supportsSourceMonitor(operation string) bool {
	return m.ready && m.sourceMonitorCapability.Supports(operation)
}

func (m *Model) supportsClawHub(operation clawhub.LifecycleOperation) bool {
	return m.ready && m.clawHubClient != nil && m.clawHubCapability.Supports(string(operation))
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

func (m *Model) supportsAuthoring(operation string) bool {
	return m.ready && m.authoringCapability.Supports(operation)
}

func (m *Model) supportsWorkforceAuthoring() bool {
	return m.supportsAuthoring(kernelapi.OperationPropose) || m.supportsAuthoring(kernelapi.OperationCompile)
}

// readyRefinement exposes exactly one server-backed question when the
// contextual capability proves that this exact ChangeSet revision may be
// refined. The TUI never infers authority from host-wide feature flags.
func (m *Model) readyRefinement() *authoring.RefinementQuestion {
	changeSet := m.authoringChangeSet
	context := m.authoringCapability.Context
	if changeSet == nil || context == nil || context.ChangeSetID != changeSet.ID || context.Revision != changeSet.Revision ||
		!m.supportsAuthoring(kernelapi.OperationRefine) {
		return nil
	}
	return changeSet.Refinement.NextQuestion()
}

func (m *Model) activateReadyRefinement() {
	question := m.readyRefinement()
	if question == nil {
		if m.mode == modeWorkforceRefinement {
			m.resetComposerMode()
		}
		return
	}
	m.section = sectionAuthoring
	changed := m.activeRefinementQuestionID != question.ID
	if changed {
		m.editor.Reset()
		m.pendingRefinementKey, m.pendingRefinementIntent = "", ""
	}
	m.activeRefinementQuestionID = question.ID
	m.mode = modeWorkforceRefinement
	m.editor.Placeholder = refinementAnswerPlaceholder(*question)
	if changed {
		m.focusComposerEditor()
	}
}

func parseRefinementAnswer(question authoring.RefinementQuestion, input string) (authoring.RefinementAnswerValue, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return authoring.RefinementAnswerValue{}, errors.New("answer is required")
	}
	switch question.Answer.Kind {
	case authoring.RefinementAnswerText:
		return authoring.RefinementAnswerValue{Text: input}, nil
	case authoring.RefinementAnswerStringList:
		items := splitRefinementSelections(input)
		if len(items) == 0 {
			return authoring.RefinementAnswerValue{}, errors.New("at least one value is required")
		}
		return authoring.RefinementAnswerValue{Items: items}, nil
	case authoring.RefinementAnswerSingleSelect, authoring.RefinementAnswerMultiSelect:
		selected, err := matchRefinementOptions(question.Answer.Options, input)
		if err != nil {
			return authoring.RefinementAnswerValue{}, err
		}
		if question.Answer.Kind == authoring.RefinementAnswerSingleSelect && len(selected) != 1 {
			return authoring.RefinementAnswerValue{}, errors.New("choose exactly one option")
		}
		return authoring.RefinementAnswerValue{OptionIDs: selected}, nil
	case authoring.RefinementAnswerBoolean:
		value, ok := parseRefinementBoolean(input)
		if !ok {
			return authoring.RefinementAnswerValue{}, errors.New("answer yes or no")
		}
		return authoring.RefinementAnswerValue{Boolean: &value}, nil
	case authoring.RefinementAnswerCredentialReference:
		kind, id, ok := strings.Cut(input, "/")
		kind, id = strings.TrimSpace(kind), strings.TrimSpace(id)
		if !ok || kind == "" || id == "" {
			return authoring.RefinementAnswerValue{}, errors.New("use credential kind/id")
		}
		return authoring.RefinementAnswerValue{CredentialReference: &capability.CredentialReference{Kind: kind, ID: id}}, nil
	case authoring.RefinementAnswerSkillSelection:
		selected, err := matchRefinementOptions(question.Answer.Options, input)
		if err != nil {
			return authoring.RefinementAnswerValue{}, err
		}
		return authoring.RefinementAnswerValue{SkillIDs: selected}, nil
	default:
		return authoring.RefinementAnswerValue{}, errors.New("unsupported refinement answer kind")
	}
}

func splitRefinementSelections(input string) []string {
	seen := make(map[string]bool)
	values := make([]string, 0)
	for _, value := range strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == '\n' }) {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			values = append(values, value)
		}
	}
	return values
}

func matchRefinementOptions(options []authoring.RefinementQuestionOption, input string) ([]string, error) {
	byAnswer := make(map[string]string, len(options)*2)
	for _, option := range options {
		byAnswer[strings.ToLower(strings.TrimSpace(option.ID))] = option.ID
		byAnswer[strings.ToLower(strings.TrimSpace(option.Label))] = option.ID
	}
	selected := make([]string, 0)
	seen := make(map[string]bool)
	for _, answer := range splitRefinementSelections(input) {
		id, ok := byAnswer[strings.ToLower(answer)]
		if !ok {
			return nil, fmt.Errorf("unknown option %q", answer)
		}
		if !seen[id] {
			seen[id] = true
			selected = append(selected, id)
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("choose at least one advertised option")
	}
	return selected, nil
}

func parseRefinementBoolean(input string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "yes", "y", "true":
		return true, true
	case "no", "n", "false":
		return false, true
	default:
		return false, false
	}
}

func refinementAnswerPlaceholder(question authoring.RefinementQuestion) string {
	switch question.Answer.Kind {
	case authoring.RefinementAnswerStringList:
		return "Enter comma-separated values…"
	case authoring.RefinementAnswerSingleSelect:
		return "Enter one advertised option…"
	case authoring.RefinementAnswerMultiSelect, authoring.RefinementAnswerSkillSelection:
		return "Enter one or more advertised options, separated by commas…"
	case authoring.RefinementAnswerBoolean:
		return "Answer yes or no…"
	case authoring.RefinementAnswerCredentialReference:
		return "Enter an authorized credential reference as kind/id…"
	default:
		return "Type your answer…"
	}
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

type workforceCredentialRow struct {
	Key       string
	AgentID   string
	AgentName string
	Kind      string
	Choices   []capability.CredentialBindingChoice
}

type workforceBindingConfigurationRow struct {
	Key       string
	AgentID   string
	AgentName string
	Field     capability.BindingConfigurationFieldChoice
}

func (m *Model) workforceBindingConfigurationRows() []workforceBindingConfigurationRow {
	if m.authoringChangeSet == nil || m.authoringCapability.Context == nil {
		return nil
	}
	fields := m.authoringCapability.Context.BindingConfigurationFields
	if capability.ValidateBindingConfigurationFields(fields) != nil {
		return nil
	}
	fieldsBySkill := make(map[string][]capability.BindingConfigurationFieldChoice)
	for _, field := range fields {
		catalogSkill, exists := m.authoringChangeSet.Catalog.Skills[field.CatalogSkillID]
		if !field.Required || !exists || !bindingConfigurationFieldMatchesCatalog(field, catalogSkill) {
			continue
		}
		fieldsBySkill[field.CatalogSkillID] = append(fieldsBySkill[field.CatalogSkillID], field)
	}
	rows := make([]workforceBindingConfigurationRow, 0)
	for _, definition := range m.authoringChangeSet.Result.Candidate.Agents {
		if definition == nil {
			continue
		}
		name := strings.TrimSpace(definition.DisplayName)
		if name == "" {
			name = definition.ID
		}
		for _, requirement := range definition.SkillRequirements {
			for _, field := range fieldsBySkill[requirement.SkillID] {
				if identity := m.authoringChangeSet.Placement.SkillRuntimeIdentities[definition.ID][requirement.SkillID]; identity.Valid() && !identity.Equal(field.Skill) {
					continue
				}
				rows = append(rows, workforceBindingConfigurationRow{
					Key:     definition.ID + "\x00" + requirement.SkillID + "\x00" + field.Key,
					AgentID: definition.ID, AgentName: name, Field: field,
				})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].AgentName != rows[j].AgentName {
			return rows[i].AgentName < rows[j].AgentName
		}
		if rows[i].Field.CatalogSkillID != rows[j].Field.CatalogSkillID {
			return rows[i].Field.CatalogSkillID < rows[j].Field.CatalogSkillID
		}
		return rows[i].Field.Key < rows[j].Field.Key
	})
	return rows
}

func bindingConfigurationFieldMatchesCatalog(field capability.BindingConfigurationFieldChoice, catalogSkill authoring.SkillCapability) bool {
	if field.Skill.ID != catalogSkill.ID || field.Skill.Version != catalogSkill.Version || field.Skill.SourceIdentity != catalogSkill.SourceIdentity {
		return false
	}
	return capability.ValidateBindingConfigurationFieldSchema(field, catalogSkill.BindingConfigSchema) == nil
}

func (m *Model) syncWorkforceBindingConfigurationChoices() {
	rows := m.workforceBindingConfigurationRows()
	m.authoringConfigChoices = make(map[string]int, len(rows))
	for _, row := range rows {
		selected := 0
		current := m.authoringChangeSet.Placement.BindingConfigs[row.AgentID][row.Field.CatalogSkillID][row.Field.Key]
		for index, option := range row.Field.Options {
			value, _, err := option.Value.Value()
			if err == nil && reflect.DeepEqual(current, value) {
				selected = index
				break
			}
		}
		m.authoringConfigChoices[row.Key] = selected
	}
	if len(rows) == 0 {
		m.authoringConfigSelected = 0
	} else {
		m.authoringConfigSelected = min(m.authoringConfigSelected, len(rows)-1)
	}
}

func (m *Model) canPlaceWorkforceBindingConfigurations() bool {
	if m.authoringChangeSet == nil || m.authoringCapability.Context == nil ||
		m.authoringCapability.Context.ChangeSetID != m.authoringChangeSet.ID ||
		m.authoringCapability.Context.Revision != m.authoringChangeSet.Revision || !m.supportsAuthoring(kernelapi.OperationPatch) {
		return false
	}
	rows := m.workforceBindingConfigurationRows()
	for _, row := range rows {
		selected := m.authoringConfigChoices[row.Key]
		if selected < 0 || selected >= len(row.Field.Options) {
			return true
		}
		value, _, err := row.Field.Options[selected].Value.Value()
		if err != nil || !reflect.DeepEqual(m.authoringChangeSet.Placement.BindingConfigs[row.AgentID][row.Field.CatalogSkillID][row.Field.Key], value) {
			return true
		}
	}
	return false
}

func (m *Model) moveWorkforceBindingConfigurationSelection(delta int) {
	rows := m.workforceBindingConfigurationRows()
	if len(rows) == 0 {
		m.authoringConfigSelected = 0
		return
	}
	m.authoringConfigSelected = (m.authoringConfigSelected + delta + len(rows)) % len(rows)
}

func (m *Model) moveWorkforceBindingConfigurationChoice(delta int) {
	rows := m.workforceBindingConfigurationRows()
	if len(rows) == 0 {
		return
	}
	row := rows[min(m.authoringConfigSelected, len(rows)-1)]
	selected := m.authoringConfigChoices[row.Key]
	m.authoringConfigChoices[row.Key] = (selected + delta + len(row.Field.Options)) % len(row.Field.Options)
}

func (m *Model) workforceCredentialRows() []workforceCredentialRow {
	if m.authoringChangeSet == nil || m.authoringCapability.Context == nil {
		return nil
	}
	choicesByKind := make(map[string][]capability.CredentialBindingChoice)
	for _, choice := range m.authoringCapability.Context.CredentialBindings {
		kind := strings.TrimSpace(choice.Reference.Kind)
		if kind != "" && strings.TrimSpace(choice.Reference.ID) != "" && strings.TrimSpace(choice.DisplayName) != "" {
			choicesByKind[kind] = append(choicesByKind[kind], choice)
		}
	}
	for kind := range choicesByKind {
		sort.Slice(choicesByKind[kind], func(i, j int) bool {
			left, right := choicesByKind[kind][i], choicesByKind[kind][j]
			if left.DisplayName == right.DisplayName {
				return left.Reference.ID < right.Reference.ID
			}
			return left.DisplayName < right.DisplayName
		})
	}
	names := make(map[string]string)
	for _, definition := range m.authoringChangeSet.Result.Candidate.Agents {
		if definition != nil {
			names[definition.ID] = definition.DisplayName
		}
	}
	rows := make([]workforceCredentialRow, 0)
	for agentID, kinds := range m.authoringChangeSet.RequiredCredentials {
		for _, kind := range kinds {
			kind = strings.TrimSpace(kind)
			if kind == "" {
				continue
			}
			name := names[agentID]
			if name == "" {
				name = agentID
			}
			rows = append(rows, workforceCredentialRow{Key: agentID + "\x00" + kind, AgentID: agentID, AgentName: name, Kind: kind, Choices: choicesByKind[kind]})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].AgentName == rows[j].AgentName {
			return rows[i].Kind < rows[j].Kind
		}
		return rows[i].AgentName < rows[j].AgentName
	})
	return rows
}

func (m *Model) syncWorkforceCredentialChoices() {
	rows := m.workforceCredentialRows()
	m.authoringCredentialChoices = make(map[string]int, len(rows))
	for _, row := range rows {
		selected := 0
		current := m.authoringChangeSet.Placement.CredentialReferences[row.AgentID][row.Kind]
		for index, choice := range row.Choices {
			if choice.Reference == current {
				selected = index
				break
			}
		}
		m.authoringCredentialChoices[row.Key] = selected
	}
	if len(rows) == 0 {
		m.authoringCredentialSelected = 0
	} else {
		m.authoringCredentialSelected = min(m.authoringCredentialSelected, len(rows)-1)
	}
}

func (m *Model) canPlaceWorkforceCredentials() bool {
	return m.authoringChangeSet != nil && m.authoringCapability.Context != nil &&
		m.authoringCapability.Context.ChangeSetID == m.authoringChangeSet.ID &&
		m.authoringCapability.Context.Revision == m.authoringChangeSet.Revision &&
		m.supportsAuthoring(kernelapi.OperationPatch) && len(m.workforceCredentialRows()) > 0
}

func (m *Model) moveWorkforceCredentialSelection(delta int) {
	rows := m.workforceCredentialRows()
	if len(rows) == 0 {
		m.authoringCredentialSelected = 0
		return
	}
	m.authoringCredentialSelected = (m.authoringCredentialSelected + delta + len(rows)) % len(rows)
}

func (m *Model) moveWorkforceCredentialChoice(delta int) {
	rows := m.workforceCredentialRows()
	if len(rows) == 0 {
		return
	}
	row := rows[min(m.authoringCredentialSelected, len(rows)-1)]
	if len(row.Choices) == 0 {
		return
	}
	selected := m.authoringCredentialChoices[row.Key]
	m.authoringCredentialChoices[row.Key] = (selected + delta + len(row.Choices)) % len(row.Choices)
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
	m.resetEvidenceInspection()
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
	m.resetEvidenceInspection()
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
	if m.section == sectionInitiatives {
		m.moveInitiativeSelection(delta)
		return
	}
	if m.section == sectionOutreach {
		m.moveOutreachSelection(delta)
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
	m.resetEvidenceInspection()
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

func (m *Model) prepareSourcePolicyComposer(mode editorMode, template string) {
	m.mode = mode
	m.editor.Reset()
	m.editor.SetValue(template)
	m.editor.Placeholder = template
	m.focusComposerEditor()
}

func (m *Model) prepareComposerForSection() {
	switch {
	case m.section == sectionAuthoring && m.readyRefinement() != nil:
		m.activateReadyRefinement()
	case m.section == sectionReadiness && m.canProposeAgentBehaviorAmendment():
		m.prepareAgentAmendmentComposer(modeAgentAmendmentPropose, "systemPrompt|personality\nConcise rationale\nNew immutable value")
	case m.section == sectionTeams && m.canProposeTeamPurposeAmendment():
		m.prepareTeamAmendmentComposer(modeTeamAmendmentPropose, "First line: concise rationale\nRemaining lines: the Team's new purpose")
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
	case m.section == sectionOutreach && m.canCreateOutreachDraft():
		m.prepareOutreachComposer()
	case m.section == sectionSkills && m.supportsClawHub(clawhub.LifecycleInstall):
		m.mode = modeSkillInstall
		m.editor.Placeholder = "Enter @owner/skill to install…"
		m.focusComposerEditor()
	case m.section == sectionSkills && m.supportsSkillBinding(kernelapi.OperationUpsert):
		m.prepareSkillBindingComposer(nil)
	case m.section == sectionSkills && m.sourcePolicyCapability.Supports(kernelapi.OperationRegister):
		m.prepareSourcePolicyComposer(modeSourcePolicyRegister, "id: public-forums\nversion: 2026-07-22\nsources: forums.example|/feeds|GET\nmax-items: 20\nretention-days: 30\napproval: source-policy-review\nreason: bounded read access")
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
		if m.readyRefinement() != nil {
			m.activateReadyRefinement()
			return
		}
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
	if m.section == sectionOutreach {
		m.mode = modeCreate
		m.editor.Placeholder = "Select evidence and an authorized action, then press n to draft outreach."
		return
	}
	if m.section == sectionSkills {
		if m.supportsSkillBinding(kernelapi.OperationUpsert) {
			m.mode = modeSkillBindingUpsert
			m.editor.Placeholder = "Describe the exact Skill authority…"
		} else if m.supportsClawHub(clawhub.LifecycleInstall) {
			m.mode = modeSkillInstall
			m.editor.Placeholder = "Enter @owner/skill to install…"
		} else if m.sourcePolicyCapability.Supports(kernelapi.OperationRegister) {
			m.mode = modeSourcePolicyRegister
			m.editor.Placeholder = "Press P to review a source policy version template."
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

func clawHubClient(kernelClient client.KernelClient) client.ClawHubClient {
	value, _ := kernelClient.(client.ClawHubClient)
	return value
}

func skillBindingClient(kernelClient client.KernelClient) client.SkillBindingClient {
	value, _ := kernelClient.(client.SkillBindingClient)
	return value
}

func sourcePolicyClient(kernelClient client.KernelClient) client.SourcePolicyLifecycleClient {
	value, _ := kernelClient.(client.SourcePolicyLifecycleClient)
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
