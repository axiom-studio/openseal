package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/charmbracelet/lipgloss"
)

var (
	accent        = lipgloss.Color("#6C8CFF")
	accentSoft    = lipgloss.Color("#AAB8FF")
	muted         = lipgloss.Color("#7B8496")
	text          = lipgloss.Color("#E7EAF0")
	border        = lipgloss.Color("#333A49")
	danger        = lipgloss.Color("#FF7A90")
	success       = lipgloss.Color("#7BDCB5")
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(text)
	brandStyle    = lipgloss.NewStyle().Bold(true).Foreground(accent)
	mutedStyle    = lipgloss.NewStyle().Foreground(muted)
	panelStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(1, 2)
	selectedStyle = lipgloss.NewStyle().Foreground(text).Background(lipgloss.Color("#27304A")).Bold(true)
)

func (m *Model) render() string {
	width := max(m.width-4, 36)
	header := m.renderHeader(width)
	var body string
	if m.unavailable != "" {
		body = panelStyle.Width(width - 4).Render(
			headerStyle.Render("Capability unavailable") + "\n\n" +
				m.unavailable + "\n\n" + mutedStyle.Render("The TUI will not show controls the server cannot perform."),
		)
	} else if !m.ready {
		message := "Connecting to the OpenSeal kernel…"
		if m.err != nil {
			message = "OpenSeal could not be reached.\n\n" + m.err.Error() + "\n\nStart it with: openseal daemon"
		}
		body = panelStyle.Width(width - 4).Render(message)
	} else {
		composer := m.renderComposer(m.composerWidth())
		panel := m.renderPanel(m.runsWidth())
		if m.width >= 90 {
			body = lipgloss.JoinHorizontal(lipgloss.Top, composer, "  ", panel)
		} else {
			body = composer + "\n\n" + panel
		}
	}
	footer := m.renderFooter(width)
	return lipgloss.NewStyle().Padding(1, 2).Render(header + "\n\n" + body + "\n" + footer)
}

func (m *Model) renderHeader(width int) string {
	title := brandStyle.Render("OpenSeal") + "  " + headerStyle.Render("Agents, Teams, Objectives & Work")
	connection := mutedStyle.Render(fmt.Sprintf("%s · %s/%s", m.config.Endpoint, m.config.Scope.Kind, m.config.Scope.ID))
	space := max(1, width-lipgloss.Width(title)-lipgloss.Width(connection))
	return title + strings.Repeat(" ", space) + connection
}

func (m *Model) renderComposer(width int) string {
	if m.mode == modeTeamAmendmentPropose && !m.supportsTeamDefinition(kernelapi.OperationProposeAmendment) {
		return m.renderUnavailableComposer(width, "Propose Team amendment", "This server does not advertise governed Team amendments.")
	}
	if m.mode == modeTeamAmendmentEvaluate && !m.supportsTeamDefinition(kernelapi.OperationEvaluateAmendment) {
		return m.renderUnavailableComposer(width, "Evaluate Team amendment", "This server does not advertise Team amendment evaluations.")
	}
	if (m.mode == modeTeamAmendmentApprove || m.mode == modeTeamAmendmentReject) && !m.supportsTeamDefinition(kernelapi.OperationResolveAmendment) {
		return m.renderUnavailableComposer(width, "Review Team amendment", "This server does not advertise Team amendment decisions.")
	}
	if m.mode == modeTeamAmendmentActivate && !m.supportsTeamDefinition(kernelapi.OperationActivateAmendment) {
		return m.renderUnavailableComposer(width, "Activate Team amendment", "This server does not advertise Team amendment activation.")
	}
	if m.mode == modeWorkforceAuthoring && !m.supportsWorkforceAuthoring() {
		return m.renderUnavailableComposer(width, "Create Agents and Teams", "This server does not advertise workforce authoring.")
	}
	if m.mode == modeObjectiveCreate && !m.supportsObjective(kernelapi.OperationCreate) {
		return m.renderUnavailableComposer(width, "Add an objective", "This server does not advertise objective creation.")
	}
	if m.mode == modeObjectiveEdit && !m.supportsObjective(kernelapi.OperationUpdate) {
		return m.renderUnavailableComposer(width, "Amend objective", "This server does not advertise objective updates.")
	}
	if m.mode == modeInitiativeCreate && !m.supportsInitiative(kernelapi.OperationCreate) {
		return m.renderUnavailableComposer(width, "Create an Initiative", "This server does not advertise Initiative creation.")
	}
	if m.mode == modeInitiativeEdit && !m.supportsInitiative(kernelapi.OperationPatch) {
		return m.renderUnavailableComposer(width, "Amend Initiative", "This server does not advertise Initiative updates.")
	}
	if m.mode == modeSkillInstall && !m.supportsClawHub(clawhub.LifecycleInstall) {
		return m.renderUnavailableComposer(width, "Install a Skill", "This server does not advertise governed ClawHub installation.")
	}
	if m.mode == modeChannelCreate && !m.supportsChannel(kernelapi.OperationCreate) {
		return m.renderUnavailableComposer(width, "Create a Team channel", "This server does not advertise channel creation.")
	}
	if m.mode == modeChannelPost && !m.supportsChannel(kernelapi.OperationPost) {
		return m.renderUnavailableComposer(width, "Message the Team", "This server does not advertise channel messaging.")
	}
	if !m.supportsRun(kernelapi.OperationCreate) && m.mode != modeGuide && m.mode != modeChannelCreate && m.mode != modeChannelPost && m.mode != modeWorkforceAuthoring && m.mode != modeWorkforceApprove && m.mode != modeWorkforceReject && m.mode != modeWorkforceApply && m.mode != modeWorkforceRetry && m.mode != modeRequestCreate && m.mode != modeRequestAccept && m.mode != modeRequestReject && m.mode != modeRequestClarify && m.mode != modeRequestProvideClarification && m.mode != modeRequestComplete && m.mode != modeApprovalApprove && m.mode != modeApprovalReject && m.mode != modeTeamAmendmentPropose && m.mode != modeTeamAmendmentEvaluate && m.mode != modeTeamAmendmentApprove && m.mode != modeTeamAmendmentReject && m.mode != modeTeamAmendmentActivate {
		content := headerStyle.Render("Start durable work") + "\n" +
			mutedStyle.Render("This server does not advertise work creation.") + "\n\n" +
			"You can still inspect the capabilities and evidence available in this workspace."
		return panelStyle.Width(max(width-4, 30)).Render(content)
	}
	title := "Start durable work"
	description := "Describe an outcome. OpenSeal will keep the work safe across restarts."
	owner := humanOwner(m.config.Owner)
	switch m.mode {
	case modeTeamAmendmentPropose:
		title = "Propose Team purpose amendment"
		description = "Record a concise rationale and a new immutable Team purpose for governed review."
		owner = "No behavior changes until activation"
	case modeTeamAmendmentEvaluate:
		title = "Evaluate Team amendment"
		description = "Record pass/fail evidence for every declared criterion at this exact revision."
		owner = "Revision-bound evaluation audit"
	case modeTeamAmendmentApprove:
		title = "Approve Team amendment"
		description = "Record why this reviewed candidate may proceed. The decision is permanent."
		owner = "Eligible principal · revision-bound decision"
	case modeTeamAmendmentReject:
		title = "Reject Team amendment"
		description = "Record why this candidate must not proceed. The decision is permanent."
		owner = "Eligible principal · revision-bound decision"
	case modeTeamAmendmentActivate:
		title = "Activate Team amendment"
		description = "Atomically activate this exact reviewed definition and reload authoritative Team state."
		owner = "Optimistic revision · immutable activation audit"
	case modeWorkforceAuthoring:
		if m.authoringResult == nil {
			title = "Create Agents and Teams"
			description = "Describe outcomes, roles, boundaries, and collaboration. Review the exact candidate before activation."
		} else {
			title = "Refine the workforce"
			description = "Describe a change. OpenSeal will compile a new immutable candidate and show its governed diff."
		}
		owner = "Governed review · authoring never activates state"
	case modeWorkforceApprove:
		title = "Approve policy requirement"
		description = "Record why the exact advertised requirement is satisfied. The decision is permanent."
		owner = "Revision-bound governed decision"
	case modeWorkforceReject:
		title = "Reject workforce proposal"
		description = "Explain why this reviewed proposal must not proceed. The decision is permanent."
		owner = "Revision-bound governed decision"
	case modeWorkforceApply:
		title = "Create reviewed workforce"
		description = "Record why this exact candidate should now create its Agents, Team, and objectives atomically."
		owner = "Atomic Apply · permanent audit receipt"
	case modeWorkforceRetry:
		title = "Retry proposal generation"
		description = "Record why the failed generation should be resumed as a new durable attempt."
		owner = "Revision-bound retry · idempotent request"
	case modeGuide:
		title = "Guide selected work"
		description = "Add a concise instruction without replacing the objective."
	case modeChannelCreate:
		title = "Create a Team channel"
		description = "Name a durable place for focused collaboration."
	case modeChannelPost:
		title = "Message the Team"
		description = "Share useful context or ask a question. Messages remain auditable."
		if conversation := m.selectedConversationRecord(); conversation != nil {
			owner = "In #" + conversation.Title
		}
	case modeObjectiveCreate:
		title = "Add an objective"
		description = "Describe a durable outcome. This owner can pursue several objectives concurrently."
	case modeObjectiveEdit:
		title = "Amend selected objective"
		description = "Record a new goal revision without losing its runs, budget, or audit history."
	case modeInitiativeCreate:
		title = "Create an Initiative"
		description = "Compose durable objectives into a coordinated outcome that survives restarts."
		if objective := m.selectedObjectiveRecord(); objective != nil {
			owner = "Starts with Objective · " + compact(objective.Title, 48)
		} else {
			owner = "Select an Objective first"
		}
	case modeInitiativeEdit:
		title = "Amend selected Initiative"
		description = "Refine its purpose without losing coordination state or audit history."
	case modeSkillInstall:
		title = "Install a ClawHub Skill"
		description = "Enter an owner-qualified reference. OpenSeal verifies, compiles, and activates it atomically."
		owner = "Governed canonical Skill lifecycle"
	case modeSkillPin:
		title = "Pin selected Skill version"
		description = "Record why updates must stay fixed at this exact verified version."
		owner = "Permanent lifecycle reason"
	case modeSkillRemove:
		title = "Confirm Skill removal"
		description = "Type REMOVE exactly. OpenSeal will preserve modified or pinned installations."
		owner = "Governed uninstall · no force fallback"
	case modeRequestAccept:
		title = "Accept collaboration request"
		description = "Accept this exact revision and create traceable child work for the recipient."
		owner = "Recipient decision · durable run lineage"
	case modeRequestCreate:
		title = "Request collaboration"
		description = "Choose an Agent or Team, then describe the outcome. OpenSeal links it to the selected durable Run."
		owner = "Request or handoff · credential-free shared context"
		if run := m.selectedRun(); run != nil {
			owner = "From selected Run · " + compact(run.Goal, 44)
		}
	case modeRequestReject:
		title = "Reject collaboration request"
		description = "Record why this request cannot proceed. The requesting work will receive the decision."
		owner = "Recipient decision · permanent audit"
	case modeRequestClarify:
		title = "Request clarification"
		description = "Ask one focused question before deciding whether to accept the work."
		owner = "Recipient question · requesting work resumes"
	case modeRequestProvideClarification:
		title = "Provide clarification"
		description = "Answer the recipient's question without creating a separate conversation state."
		owner = "Requester response · same durable request"
	case modeRequestComplete:
		title = "Complete collaboration request"
		description = "Summarize the completed outcome. OpenSeal will atomically complete child work and resume its requester."
		owner = "Revision-bound completion · idempotent retry"
	case modeApprovalApprove:
		title = "Approve governed action"
		description = "Record why this exact proposed action is safe. The decision resumes its waiting Run."
		owner = "Eligible principal · permanent decision"
	case modeApprovalReject:
		title = "Reject governed action"
		description = "Record why this exact proposed action must not execute."
		owner = "Eligible principal · permanent decision"
	}
	content := headerStyle.Render(title) + "\n" + mutedStyle.Render(description) + "\n\n" + m.editor.View() + "\n\n" + mutedStyle.Render(owner)
	if m.focus == focusComposer {
		content += "\n" + lipgloss.NewStyle().Foreground(accentSoft).Render("Ctrl+S submit  ·  Tab inspect")
	}
	return panelStyle.Width(max(width-4, 30)).Render(content)
}

