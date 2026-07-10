// Package tui implements OpenSeal's prompt-first terminal client. It owns no
// durable state: every action is discovered from and sent to the public kernel
// API used by other embedding surfaces.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
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
	sectionRuns panelSection = iota
	sectionArtifacts
)

type editorMode int

const (
	modeCreate editorMode = iota
	modeGuide
)

type Model struct {
	ctx                context.Context
	client             client.KernelClient
	config             Config
	editor             textarea.Model
	focus              focusArea
	section            panelSection
	mode               editorMode
	width              int
	height             int
	loading            bool
	busy               bool
	ready              bool
	unavailable        string
	err                error
	status             string
	runCapability      kernelapi.Capability
	artifactCapability kernelapi.Capability
	runs               []*runtime.AgentRun
	selected           int
	selectedID         string
	artifacts          []*runtime.Artifact
	artifactSelected   int
	selectedArtifact   string
	artifactExpanded   bool
	pendingKey         string
	pendingGoal        string
}

type capabilitiesLoaded struct {
	document kernelapi.CapabilityDocument
	err      error
}

type runsLoaded struct {
	runs []*runtime.AgentRun
	err  error
}

type artifactsLoaded struct {
	artifacts []*runtime.Artifact
	err       error
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
		focus: focusComposer, section: sectionRuns, width: 100, height: 30,
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
		if msg.document.Version != kernelapi.Version {
			m.unavailable = fmt.Sprintf("Server contract v%s is not supported by this TUI (requires v%s).", msg.document.Version, kernelapi.Version)
			m.ready = false
			return m, nil
		}
		runCapability, hasRuns := msg.document.Find(kernelapi.AgentRunsCapabilityID, kernelapi.AgentRunsCapabilityVersion)
		artifactCapability, hasArtifacts := msg.document.Find(kernelapi.ArtifactsCapabilityID, kernelapi.ArtifactsCapabilityVersion)
		m.runCapability = runCapability
		m.artifactCapability = artifactCapability
		if !hasRuns || !runCapability.Available {
			m.runCapability = kernelapi.Capability{}
		}
		if !hasArtifacts || !artifactCapability.Available {
			m.artifactCapability = kernelapi.Capability{}
		}
		if !m.runCapability.Available && !m.artifactCapability.Available {
			m.unavailable = "This server does not advertise canonical work or artifact evidence."
			m.ready = false
			return m, nil
		}
		m.ready = true
		m.unavailable = ""
		m.err = nil
		if !m.runCapability.Available && m.artifactCapability.Available {
			m.section = sectionArtifacts
			m.focusPanelList()
		}
		return m, tea.Batch(m.loadRuns(), m.loadArtifacts())
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
			commands = append(commands, m.loadRuns(), m.loadArtifacts())
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
			if m.supportsRun(kernelapi.OperationCreate) {
				m.focusComposerEditor()
			}
		}
		return m, nil
	case "esc":
		if m.mode == modeGuide {
			m.mode = modeCreate
			m.editor.Reset()
			m.editor.Placeholder = "Describe the outcome you want…"
			m.status = "Guidance canceled."
			return m, nil
		}
	case "ctrl+s":
		if m.focus == focusComposer {
			if m.mode == modeGuide {
				return m, m.submitGuidance()
			}
			return m, m.submitRun()
		}
	}

	if m.focus == focusPanel {
		switch key {
		case "up", "k":
			m.movePanelSelection(-1)
		case "down", "j":
			m.movePanelSelection(1)
		case "w":
			if m.runCapability.Available {
				m.section = sectionRuns
			}
		case "a":
			if m.artifactCapability.Available {
				m.section = sectionArtifacts
			}
		case "n":
			if m.section == sectionRuns && m.supportsRun(kernelapi.OperationCreate) {
				m.mode = modeCreate
				m.editor.Placeholder = "Describe the outcome you want…"
				m.focusComposerEditor()
			}
		case "r":
			return m, m.loadPanel()
		case "p":
			if m.section == sectionRuns {
				return m, m.pauseOrResume()
			}
		case "x":
			if m.section == sectionRuns {
				return m, m.commandSelected(runtime.AgentRunCommandCancel, "")
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
			if m.section == sectionArtifacts && m.selectedArtifactRecord() != nil {
				m.artifactExpanded = !m.artifactExpanded
			}
		case "d":
			if m.section == sectionArtifacts {
				return m, m.downloadSelectedArtifact()
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
		document, err := m.client.Capabilities(m.ctx)
		return capabilitiesLoaded{document: document, err: err}
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

func (m *Model) loadPanel() tea.Cmd {
	if m.section == sectionArtifacts {
		return m.loadArtifacts()
	}
	return m.loadRuns()
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

func (m *Model) supportsArtifact(operation string) bool {
	return m.ready && m.artifactCapability.Supports(operation)
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
	if m.section == sectionArtifacts {
		m.moveArtifactSelection(delta)
		return
	}
	m.moveSelection(delta)
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
