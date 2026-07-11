package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
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
	if m.mode == modeWorkforceAuthoring && !m.supportsAuthoring(kernelapi.OperationCompile) {
		return m.renderUnavailableComposer(width, "Create Agents and Teams", "This server does not advertise workforce compilation.")
	}
	if m.mode == modeObjectiveCreate && !m.supportsObjective(kernelapi.OperationCreate) {
		return m.renderUnavailableComposer(width, "Add an objective", "This server does not advertise objective creation.")
	}
	if m.mode == modeObjectiveEdit && !m.supportsObjective(kernelapi.OperationUpdate) {
		return m.renderUnavailableComposer(width, "Amend objective", "This server does not advertise objective updates.")
	}
	if m.mode == modeChannelCreate && !m.supportsChannel(kernelapi.OperationCreate) {
		return m.renderUnavailableComposer(width, "Create a Team channel", "This server does not advertise channel creation.")
	}
	if m.mode == modeChannelPost && !m.supportsChannel(kernelapi.OperationPost) {
		return m.renderUnavailableComposer(width, "Message the Team", "This server does not advertise channel messaging.")
	}
	if !m.supportsRun(kernelapi.OperationCreate) && m.mode != modeGuide && m.mode != modeChannelCreate && m.mode != modeChannelPost && m.mode != modeWorkforceAuthoring {
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
		title = "Create Agents and Teams"
		description = "Describe outcomes, roles, boundaries, and collaboration. Review the exact candidate before activation."
		owner = "Preview only · compilation never activates state"
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
	tabs := make([]string, 0, 5)
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
		return title + "\n\n" + mutedStyle.Render("Compiling a verified Agent and Team preview…")
	}
	result := m.authoringResult
	if result == nil {
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
	if teamPurpose != "" {
		lines = append(lines, mutedStyle.Render(compact(teamPurpose, max(width-8, 24))))
	}
	lines = append(lines, mutedStyle.Render(fmt.Sprintf("%d Agents · %d roles · %d Team objectives", len(result.Candidate.Agents), roles, objectives)), "")
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
	lines = append(lines, "", mutedStyle.Render("Nothing is active. Tab to revise the prompt."))
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