func (m *Model) renderUnavailableComposer(width int, title, message string) string {
	content := headerStyle.Render(title) + "\n" + mutedStyle.Render(message) + "\n\n" +
		"You can still inspect the durable channel history available in this workspace."
	return panelStyle.Width(max(width-4, 30)).Render(content)
}

func (m *Model) renderPanel(width int) string {
	tabs := m.renderPanelTabs()
	var content string
	if m.section == sectionAuthoring {
		content = m.renderAuthoringContent(width)
	} else if m.section == sectionReadiness {
		content = m.renderReadinessContent(width)
	} else if m.section == sectionTeams {
		content = m.renderTeamsContent(width)
	} else if m.section == sectionObjectives {
		content = m.renderObjectivesContent(width)
	} else if m.section == sectionInitiatives {
		content = m.renderInitiativesContent(width)
	} else if m.section == sectionSkills {
		content = m.renderClawHubSkillsContent(width)
	} else if m.section == sectionChannels {
		content = m.renderChannelsContent(width)
	} else if m.section == sectionRequests {
		content = m.renderAgentRequestsContent(width)
	} else if m.section == sectionApprovals {
		content = m.renderActionApprovalsContent(width)
	} else if m.section == sectionActivity {
		content = m.renderActivityContent(width)
	} else if m.section == sectionArtifacts {
		content = m.renderArtifactsContent(width)
	} else {
		content = m.renderRunsContent(width)
	}
	return panelStyle.Width(max(width-4, 30)).Render(tabs + "\n\n" + content)
}

