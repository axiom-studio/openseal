package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
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
	if !m.supportsRun(kernelapi.OperationCreate) && m.mode != modeGuide && m.mode != modeChannelCreate && m.mode != modeChannelPost && m.mode != modeWorkforceAuthoring && m.mode != modeWorkforceApprove && m.mode != modeWorkforceReject && m.mode != modeWorkforceApply && m.mode != modeWorkforceRetry {
		content := headerStyle.Render("Start durable work") + "\n" +
			mutedStyle.Render("This server does not advertise work creation.") + "\n\n" +
			"You can still inspect the capabilities and evidence available in this workspace."
		return panelStyle.Width(max(width-4, 30)).Render(content)
	}
	title := "Start durable work"
	description := "Describe an outcome. OpenSeal will keep the work safe across restarts."
	owner := humanOwner(m.config.Owner)
	switch m.mode {
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
	} else if m.section == sectionObjectives {
		content = m.renderObjectivesContent(width)
	} else if m.section == sectionInitiatives {
		content = m.renderInitiativesContent(width)
	} else if m.section == sectionSkills {
		content = m.renderClawHubSkillsContent(width)
	} else if m.section == sectionChannels {
		content = m.renderChannelsContent(width)
	} else if m.section == sectionArtifacts {
		content = m.renderArtifactsContent(width)
	} else {
		content = m.renderRunsContent(width)
	}
	return panelStyle.Width(max(width-4, 30)).Render(tabs + "\n\n" + content)
}

func (m *Model) renderPanelTabs() string {
	tabs := make([]string, 0, 6)
	if m.authoringCapability.Available {
		label := "f Workforce"
		if m.section == sectionAuthoring {
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
	if m.clawHubCapability.Available {
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
	title := headerStyle.Render("Installed Skills")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, ""}
	if len(m.clawHubSkills) == 0 {
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