func (m *Model) renderPanelTabs() string {
	tabs := make([]string, 0, 12)
	if m.authoringCapability.Available {
		label := "f Workforce"
		if m.section == sectionAuthoring {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	if m.agentDefinitionCapability.Available {
		label := "h Runtime"
		if m.section == sectionReadiness {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	if m.teamDefinitionCapability.Available {
		label := "T Teams"
		if m.section == sectionTeams {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	if m.objectiveCapability.Available {
		label := "o Objectives"
		if m.section == sectionObjectives {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	if m.initiativeCapability.Available {
		label := "i Initiatives"
		if m.section == sectionInitiatives {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	if m.clawHubCapability.Available || m.skillActionCapability.Available {
		label := "s Skills"
		if m.section == sectionSkills {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	if m.runCapability.Available {
		label := "w Work"
		if m.section == sectionRuns {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	if m.requestCapability.Available {
		label := "R Requests"
		if m.section == sectionRequests {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	if m.approvalCapability.Available {
		label := "A Approvals"
		if m.section == sectionApprovals {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	if m.activityCapability.Available {
		label := "t Activity"
		if m.section == sectionActivity {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	if m.channelCapability.Available {
		label := "c Channels"
		if m.section == sectionChannels {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	if m.artifactCapability.Available {
		label := "a Evidence"
		if m.section == sectionArtifacts {
			label = selectedStyle.Render(label)
		} else {
			label = mutedStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	return strings.Join(tabs, "  ")
}

func (m *Model) renderReadinessContent(width int) string {
	title := headerStyle.Render("Native runtime readiness")
	if m.loading && len(m.compilations) == 0 {
		return title + "\n\n" + mutedStyle.Render("Loading immutable compilation evidence…")
	}
	if len(m.compilations) == 0 {
		return title + "\n\n" + mutedStyle.Render("No compilation record exists for this Agent. Runtime readiness is unverified.")
	}
	latest := m.compilations[0]
	lines := []string{title, ""}
	if latest.Status == "clean" {
		lines = append(lines, lipgloss.NewStyle().Foreground(success).Bold(true).Render("READY")+"  Behavior compiled to the native Agent runtime.")
	} else {
		lines = append(lines, lipgloss.NewStyle().Foreground(danger).Bold(true).Render("NEEDS ATTENTION")+"  This candidate was not activated because behavior could not be preserved.")
		for _, diagnostic := range latest.Diagnostics {
			location := diagnostic.Path
			if diagnostic.NodeID != "" {
				location = "Node " + diagnostic.NodeID + " · " + location
			}
			lines = append(lines, "", "• "+compact(diagnostic.Message, max(width-10, 24)), mutedStyle.Render("  "+location+" · "+diagnostic.Code))
		}
	}
	lines = append(lines, "", mutedStyle.Render(fmt.Sprintf("Candidate %s · source %s %s · %d immutable record(s)", compact(latest.CandidateVersion, 20), latest.Source.Kind, latest.Source.Version, len(m.compilations))))
	return strings.Join(lines, "\n")
}

func (m *Model) renderTeamsContent(width int) string {
	title := headerStyle.Render("Team deployments")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, ""}
	if len(m.teamDeployments) == 0 {
		lines = append(lines, mutedStyle.Render("No deployed Teams yet. Use Workforce to describe the outcomes and roles you need."))
		return strings.Join(lines, "\n")
	}
	visible := max(3, min(len(m.teamDeployments), max(m.height-18, 5)))
	start := max(0, min(m.teamDeploymentSelected-visible/2, len(m.teamDeployments)-visible))
	for index := start; index < min(len(m.teamDeployments), start+visible); index++ {
		entry := m.teamDeployments[index]
		if entry.Deployment == nil {
			continue
		}
		name := entry.Deployment.ID
		if entry.Definition != nil && strings.TrimSpace(entry.Definition.DisplayName) != "" {
			name = entry.Definition.DisplayName
		}
		prefix, style := "  ", lipgloss.NewStyle().Foreground(text)
		if index == m.teamDeploymentSelected {
			prefix, style = "› ", selectedStyle
		}
		lines = append(lines, style.Render(fmt.Sprintf("%s%-9s %s", prefix, entry.Deployment.Status, compact(name, max(width-17, 20)))))
	}
	entry := m.selectedTeamDeploymentRecord()
	if entry == nil || entry.Deployment == nil {
		return strings.Join(lines, "\n")
	}
	deployment := entry.Deployment
	definition := entry.Definition
	lines = append(lines, "", mutedStyle.Render("Selected"))
	if definition != nil {
		lines = append(lines, compact(definition.Purpose, max(width-8, 24)))
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("Definition %s@%s · %s coordination", definition.ID, definition.Version, definition.Coordination.Mode)))
	} else {
		lines = append(lines, mutedStyle.Render("Definition metadata is unavailable."))
	}
	lines = append(lines, mutedStyle.Render(fmt.Sprintf("Deployment %s · revision %d · %d member(s)", deployment.ID, deployment.Revision, len(deployment.Roster))))
	if deployment.Restrictions.MaximumConcurrency > 0 || deployment.Restrictions.MaximumRisk != "" {
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("Bounds · %d concurrent · maximum risk %s", deployment.Restrictions.MaximumConcurrency, deployment.Restrictions.MaximumRisk)))
	}
	if len(deployment.Roster) > 0 {
		lines = append(lines, "", mutedStyle.Render("Roster"))
		for _, assignment := range deployment.Roster {
			name := assignment.DisplayName
			if strings.TrimSpace(name) == "" {
				name = assignment.AgentDeploymentID
			}
			lines = append(lines, compact(fmt.Sprintf("• %s · %s · Agent %s", name, assignment.RoleID, assignment.AgentDeploymentID), max(width-6, 24)))
		}
	}
	if definition != nil && len(definition.ObjectiveTemplates) > 0 {
		lines = append(lines, "", mutedStyle.Render(fmt.Sprintf("Objective portfolio · %d template(s)", len(definition.ObjectiveTemplates))))
		for _, objective := range definition.ObjectiveTemplates[:min(3, len(definition.ObjectiveTemplates))] {
			lines = append(lines, compact("• "+objective.Title, max(width-6, 24)))
		}
	}
	if m.supportsTeamDefinition(kernelapi.OperationListAmendments) {
		lines = append(lines, "", mutedStyle.Render(fmt.Sprintf("Governance history · %d amendment(s)", len(m.teamAmendments))))
		for index, amendment := range m.teamAmendments[:min(3, len(m.teamAmendments))] {
			if amendment == nil {
				continue
			}
			prefix := "  "
			if index == m.teamAmendmentSelected {
				prefix = "› "
			}
			lines = append(lines, compact(fmt.Sprintf("%s%s · %s → %s · r%d", prefix, amendment.Status, amendment.BaseVersion, amendment.Candidate.Version, amendment.Revision), max(width-6, 24)))
		}
		if amendment := m.selectedTeamAmendmentRecord(); amendment != nil {
			lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("Proposed by %s:%s · %s", amendment.ProposerType, amendment.ProposerID, amendment.Rationale), max(width-6, 24))))
			if amendment.RiskWidening {
				lines = append(lines, lipgloss.NewStyle().Foreground(danger).Render("Risk widening · approval required"))
			}
			for _, change := range amendment.Changes {
				lines = append(lines, mutedStyle.Render("  Changed · "+change.Field))
			}
			for _, evaluation := range amendment.Evaluations {
				outcome := "failed"
				if evaluation.Passed {
					outcome = "passed"
				}
				lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("  Evaluation %s · %s · %s", evaluation.CriterionID, outcome, evaluation.Summary), max(width-8, 24))))
			}
			if amendment.Decision != nil {
				decision := "rejected"
				if amendment.Decision.Approved {
					decision = "approved"
				}
				lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("  Decision %s by %s:%s · %s", decision, amendment.Decision.ActorType, amendment.Decision.ActorID, amendment.Decision.Reason), max(width-8, 24))))
			}
			if amendment.Status == kernelteam.AmendmentActivated {
				lines = append(lines, mutedStyle.Render("  Activation · "+amendment.ActivationID))
			}
		}
	}
	actions := []string{"r refresh"}
	if m.supportsTeamDefinition(kernelapi.OperationUpdate) && (deployment.Status == "active" || deployment.Status == "paused") {
		actions = append(actions, "p pause/resume")
	}
	if m.canProposeTeamPurposeAmendment() {
		actions = append(actions, "m propose purpose")
	}
	if len(m.teamAmendments) > 1 {
		actions = append(actions, "[ ] amendment")
	}
	if m.canEvaluateSelectedTeamAmendment() {
		actions = append(actions, "Enter evaluate")
	}
	if m.canResolveSelectedTeamAmendment() {
		actions = append(actions, "y approve", "x reject")
	}
	if m.canActivateSelectedTeamAmendment() {
		actions = append(actions, "v activate")
	}
	lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render(strings.Join(actions, " · ")))
	return strings.Join(lines, "\n")
}

func (m *Model) renderAuthoringContent(width int) string {
	title := headerStyle.Render("Workforce candidate")
	if m.busy {
		return title + "\n\n" + mutedStyle.Render("Saving a verified Agent and Team change set…")
	}
	result := m.authoringResult
	if result == nil {
		if changeSet := m.authoringChangeSet; changeSet != nil {
			lines := []string{title, "", mutedStyle.Render(fmt.Sprintf("Change set %s · %s · revision %d", compact(changeSet.ID, 16), changeSet.Status, changeSet.Revision))}
			if changeSet.Generation != nil {
				lines = append(lines, mutedStyle.Render(fmt.Sprintf("Run %s · attempt %d", compact(changeSet.Generation.RunID, 16), changeSet.Generation.Attempt)))
				if changeSet.Generation.LastError != "" {
					lines = append(lines, "", lipgloss.NewStyle().Foreground(danger).Render(compact(changeSet.Generation.LastError, max(width-8, 24))))
				}
			}
			if changeSet.Status == authoring.ChangeSetEvaluating {
				lines = append(lines, "", mutedStyle.Render("Generation is durable and continues in the background. Refresh is automatic."))
			}
			if m.canRetryWorkforce() {
				lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render("r retry generation"))
			}
			return strings.Join(lines, "\n")
		}
		return title + "\n\n" + mutedStyle.Render("Describe the workforce on the left. OpenSeal will verify every generated definition, Skill gap, authority change, and role assignment.")
	}
	state := lipgloss.NewStyle().Foreground(success).Render("READY FOR REVIEW")
	issues := len(result.Questions) + len(result.Validation) + len(result.MissingRequirements)
	if !result.Valid {
		state = lipgloss.NewStyle().Foreground(accentSoft).Render(fmt.Sprintf("%d ITEM(S) NEED ATTENTION", issues))
	}
	teamName, teamPurpose, roles, objectives := "Workforce", "", 0, 0
	if result.Candidate.Team != nil {
		teamName, teamPurpose = result.Candidate.Team.DisplayName, result.Candidate.Team.Purpose
		roles, objectives = len(result.Candidate.Team.Roles), len(result.Candidate.Team.ObjectiveTemplates)
	}
	lines := []string{title, state, "", headerStyle.Render(compact(teamName, max(width-8, 24)))}
	if m.authoringChangeSet != nil {
		changeSet := m.authoringChangeSet
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("Change set %s · %s · revision %d", compact(changeSet.ID, 16), changeSet.Status, changeSet.Revision)))
		if len(changeSet.Evaluations) > 0 {
			evaluation := changeSet.Evaluations[len(changeSet.Evaluations)-1]
			outcome := "denied"
			if evaluation.Allowed {
				outcome = "allowed"
			}
			lines = append(lines, mutedStyle.Render(fmt.Sprintf("Policy %s · %s:%s · %d requirement(s)", outcome, evaluation.Actor.Type, evaluation.Actor.ID, len(evaluation.ApprovalRequirements))))
		}
		if len(changeSet.ApprovalDecisions) > 0 {
			decision := changeSet.ApprovalDecisions[len(changeSet.ApprovalDecisions)-1]
			outcome := "rejected"
			if decision.Approved {
				outcome = "approved"
			}
			lines = append(lines, mutedStyle.Render(fmt.Sprintf("Latest decision %s · %s / %s · %s:%s", outcome, decision.PolicyID, decision.Role, decision.Actor.Type, decision.Actor.ID)))
			if decision.Reason != "" {
				lines = append(lines, mutedStyle.Render("  "+compact(decision.Reason, max(width-10, 24))))
			}
		}
		if len(changeSet.Lifecycle) > 0 {
			event := changeSet.Lifecycle[len(changeSet.Lifecycle)-1]
			lines = append(lines, mutedStyle.Render(fmt.Sprintf("Audit r%d · %s · %s:%s · %s", event.Revision, event.Reason, event.Actor.Type, event.Actor.ID, event.At.Format(time.RFC3339))))
		}
		if changeSet.ApplyReceipt != nil {
			receipt := changeSet.ApplyReceipt
			lines = append(lines, lipgloss.NewStyle().Foreground(success).Render(fmt.Sprintf("Created atomically · %d resources · receipt %s", len(receipt.Resources), compact(receipt.ID, 12))))
			lines = append(lines, mutedStyle.Render(fmt.Sprintf("%s · %s:%s · %s", compact(receipt.Reason, max(width-24, 24)), receipt.Actor.Type, receipt.Actor.ID, receipt.AppliedAt.Format(time.RFC3339))))
			for _, resource := range receipt.Resources {
				detail := resource.Version
				if resource.Revision > 0 {
					detail = fmt.Sprintf("r%d", resource.Revision)
				}
				lines = append(lines, mutedStyle.Render(fmt.Sprintf("  %s  %s  %s", resource.Kind, compact(resource.ID, max(width-28, 16)), detail)))
			}
		}
		if m.canResolveWorkforceApproval() {
			lines = append(lines, "", mutedStyle.Render("Eligible policy requirements (j/k select)"))
			for index, reference := range m.authoringCapability.Context.EligibleApprovalRequirements {
				prefix := "  "
				style := mutedStyle
				if index == m.authoringApprovalSelected {
					prefix, style = "› ", selectedStyle
				}
				lines = append(lines, style.Render(fmt.Sprintf("%s%s / %s · evaluation %s", prefix, reference.PolicyID, reference.Role, compact(reference.EvaluationID, 12))))
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(accentSoft).Render("y approve · x reject selected requirement"))
		} else if m.canApplyWorkforce() {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render("Enter create this exact reviewed workforce"))
		} else if m.supportsAuthoring(kernelapi.OperationEvaluate) {
			lines = append(lines, "", mutedStyle.Render("Waiting for the configured policy evaluator to submit its decision."))
		}
		if m.canPlaceWorkforceCredentials() {
			rows := m.workforceCredentialRows()
			lines = append(lines, "", mutedStyle.Render("Authorized credential placement (j/k select · [/] choose)"))
			for index, row := range rows {
				prefix, style := "  ", mutedStyle
				if index == m.authoringCredentialSelected {
					prefix, style = "› ", selectedStyle
				}
				choiceLabel := "No authorized credential available"
				if len(row.Choices) > 0 {
					selected := m.authoringCredentialChoices[row.Key]
					if selected < 0 || selected >= len(row.Choices) {
						selected = 0
					}
					choice := row.Choices[selected]
					choiceLabel = choice.DisplayName
					if m.authoringChangeSet.Placement.CredentialReferences[row.AgentID][row.Kind] == choice.Reference {
						choiceLabel += " · saved"
					}
				}
				lines = append(lines, style.Render(fmt.Sprintf("%s%s · %s → %s", prefix, compact(row.AgentName, max(width-34, 16)), row.Kind, compact(choiceLabel, max(width-38, 18)))))
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(accentSoft).Render("b save credential placement"))
		}
	}
	if teamPurpose != "" {
		lines = append(lines, mutedStyle.Render(compact(teamPurpose, max(width-8, 24))))
	}
	lines = append(lines, mutedStyle.Render(fmt.Sprintf("%d Agents · %d roles · %d Team objectives", len(result.Candidate.Agents), roles, objectives)), "")
	if m.authoringAmendment && (len(result.Diff) > 0 || len(result.RiskChanges) > 0) {
		widening := 0
		for _, change := range result.RiskChanges {
			if change.Widening {
				widening++
			}
		}
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("%d field changes · %d risk changes · %d widening", len(result.Diff), len(result.RiskChanges), widening)), "")
	}
	for _, agent := range result.Candidate.Agents {
		lines = append(lines, fmt.Sprintf("• %s  %s · %d concurrent", compact(agent.DisplayName, max(width-28, 18)), agent.Authority.MaximumRisk, agent.Authority.MaxConcurrentRuns))
	}
	for _, question := range result.Questions {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render("? "+compact(question, max(width-8, 24))))
	}
	for _, missing := range result.MissingRequirements {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render(fmt.Sprintf("Connect %s %s for %s", missing.Kind, missing.ID, missing.RequiredBy)))
	}
	for _, issue := range result.Validation {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(danger).Render(compact(issue.Path+": "+issue.Message, max(width-8, 24))))
	}
	if m.authoringChangeSet == nil || m.authoringChangeSet.Status != authoring.ChangeSetApplied {
		lines = append(lines, "", mutedStyle.Render("Nothing is active. Tab to refine this candidate."))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderObjectivesContent(width int) string {
	title := headerStyle.Render("Objective portfolio")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, ""}
	if len(m.objectives) == 0 {
		lines = append(lines, mutedStyle.Render("No objectives yet. Add an outcome this Agent or Team should own."))
	} else {
		visible := max(3, min(len(m.objectives), max(m.height-18, 5)))
		start := max(0, min(m.objectiveSelected-visible/2, len(m.objectives)-visible))
		for index := start; index < min(len(m.objectives), start+visible); index++ {
			objective := m.objectives[index]
			prefix := "  "
			style := lipgloss.NewStyle().Foreground(text)
			if index == m.objectiveSelected {
				prefix = "› "
				style = selectedStyle
			}
			line := fmt.Sprintf("%s%-10s %s", prefix, string(objective.Status), compact(objective.Title, max(width-18, 20)))
			lines = append(lines, style.Render(line))
		}
	}
	if objective := m.selectedObjectiveRecord(); objective != nil {
		lines = append(lines, "", mutedStyle.Render("Selected"), compact(objective.Goal, max(width-8, 24)))
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("Revision %d · updated %s", objective.Revision, relativeTime(objective.UpdatedAt))))
		if objective.Budget != nil {
			allocated := len(objective.BudgetAllocations)
			lines = append(lines, mutedStyle.Render(fmt.Sprintf("Bounded autonomy · %d run allocation(s)", allocated)))
			lines = append(lines, renderBudgetLines(objective.Budget, nil, max(width-8, 24))...)
		}
		if m.supportsObjective(kernelapi.OperationUpdate) {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render("Enter amend  ·  n add objective"))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderInitiativesContent(width int) string {
	title := headerStyle.Render("Initiative portfolio")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, ""}
	if len(m.initiatives) == 0 {
		lines = append(lines, mutedStyle.Render("No Initiatives yet. Compose one from a selected Objective."))
	} else {
		visible := max(3, min(len(m.initiatives), max(m.height-18, 5)))
		start := max(0, min(m.initiativeSelected-visible/2, len(m.initiatives)-visible))
		for index := start; index < min(len(m.initiatives), start+visible); index++ {
			initiative := m.initiatives[index]
			prefix, style := "  ", lipgloss.NewStyle().Foreground(text)
			if index == m.initiativeSelected {
				prefix, style = "› ", selectedStyle
			}
			lines = append(lines, style.Render(fmt.Sprintf("%s%-10s %s", prefix, string(initiative.Status), compact(initiative.Title, max(width-18, 20)))))
		}
	}
	if initiative := m.selectedInitiativeRecord(); initiative != nil {
		lines = append(lines, "", mutedStyle.Render("Selected"), compact(initiative.Purpose, max(width-8, 24)))
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("%d objectives · %d milestones · %d monitors · %d deliverables", len(initiative.ObjectiveRefs), len(initiative.Milestones), len(initiative.SourceMonitors), len(initiative.Deliverables))))
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("Revision %d · updated %s", initiative.Revision, relativeTime(initiative.UpdatedAt))))
		if len(initiative.SourceMonitors) > 0 {
			lines = append(lines, "", mutedStyle.Render("Source monitors"))
			for _, monitor := range initiative.SourceMonitors {
				lines = append(lines, compact(fmt.Sprintf("%s · %s@%s %s", monitor.ID, monitor.SkillID, monitor.SkillVersion, monitor.Action), max(width-4, 24)))
				details := fmt.Sprintf("Agent %s · %s", monitor.AssignedAgentID, monitor.Deduplication)
				lines = append(lines, mutedStyle.Render(compact(details, max(width-6, 24))))
				if monitor.SourcePolicyRef != "" {
					lines = append(lines, mutedStyle.Render(compact("Policy · "+monitor.SourcePolicyRef, max(width-6, 24))))
				}
				if objective := m.objectiveRecord(monitor.ObjectiveID); objective != nil {
					if objective.NextEvaluationAt != nil {
						lines = append(lines, mutedStyle.Render(compact("Next evaluation "+relativeTime(*objective.NextEvaluationAt), max(width-6, 24))))
					}
					if condition := objective.ScheduleCondition; condition != nil {
						conditionLine := compact(fmt.Sprintf("Schedule %s · %s", strings.ReplaceAll(string(condition.State), "_", " "), condition.Reason), max(width-6, 24))
						style := mutedStyle
						if condition.State == runtime.ObjectiveScheduleBudgetExhausted {
							style = lipgloss.NewStyle().Foreground(danger)
						}
						lines = append(lines, style.Render(conditionLine))
					}
				}
				if status, ok := m.sourceMonitorStatuses[sourceMonitorStatusKey(initiative.ID, monitor.ID)]; ok {
					if status.err != nil {
						lines = append(lines, lipgloss.NewStyle().Foreground(danger).Render(compact("Status unavailable · "+status.err.Error(), max(width-6, 24))))
					} else if status.checkpoint == nil {
						lines = append(lines, mutedStyle.Render("Awaiting first successful Run"))
					} else {
						lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("Last success %s · %d evidence · Run %s", relativeTime(status.checkpoint.LastSuccessAt), status.checkpoint.ObservationCount, status.checkpoint.LastRunID), max(width-6, 24))))
						if status.policyErr != nil {
							lines = append(lines, lipgloss.NewStyle().Foreground(danger).Render(compact("Authorization audit unavailable · "+status.policyErr.Error(), max(width-6, 24))))
						} else if status.policyDecision != nil {
							decision := status.policyDecision
							lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("Authorized by %s@%s · %s", decision.PolicyID, decision.PolicyVersion, relativeTime(status.policyAuthorizedAt)), max(width-6, 24))))
							lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("  %s%s · up to %d items", decision.SourceHost, decision.PathPrefix, decision.MaximumItems), max(width-6, 24))))
						} else if m.activityCapability.Supports(kernelapi.OperationList) {
							lines = append(lines, mutedStyle.Render("No authorization decision recorded for the last Run"))
						}
						for _, observation := range status.observations[:min(3, len(status.observations))] {
							lines = append(lines, mutedStyle.Render(compact("  • "+observation.Summary+" · "+observation.SourceURI, max(width-6, 24))))
							if observation.ArtifactRef != nil {
								lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("    Artifact %s · revision %d", observation.ArtifactRef.ID, observation.ArtifactRef.Revision), max(width-8, 24))))
							}
						}
					}
				} else if m.supportsSourceMonitor(kernelapi.OperationGetCheckpoint) {
					lines = append(lines, mutedStyle.Render("Refreshing durable status…"))
				}
			}
		}
		actions := []string{}
		if m.supportsInitiative(kernelapi.OperationPatch) {
			actions = append(actions, "Enter amend", "p pause/resume", "l link selected objective")
		}
		if m.supportsInitiative(kernelapi.OperationCreate) {
			actions = append(actions, "n create")
		}
		if len(actions) > 0 {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render(strings.Join(actions, "  ·  ")))
		}
	} else if m.supportsInitiative(kernelapi.OperationCreate) {
		lines = append(lines, "", mutedStyle.Render("Select an Objective, then press i and n to compose an Initiative."))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderClawHubSkillsContent(width int) string {
	title := headerStyle.Render("Skills and authorized actions")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, "", mutedStyle.Render("Installed packages")}
	if !m.clawHubCapability.Available {
		lines = append(lines, mutedStyle.Render("Skill installation is not available from this kernel."))
	} else if len(m.clawHubSkills) == 0 {
		lines = append(lines, mutedStyle.Render("No ClawHub Skills installed. Press n and enter @owner/skill."))
	} else {
		visible := max(3, min(len(m.clawHubSkills), max(m.height-18, 5)))
		start := max(0, min(m.clawHubSelected-visible/2, len(m.clawHubSkills)-visible))
		for index := start; index < min(len(m.clawHubSkills), start+visible); index++ {
			skill := m.clawHubSkills[index]
			prefix, style := "  ", lipgloss.NewStyle().Foreground(text)
			if index == m.clawHubSelected {
				prefix, style = "› ", selectedStyle
			}
			state := "verified"
			if !skill.Verified {
				state = "unverified"
			}
			if skill.Pinned {
				state = "pinned"
			}
			lines = append(lines, style.Render(fmt.Sprintf("%s%-10s %s@%s", prefix, state, skill.Reference.String(), skill.Version)))
		}
	}
	if skill := m.selectedClawHubRecord(); skill != nil {
		lines = append(lines, "", mutedStyle.Render("Selected"), skill.SourceIdentity, mutedStyle.Render(fmt.Sprintf("v%s · installed %s", skill.Version, time.UnixMilli(skill.InstalledAt).Format("2006-01-02"))))
		if skill.PinReason != "" {
			lines = append(lines, mutedStyle.Render("Pin · "+skill.PinReason))
		}
		if skill.LocallyModified {
			lines = append(lines, lipgloss.NewStyle().Foreground(danger).Render("Local modifications detected · update and removal are protected"))
		}
		actions := []string{}
		if m.supportsClawHub(clawhub.LifecycleVerifyInstalled) {
			actions = append(actions, "v verify")
		}
		if m.supportsClawHub(clawhub.LifecycleUpdate) && !skill.Pinned && !skill.LocallyModified {
			actions = append(actions, "u update")
		}
		if m.supportsClawHub(clawhub.LifecycleUpdateAll) {
			actions = append(actions, "U update all")
		}
		if skill.Pinned && m.supportsClawHub(clawhub.LifecycleUnpin) {
			actions = append(actions, "p unpin")
		} else if !skill.Pinned && m.supportsClawHub(clawhub.LifecyclePin) {
			actions = append(actions, "p pin")
		}
		if m.supportsClawHub(clawhub.LifecycleUninstall) {
			actions = append(actions, "x remove")
		}
		if m.supportsClawHub(clawhub.LifecycleInstall) {
			actions = append(actions, "n install")
		}
		lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render(strings.Join(actions, "  ·  ")))
	}
	lines = append(lines, "", mutedStyle.Render("Authorized for this Agent"))
	if !m.skillActionCapability.Available {
		lines = append(lines, mutedStyle.Render("The kernel does not advertise owner-scoped action discovery."))
	} else if len(m.skillActions) == 0 {
		lines = append(lines,
			mutedStyle.Render("No executable action is bound to this Agent."),
			mutedStyle.Render("Install or bind a Skill, satisfy its credential and policy requirements, then refresh."),
		)
	} else {
		for _, action := range m.skillActions {
			identity := fmt.Sprintf("%s@%s/%s", action.SkillID, action.Version, action.Action)
			contract := fmt.Sprintf("%s · %s · binding %s@%d", action.Risk, action.SideEffect, compact(action.BindingID, 24), action.BindingRevision)
			lines = append(lines, "• "+compact(action.Name, max(width-8, 24))+"  "+mutedStyle.Render(identity), mutedStyle.Render("  "+contract))
			if len(action.SemanticArguments) > 0 {
				roles := make([]string, 0, len(action.SemanticArguments))
				for role, argument := range action.SemanticArguments {
					roles = append(roles, role+" → "+argument)
				}
				sort.Strings(roles)
				lines = append(lines, mutedStyle.Render("  "+strings.Join(roles, " · ")))
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderRunsContent(width int) string {
	title := headerStyle.Render("Current work")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, ""}
	if len(m.runs) == 0 {
		lines = append(lines, mutedStyle.Render("No work yet. Describe an outcome to begin."))
	} else {
		visible := max(3, min(len(m.runs), max(m.height-18, 5)))
		start := max(0, min(m.selected-visible/2, len(m.runs)-visible))
		for index := start; index < min(len(m.runs), start+visible); index++ {
			run := m.runs[index]
			prefix := "  "
			style := lipgloss.NewStyle().Foreground(text)
			if index == m.selected {
				prefix = "› "
				style = selectedStyle
			}
			goal := compact(run.Goal, max(width-20, 20))
			line := fmt.Sprintf("%s%-12s %s", prefix, humanStatus(run.Status), goal)
			lines = append(lines, style.Render(line))
		}
	}
	if run := m.selectedRun(); run != nil {
		lines = append(lines, "", mutedStyle.Render("Selected"), compact(run.Goal, max(width-8, 24)))
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("Updated %s · revision %d", relativeTime(run.UpdatedAt), run.Revision)))
		if run.ParentRunID != "" {
			lines = append(lines, mutedStyle.Render(compact("Parent Run "+run.ParentRunID, max(width-8, 24))))
		}
		if run.Budget != nil {
			state := run.BudgetState
			if state == "" {
				state = runtime.BudgetStateActive
			}
			lines = append(lines, mutedStyle.Render("Autonomy budget · "+string(state)))
			lines = append(lines, renderBudgetLines(run.Budget, &run.BudgetUsage, max(width-8, 24))...)
		}
		if receipt, ok := runDeliveryReceipt(run); ok {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(success).Bold(true).Render("DELIVERY ACCEPTED"))
			summary := fmt.Sprintf("%d recipient%s · %d artifact%s", receipt.recipientCount, pluralSuffix(receipt.recipientCount), len(receipt.artifacts), pluralSuffix(len(receipt.artifacts)))
			if len(receipt.domains) > 0 {
				summary += " · " + strings.Join(receipt.domains, ", ")
			}
			lines = append(lines, compact(summary+" · "+receipt.deliveredAt, max(width-8, 24)))
			lines = append(lines, mutedStyle.Render("Receipt "+compact(receipt.id, max(width-16, 16))))
			for _, artifact := range receipt.artifacts {
				lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("Artifact %s · version %d", artifact.id, artifact.version), max(width-8, 24))))
			}
		}
		actions := m.availableActions(run)
		if actions != "" {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render(actions))
		}
	}
	if m.focus == focusPanel {
		lines = append(lines, "", mutedStyle.Render("↑/↓ select · n new · r refresh · Tab compose"))
	}
	return strings.Join(lines, "\n")
}

func renderBudgetLines(policy *runtime.BudgetPolicy, usage *runtime.BudgetUsage, width int) []string {
	if policy == nil {
		return nil
	}
	type dimension struct {
		label string
		used  int64
		limit int64
	}
	dimensions := []dimension{
		{label: "attempts", limit: policy.MaxAttempts},
		{label: "turns", limit: policy.MaxTurns},
		{label: "input", limit: policy.MaxInputTokens},
		{label: "output", limit: policy.MaxOutputTokens},
		{label: "tokens", limit: policy.MaxTotalTokens},
		{label: "cost μ", limit: policy.MaxCostMicros},
		{label: "duration ms", limit: policy.MaxDurationMS},
		{label: "actions", limit: policy.MaxActions},
	}
	if usage != nil {
		dimensions[0].used = usage.Attempts
		dimensions[1].used = usage.Turns
		dimensions[2].used = usage.InputTokens
		dimensions[3].used = usage.OutputTokens
		dimensions[4].used = usage.InputTokens + usage.OutputTokens
		dimensions[5].used = usage.CostMicros
		dimensions[6].used = usage.DurationMS
		dimensions[7].used = usage.Actions
	}
	parts := make([]string, 0, len(dimensions))
	for _, item := range dimensions {
		if item.limit == 0 {
			continue
		}
		if usage == nil {
			parts = append(parts, fmt.Sprintf("%s %d", item.label, item.limit))
		} else {
			parts = append(parts, fmt.Sprintf("%s %d/%d", item.label, item.used, item.limit))
		}
	}
	if len(parts) == 0 {
		return []string{mutedStyle.Render("No finite limits")}
	}
	lines := make([]string, 0, (len(parts)+2)/3)
	for len(parts) > 0 {
		count := min(3, len(parts))
		lines = append(lines, mutedStyle.Render(compact(strings.Join(parts[:count], " · "), width)))
		parts = parts[count:]
	}
	return lines
}

func (m *Model) renderAgentRequestsContent(width int) string {
	title := headerStyle.Render("Collaboration requests")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, ""}
	if len(m.agentRequests) == 0 {
		lines = append(lines, mutedStyle.Render("No incoming or outgoing requests for this Agent or Team."))
	} else {
		visible := max(3, min(len(m.agentRequests), max(m.height-21, 5)))
		start := max(0, min(m.agentRequestSelected-visible/2, len(m.agentRequests)-visible))
		local := m.localCollaborationParty()
		for index := start; index < min(len(m.agentRequests), start+visible); index++ {
			request := m.agentRequests[index]
			prefix, style := "  ", lipgloss.NewStyle().Foreground(text)
			if index == m.agentRequestSelected {
				prefix, style = "› ", selectedStyle
			}
			direction, peer := "to", request.Recipient
			if request.Recipient == local {
				direction, peer = "from", request.Requester
			}
			line := fmt.Sprintf("%s%-10s %s %s:%s · %s", prefix, humanAgentRequestStatus(request.Status), direction, peer.Type, compact(peer.ID, 14), compact(request.Goal, max(width-38, 18)))
			lines = append(lines, style.Render(compact(line, max(width-4, 28))))
		}
	}
	if request := m.selectedAgentRequestRecord(); request != nil {
		lines = append(lines, "", mutedStyle.Render(fmt.Sprintf("%s · revision %d · updated %s", request.Kind, request.Revision, relativeTime(request.UpdatedAt))))
		lines = append(lines, compact(request.Goal, max(width-8, 24)))
		if request.Instructions != "" {
			lines = append(lines, mutedStyle.Render(compact(request.Instructions, max(width-8, 24))))
		}
		lineage := "Source Run " + compact(request.SourceRunID, 14)
		if request.ChildRunID != "" {
			lineage += " · child " + compact(request.ChildRunID, 14)
		}
		if request.SemanticRole != "" {
			lineage += " · role " + request.SemanticRole
		}
		lines = append(lines, mutedStyle.Render(compact(lineage, max(width-8, 24))))
		if request.Clarification != "" {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render("Question · "+compact(request.Clarification, max(width-18, 24))))
		}
		if request.Response != "" {
			lines = append(lines, mutedStyle.Render("Response · "+compact(request.Response, max(width-18, 24))))
		}
		if request.CompletionSummary != "" {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(success).Render("Completed · "+compact(request.CompletionSummary, max(width-20, 24))))
		}
		if request.ResolutionReason != "" {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(danger).Render(compact(request.ResolutionReason, max(width-8, 24))))
		}
		if len(request.AcceptanceCriteria) > 0 || len(request.ArtifactRequirements) > 0 {
			required := 0
			for _, requirement := range request.ArtifactRequirements {
				if requirement.Required {
					required++
				}
			}
			lines = append(lines, mutedStyle.Render(fmt.Sprintf("Evidence contract · %d acceptance field(s) · %d required artifact(s)", len(request.AcceptanceCriteria), required)))
		}
		actions := make([]string, 0, 5)
		if m.canRespondToSelectedRequest(runtime.AgentRequestDecisionAccept) {
			actions = append(actions, "y accept", "? clarify", "x reject")
		}
		if m.canRespondToSelectedRequest(runtime.AgentRequestDecisionProvideClarification) {
			actions = append(actions, "M answer clarification")
		}
		if m.canCompleteSelectedAgentRequest() {
			actions = append(actions, "Enter complete")
		} else if request.Status == runtime.AgentRequestStatusAccepted && request.Recipient == m.localCollaborationParty() && (len(request.AcceptanceCriteria) > 0 || hasRequiredArtifact(request.ArtifactRequirements)) {
			lines = append(lines, mutedStyle.Render("Completion remains with the child Run until its required evidence and artifacts are registered."))
		}
		if len(actions) > 0 {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render(strings.Join(actions, "  ·  ")))
		}
	}
	if m.focus == focusPanel {
		actions := []string{"↑/↓ select", "r refresh", "R requests"}
		if m.canCreateAgentRequestFromSelectedRun() {
			actions = append(actions, "n request from selected Work")
		}
		lines = append(lines, "", mutedStyle.Render(strings.Join(actions, " · ")))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderActionApprovalsContent(width int) string {
	title := headerStyle.Render("Action approvals")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, ""}
	if len(m.actionApprovals) == 0 {
		lines = append(lines, mutedStyle.Render("No governed action checkpoints for this Agent or Team."))
	} else {
		visible := max(3, min(len(m.actionApprovals), max(m.height-21, 5)))
		start := max(0, min(m.actionApprovalSelected-visible/2, len(m.actionApprovals)-visible))
		for index := start; index < min(len(m.actionApprovals), start+visible); index++ {
			approval := m.actionApprovals[index]
			prefix, style := "  ", lipgloss.NewStyle().Foreground(text)
			if index == m.actionApprovalSelected {
				prefix, style = "› ", selectedStyle
			}
			line := fmt.Sprintf("%s%-10s %-10s %s", prefix, approval.Status, approval.Risk, compact(approval.Summary, max(width-28, 20)))
			lines = append(lines, style.Render(compact(line, max(width-4, 28))))
		}
	}
	if approval := m.selectedActionApprovalRecord(); approval != nil {
		lines = append(lines, "", compact(approval.Summary, max(width-8, 24)))
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("Run %s · action %s · revision %d", compact(approval.RunID, 14), compact(approval.ActionCallID, 14), approval.Revision)))
		if skillID, ok := approval.ProposedAction["skillId"].(string); ok {
			action, _ := approval.ProposedAction["action"].(string)
			version, _ := approval.ProposedAction["skillVersion"].(string)
			lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("Proposed · %s@%s / %s", skillID, version, action), max(width-8, 24))))
		}
		if approval.PolicyReason != "" {
			lines = append(lines, "", mutedStyle.Render("Policy"), compact(approval.PolicyReason, max(width-8, 24)))
		}
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("%d evidence reference(s) · expires %s", len(approval.EvidenceRefs), approval.ExpiresAt.Format(time.RFC3339))))
		if approval.DecisionBy != nil {
			lines = append(lines, mutedStyle.Render(fmt.Sprintf("Decision · %s:%s · %s", approval.DecisionBy.Type, approval.DecisionBy.ID, compact(approval.DecisionReason, max(width-28, 18)))))
		}
		if m.canResolveSelectedActionApproval() {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render("y approve  ·  x reject"))
		} else if approval.Status == runtime.ApprovalStatusPending && m.supportsActionApproval(kernelapi.OperationResolve) {
			lines = append(lines, "", mutedStyle.Render("This local principal is not an eligible approver."))
		}
	}
	if m.focus == focusPanel {
		lines = append(lines, "", mutedStyle.Render("↑/↓ select · r refresh · A approvals"))
	}
	return strings.Join(lines, "\n")
}

func hasRequiredArtifact(requirements []runtime.ArtifactRequirement) bool {
	for _, requirement := range requirements {
		if requirement.Required {
			return true
		}
	}
	return false
}

func humanAgentRequestStatus(status runtime.AgentRequestStatus) string {
	if status == runtime.AgentRequestStatusClarificationRequested {
		return "clarify"
	}
	return strings.ReplaceAll(string(status), "_", " ")
}

type tuiDeliveryArtifact struct {
	id      string
	version int
}

type tuiDeliveryReceipt struct {
	id             string
	deliveredAt    string
	recipientCount int
	artifacts      []tuiDeliveryArtifact
	domains        []string
}

func runDeliveryReceipt(run *runtime.AgentRun) (tuiDeliveryReceipt, bool) {
	if run == nil || run.Output == nil || run.Output["status"] != "accepted" {
		return tuiDeliveryReceipt{}, false
	}
	id, idOK := run.Output["receiptId"].(string)
	deliveredAt, deliveredOK := run.Output["deliveredAt"].(string)
	recipientCount, countOK := wholeNumber(run.Output["recipientCount"])
	references, referencesOK := run.Output["artifactRefs"].([]interface{})
	if !idOK || strings.TrimSpace(id) == "" || !deliveredOK || strings.TrimSpace(deliveredAt) == "" || !countOK || recipientCount < 1 || !referencesOK {
		return tuiDeliveryReceipt{}, false
	}
	artifacts := make([]tuiDeliveryArtifact, 0, len(references))
	for _, candidate := range references {
		reference, ok := candidate.(map[string]interface{})
		if !ok {
			return tuiDeliveryReceipt{}, false
		}
		artifactID, idOK := reference["id"].(string)
		version, versionOK := wholeNumber(reference["version"])
		if !idOK || strings.TrimSpace(artifactID) == "" || !versionOK || version < 1 {
			return tuiDeliveryReceipt{}, false
		}
		artifacts = append(artifacts, tuiDeliveryArtifact{id: artifactID, version: version})
	}
	domains := deliveryRecipientDomains(run.Context)
	return tuiDeliveryReceipt{id: id, deliveredAt: deliveredAt, recipientCount: recipientCount, artifacts: artifacts, domains: domains}, true
}

func deliveryRecipientDomains(context map[string]interface{}) []string {
	invocation, ok := context["capabilityInvocation"].(map[string]interface{})
	if !ok {
		return nil
	}
	inputs, ok := invocation["inputs"].(map[string]interface{})
	if !ok {
		return nil
	}
	recipients, ok := inputs["to"].([]interface{})
	if !ok {
		return nil
	}
	unique := map[string]struct{}{}
	for _, candidate := range recipients {
		recipient, ok := candidate.(string)
		separator := strings.LastIndex(recipient, "@")
		if !ok || separator <= 0 {
			continue
		}
		domain := strings.ToLower(strings.TrimSpace(recipient[separator+1:]))
		if domain != "" {
			unique[domain] = struct{}{}
		}
	}
	domains := make([]string, 0, len(unique))
	for domain := range unique {
		domains = append(domains, domain)
	}
	slices.Sort(domains)
	return domains
}

func wholeNumber(value interface{}) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case float64:
		converted := int(typed)
		return converted, float64(converted) == typed
	default:
		return 0, false
	}
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func (m *Model) renderActivityContent(width int) string {
	title := headerStyle.Render("Activity")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, mutedStyle.Render("Durable decisions, work, skill calls, approvals, evidence, and outcomes."), ""}
	if len(m.activity) == 0 {
		lines = append(lines, mutedStyle.Render("No activity has been recorded for this "+string(m.config.Owner.Type)+" yet."))
	} else {
		visible := max(3, min(len(m.activity), max(m.height-23, 5)))
		start := max(0, min(m.activitySelected-visible/2, len(m.activity)-visible))
		for index := start; index < min(len(m.activity), start+visible); index++ {
			item := m.activity[index]
			prefix, style := "  ", lipgloss.NewStyle().Foreground(text)
			if index == m.activitySelected {
				prefix, style = "› ", selectedStyle
			} else if item.Severity == runtime.ActivitySeverityError {
				style = lipgloss.NewStyle().Foreground(danger)
			}
			label := strings.ReplaceAll(item.EventType, "_", " ")
			line := fmt.Sprintf("%s%-10s %s · %s", prefix, compact(label, 10), compact(item.Summary, max(width-31, 18)), relativeTime(item.CreatedAt))
			lines = append(lines, style.Render(compact(line, max(width-4, 28))))
		}
	}
	if item := m.selectedActivityRecord(); item != nil {
		lines = append(lines, "", compact(item.Summary, max(width-8, 24)))
		actor := strings.TrimSpace(item.Actor.Type + ":" + item.Actor.ID)
		if actor == ":" {
			actor = "system"
		}
		lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("%s · %s · %s · actor %s", item.EventType, item.Severity, item.Visibility, actor), max(width-8, 24))))
		if m.activityExpanded {
			lines = append(lines, "", mutedStyle.Render("Subject and lineage"))
			for _, subject := range activitySubjectLines(item) {
				lines = append(lines, mutedStyle.Render(compact(subject, max(width-8, 24))))
			}
			if item.CorrelationID != "" {
				lines = append(lines, mutedStyle.Render(compact("Correlation "+item.CorrelationID, max(width-8, 24))))
			}
			if item.CausationID != "" {
				lines = append(lines, mutedStyle.Render(compact("Caused by "+item.CausationID, max(width-8, 24))))
			}
			if len(item.ConversationRefs) > 0 {
				lines = append(lines, mutedStyle.Render(compact("Conversations "+strings.Join(item.ConversationRefs, ", "), max(width-8, 24))))
			}
			if item.Payload != nil {
				lines = append(lines, "", mutedStyle.Render("Evidence · redacted canonical projection"))
				encoded, err := json.MarshalIndent(item.Payload, "", "  ")
				if err == nil {
					payloadLines := strings.Split(string(encoded), "\n")
					for _, line := range payloadLines[:min(len(payloadLines), 12)] {
						lines = append(lines, mutedStyle.Render(compact(line, max(width-8, 24))))
					}
					if len(payloadLines) > 12 {
						lines = append(lines, mutedStyle.Render(fmt.Sprintf("… %d more projected lines", len(payloadLines)-12)))
					}
				}
			}
		}
		action := "Enter expand"
		if m.activityExpanded {
			action = "Enter collapse"
		}
		if m.activityHasMore {
			action += "  ·  m older"
		}
		lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render(action))
	}
	if m.focus == focusPanel {
		lines = append(lines, mutedStyle.Render("↑/↓ select · r refresh · Tab compose"))
	}
	return strings.Join(lines, "\n")
}

func activitySubjectLines(item *runtime.ActivityProjection) []string {
	if item == nil {
		return nil
	}
	lines := make([]string, 0, 4)
	for _, subject := range []struct{ label, value string }{
		{"Agent", item.AgentID}, {"Team", item.TeamID}, {"Objective", item.ObjectiveID}, {"Initiative", item.InitiativeID},
		{"Run", item.RunID}, {"Parent Run", item.ParentRunID}, {"Turn", item.TurnID},
	} {
		if subject.value != "" {
			lines = append(lines, subject.label+" "+subject.value)
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "No additional subject identifiers")
	}
	return lines
}

func (m *Model) renderArtifactsContent(width int) string {
	title := headerStyle.Render("Artifacts & evidence")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, ""}
	if len(m.artifacts) == 0 {
		lines = append(lines, mutedStyle.Render("No durable artifacts have been registered in this workspace."))
	} else {
		visible := max(3, min(len(m.artifacts), max(m.height-22, 5)))
		start := max(0, min(m.artifactSelected-visible/2, len(m.artifacts)-visible))
		for index := start; index < min(len(m.artifacts), start+visible); index++ {
			artifact := m.artifacts[index]
			prefix := "  "
			style := lipgloss.NewStyle().Foreground(text)
			if index == m.artifactSelected {
				prefix = "› "
				style = selectedStyle
			}
			kind := artifact.Type
			if kind == "" {
				kind = artifact.MediaType
			}
			line := fmt.Sprintf("%s%-10s %s", prefix, compact(kind, 10), compact(artifact.Name, max(width-18, 20)))
			lines = append(lines, style.Render(line))
		}
	}
	if artifact := m.selectedArtifactRecord(); artifact != nil {
		lines = append(lines, "", mutedStyle.Render("Selected"), compact(artifact.Name, max(width-8, 24)))
		lines = append(lines, mutedStyle.Render(fmt.Sprintf(
			"%s · v%d · %s · %s", humanBytes(artifact.SizeBytes), artifact.Version,
			artifact.Classification, relativeTime(artifact.CreatedAt),
		)))
		producer := fmt.Sprintf("Produced by %s:%s", artifact.Provenance.Producer.Type, artifact.Provenance.Producer.ID)
		if artifact.Provenance.Owner != nil {
			producer += fmt.Sprintf(" · for %s:%s", artifact.Provenance.Owner.Type, artifact.Provenance.Owner.ID)
		}
		if artifact.Provenance.RunID != "" {
			producer += " · run " + compact(artifact.Provenance.RunID, 12)
		}
		lines = append(lines, mutedStyle.Render(producer))
		if len(artifact.Evidence) > 0 {
			lines = append(lines, mutedStyle.Render(fmt.Sprintf("%d evidence link(s)", len(artifact.Evidence))))
			if m.artifactExpanded {
				for _, evidence := range artifact.Evidence {
					confidence := ""
					if evidence.Confidence != nil {
						confidence = fmt.Sprintf(" · %.0f%%", *evidence.Confidence*100)
					}
					lines = append(lines, compact(fmt.Sprintf("  %s %s%s", evidence.Relation, evidence.TargetRef, confidence), max(width-8, 28)))
					if evidence.Summary != "" {
						lines = append(lines, mutedStyle.Render("    "+compact(evidence.Summary, max(width-12, 24))))
					}
				}
			}
		}
		actions := make([]string, 0, 2)
		if len(artifact.Evidence) > 0 {
			if m.artifactExpanded {
				actions = append(actions, "e collapse evidence")
			} else {
				actions = append(actions, "e expand evidence")
			}
		}
		if m.supportsArtifact(kernelapi.OperationDownload) {
			actions = append(actions, "d download")
		}
		if len(actions) > 0 {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render(strings.Join(actions, "  ·  ")))
		}
	}
	if m.focus == focusPanel {
		lines = append(lines, "", mutedStyle.Render("↑/↓ select · r refresh · w work · a evidence"))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderChannelsContent(width int) string {
	title := headerStyle.Render("Team channels")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, ""}
	if len(m.conversations) == 0 {
		lines = append(lines, mutedStyle.Render("No channels yet. Create one for focused Team collaboration."))
	} else {
		visible := max(1, min(len(m.conversations), 3))
		start := max(0, min(m.conversationSelected-visible/2, len(m.conversations)-visible))
		for index := start; index < min(len(m.conversations), start+visible); index++ {
			conversation := m.conversations[index]
			prefix := "  # "
			style := lipgloss.NewStyle().Foreground(text)
			if index == m.conversationSelected {
				prefix = "› # "
				style = selectedStyle
			}
			line := fmt.Sprintf("%s%-24s %d message(s)", prefix, compact(conversation.Title, 24), conversation.LastSequence)
			lines = append(lines, style.Render(compact(line, max(width-6, 28))))
		}
	}

	if conversation := m.selectedConversationRecord(); conversation != nil {
		if presence := activePresenceSummary(m.channelPresence, width); presence != "" {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render(presence))
		}
		lines = append(lines, "", mutedStyle.Render("Recent conversation"))
		if len(m.channelMessages) == 0 {
			lines = append(lines, mutedStyle.Render("No messages yet. Start with useful context or a clear question."))
		} else {
			visibleMessages := max(2, min(len(m.channelMessages), max(m.height-25, 4)))
			start := min(len(m.channelMessages), visibleMessages) - 1
			for index := start; index >= 0; index-- {
				message := m.channelMessages[index]
				if message == nil {
					continue
				}
				sender := fmt.Sprintf("%s:%s", message.Sender.Type, message.Sender.ID)
				meta := fmt.Sprintf("%s · %s · %s", compact(sender, 24), humanIntent(message.Intent), relativeTime(message.CreatedAt))
				if message.ThreadRootID != "" {
					meta += " · thread"
				}
				lines = append(lines, mutedStyle.Render(compact(meta, max(width-8, 28))))
				lines = append(lines, "  "+compact(message.Content, max(width-10, 24)))
			}
		}

		if m.supportsChannel(kernelapi.OperationAudit) && len(m.channelRounds) > 0 {
			label := fmt.Sprintf("%d participation round(s)", len(m.channelRounds))
			if m.channelAuditExpanded {
				label += " · e collapse audit"
			} else {
				label += " · e inspect audit"
			}
			lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render(label))
			if m.channelAuditExpanded {
				lines = append(lines, renderParticipationAudit(m.channelRounds, width)...)
			}
		}
	}
	if m.focus == focusPanel {
		actions := []string{"↑/↓ channel", "r refresh"}
		if m.supportsChannel(kernelapi.OperationCreate) {
			actions = append(actions, "n new")
		}
		if m.selectedConversationRecord() != nil && m.supportsChannel(kernelapi.OperationPost) {
			actions = append(actions, "m message", "Tab compose")
		}
		lines = append(lines, "", mutedStyle.Render(strings.Join(actions, " · ")))
	}
	return strings.Join(lines, "\n")
}

func activePresenceSummary(presence []*runtime.ConversationPresence, width int) string {
	active := make([]string, 0, len(presence))
	now := time.Now()
	for _, item := range presence {
		if item == nil || !item.ExpiresAt.After(now) {
			continue
		}
		label := fmt.Sprintf("%s:%s is %s", item.Participant.Type, item.Participant.ID, item.State)
		if item.Summary != "" {
			label += " — " + item.Summary
		}
		active = append(active, label)
	}
	if len(active) == 0 {
		return ""
	}
	return compact(strings.Join(active, " · "), max(width-6, 28))
}

func renderParticipationAudit(rounds []*runtime.ParticipationRoundResult, width int) []string {
	for _, result := range rounds {
		if result == nil || result.Round == nil {
			continue
		}
		round := result.Round
		lines := []string{mutedStyle.Render(fmt.Sprintf("Round %s · %s", compact(round.ID, 12), relativeTime(round.CommittedAt)))}
		for _, decision := range round.Arbitration.Decisions {
			reasons := make([]string, len(decision.Reasons))
			for index, reason := range decision.Reasons {
				reasons[index] = strings.ReplaceAll(string(reason), "_", " ")
			}
			line := fmt.Sprintf("  %s:%s · %s · %d", decision.Participant.Type, decision.Participant.ID, decision.Disposition, decision.Score)
			if len(reasons) > 0 {
				line += " · " + strings.Join(reasons, ", ")
			}
			lines = append(lines, mutedStyle.Render(compact(line, max(width-8, 28))))
		}
		return lines
	}
	return nil
}

func (m *Model) availableActions(run *runtime.AgentRun) string {
	actions := make([]string, 0, 3)
	if run.Status == runtime.AgentRunStatusPaused && m.commandAllowed(run, runtime.AgentRunCommandResume) {
		actions = append(actions, "p resume")
	} else if m.commandAllowed(run, runtime.AgentRunCommandPause) {
		actions = append(actions, "p pause")
	}
	if m.commandAllowed(run, runtime.AgentRunCommandIntervene) {
		actions = append(actions, "g guide")
	}
	if m.commandAllowed(run, runtime.AgentRunCommandCancel) {
		actions = append(actions, "x stop")
	}
	return strings.Join(actions, "  ·  ")
}

func (m *Model) renderFooter(width int) string {
	parts := make([]string, 0, 2)
	if m.err != nil {
		parts = append(parts, lipgloss.NewStyle().Foreground(danger).Render(compact(m.err.Error(), width)))
	} else if m.status != "" {
		parts = append(parts, lipgloss.NewStyle().Foreground(success).Render(compact(m.status, width)))
	}
	parts = append(parts, mutedStyle.Render("Ctrl+C quit"))
	return "\n" + strings.Join(parts, "\n")
}

func (m *Model) composerWidth() int {
	if m.width < 90 {
		return max(m.width-4, 36)
	}
	return max(40, (m.width-6)*42/100)
}

func (m *Model) runsWidth() int {
	if m.width < 90 {
		return max(m.width-4, 36)
	}
	return max(46, m.width-m.composerWidth()-6)
}

func humanStatus(status runtime.AgentRunStatus) string {
	switch status {
	case runtime.AgentRunStatusWaitingForDependency:
		return "waiting"
	case runtime.AgentRunStatusWaitingForAgent:
		return "delegated"
	case runtime.AgentRunStatusWaitingForApproval:
		return "approval"
	case runtime.AgentRunStatusWaitingForEvent:
		return "watching"
	default:
		return strings.ReplaceAll(string(status), "_", " ")
	}
}

func humanIntent(intent runtime.ConversationMessageIntent) string {
	return strings.ReplaceAll(string(intent), "_", " ")
}

func humanOwner(owner runtime.ObjectiveOwner) string {
	ownerType := string(owner.Type)
	if ownerType != "" {
		ownerType = strings.ToUpper(ownerType[:1]) + ownerType[1:]
	}
	return fmt.Sprintf("For %s %s", ownerType, owner.ID)
}

func compact(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if limit < 4 || len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

func relativeTime(value time.Time) string {
	if value.IsZero() {
		return "just now"
	}
	delta := time.Since(value)
	if delta < time.Minute {
		return "just now"
	}
	if delta < time.Hour {
		return fmt.Sprintf("%dm ago", int(delta.Minutes()))
	}
	if delta < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(delta.Hours()))
	}
	return value.Format("2006-01-02")
}

func humanBytes(size int64) string {
	const unit = int64(1024)
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= float64(unit)
		if value < float64(unit) || suffix == "TB" {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%d B", size)
}
