package tui

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/google/uuid"
)

const outreachComposerTemplate = `Name:
Affiliation:
Profile:
Disclosure:
Approval:
Intent: request_feedback

Write the reviewed public message here and include the disclosure exactly.`

type outreachDraft struct {
	identity          runtime.OutreachIdentity
	approvalPolicyRef string
	intent            runtime.OutreachMessageIntent
	body              string
}

func (m *Model) loadOutreach() tea.Cmd {
	initiative := m.selectedInitiativeRecord()
	if initiative == nil || !m.supportsOutreach(kernelapi.OperationList) {
		m.outreachThreads, m.outreachObservations, m.outreachActions = nil, nil, nil
		return nil
	}
	m.loading = true
	initiativeID := initiative.ID
	scope := initiative.Scope
	monitors := append([]runtime.SourceMonitorReference(nil), initiative.SourceMonitors...)
	observationIndex := m.outreachObservationSelected
	canCreate := m.supportsOutreach(kernelapi.OperationCreate) && m.skillActionCapability.Supports(kernelapi.OperationList)
	return func() tea.Msg {
		threads, err := m.client.ListOutreachThreads(m.ctx, runtime.OutreachThreadFilter{Scope: scope, InitiativeID: initiativeID, Limit: 100})
		if err != nil {
			return outreachLoaded{initiativeID: initiativeID, err: err}
		}
		observations := make([]*runtime.SourceObservation, 0)
		if canCreate && m.supportsSourceMonitor(kernelapi.OperationListObservations) {
			for _, monitor := range monitors {
				items, listErr := m.client.ListSourceObservations(m.ctx, runtime.SourceObservationFilter{Scope: scope, InitiativeID: initiativeID, MonitorID: monitor.ID, Limit: 100})
				if listErr != nil {
					return outreachLoaded{initiativeID: initiativeID, err: listErr}
				}
				observations = append(observations, items...)
			}
			sort.SliceStable(observations, func(left, right int) bool { return observations[left].ObservedAt.After(observations[right].ObservedAt) })
		}
		actions := []capability.ModelAction(nil)
		if len(observations) > 0 {
			selected := observations[min(observationIndex, len(observations)-1)]
			if monitor := outreachMonitor(monitors, selected.MonitorID); monitor != nil {
				result, actionErr := m.client.ListAgentSkillActions(m.ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, monitor.AssignedAgentID, []string{"target", "body"}, capability.SideEffectExternal)
				if actionErr != nil {
					return outreachLoaded{initiativeID: initiativeID, err: actionErr}
				}
				if result != nil {
					actions = result.Actions
				}
			}
		}
		return outreachLoaded{initiativeID: initiativeID, threads: threads, observations: observations, actions: actions}
	}
}

func outreachMonitor(monitors []runtime.SourceMonitorReference, id string) *runtime.SourceMonitorReference {
	for index := range monitors {
		if monitors[index].ID == id {
			return &monitors[index]
		}
	}
	return nil
}

func (m *Model) canCreateOutreachDraft() bool {
	return m.supportsOutreach(kernelapi.OperationCreate) && m.selectedInitiativeRecord() != nil &&
		m.selectedOutreachObservation() != nil && m.selectedOutreachAction() != nil
}

func (m *Model) prepareOutreachComposer() {
	if !m.canCreateOutreachDraft() {
		return
	}
	m.mode = modeOutreachCreate
	m.editor.Reset()
	m.editor.SetValue(outreachComposerTemplate)
	m.editor.Placeholder = "Declare a truthful public identity, approval policy, intent, and reviewed message."
	m.focusComposerEditor()
}

func (m *Model) submitOutreachDraft() tea.Cmd {
	initiative := m.selectedInitiativeRecord()
	observation := m.selectedOutreachObservation()
	action := m.selectedOutreachAction()
	prompt := strings.TrimSpace(m.editor.Value())
	if !m.canCreateOutreachDraft() || initiative == nil || observation == nil || action == nil || m.busy {
		return nil
	}
	draft, err := parseOutreachDraft(prompt)
	if err != nil {
		m.status = err.Error()
		return nil
	}
	fingerprint := strings.Join([]string{initiative.ID, observation.ID, action.BindingID, fmt.Sprint(action.BindingRevision), prompt}, "\x00")
	if m.pendingOutreachKey == "" || m.pendingOutreachPrompt != fingerprint {
		m.pendingOutreachKey, m.pendingOutreachPrompt = uuid.NewString(), fingerprint
	}
	key := m.pendingOutreachKey
	request := kernelapi.CreateOutreachThreadRequest{
		Scope: m.config.Scope, InitiativeID: initiative.ID, SourceObservationID: observation.ID,
		ApprovalPolicyRef: draft.approvalPolicyRef, Identity: draft.identity, IdempotencyKey: key,
		Message: kernelapi.CreateOutreachMessageRequest{
			ID: uuid.NewString(), Intent: draft.intent, Body: draft.body,
			Capability: kernelapi.OutreachCapabilitySelection{
				BindingID: action.BindingID, BindingRevision: action.BindingRevision,
				SkillID: action.SkillID, SkillVersion: action.Version, Action: action.Action,
			},
		},
	}
	m.busy, m.err, m.status = true, nil, "Saving evidence-linked outreach draft…"
	return func() tea.Msg {
		thread, createErr := m.client.CreateOutreachThread(m.ctx, request, key)
		return outreachCreated{thread: thread, err: createErr}
	}
}

func parseOutreachDraft(raw string) (outreachDraft, error) {
	parts := strings.SplitN(strings.ReplaceAll(raw, "\r\n", "\n"), "\n\n", 2)
	if len(parts) != 2 {
		return outreachDraft{}, errors.New("Keep the identity headers, then a blank line before the reviewed message.")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(parts[0], "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return outreachDraft{}, errors.New("Each outreach identity header must use Name: value form.")
		}
		fields[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	draft := outreachDraft{
		identity:          runtime.OutreachIdentity{DisplayName: fields["name"], Affiliation: fields["affiliation"], ProfileRef: fields["profile"], Disclosure: fields["disclosure"]},
		approvalPolicyRef: fields["approval"], intent: runtime.OutreachMessageIntent(fields["intent"]), body: strings.TrimSpace(parts[1]),
	}
	if err := draft.identity.Validate(); err != nil {
		return outreachDraft{}, errors.New("Name, affiliation, public profile reference, and disclosure are all required.")
	}
	if draft.approvalPolicyRef == "" {
		return outreachDraft{}, errors.New("An explicit approval policy reference is required.")
	}
	switch draft.intent {
	case runtime.OutreachIntentClarify, runtime.OutreachIntentRequestFeedback, runtime.OutreachIntentAnswer, runtime.OutreachIntentFollowUp:
	default:
		return outreachDraft{}, errors.New("Intent must be clarify, request_feedback, answer, or follow_up.")
	}
	if draft.body == "" || !strings.Contains(draft.body, draft.identity.Disclosure) {
		return outreachDraft{}, errors.New("The reviewed message must include the declared disclosure exactly.")
	}
	return draft, nil
}

func (m *Model) deliverSelectedOutreachDraft() tea.Cmd {
	thread := m.selectedOutreachRecord()
	if thread == nil || !m.supportsOutreach(kernelapi.OperationDeliver) || m.busy {
		return nil
	}
	var message *runtime.OutreachMessage
	for index := range thread.Messages {
		if thread.Messages[index].Direction == runtime.OutreachMessageOutbound && thread.Messages[index].Status == runtime.OutreachMessageDraft {
			message = &thread.Messages[index]
			break
		}
	}
	if message == nil {
		m.status = "The selected thread has no deliverable draft."
		return nil
	}
	key := "tui-outreach-delivery:" + thread.ID + ":" + message.ID
	m.busy, m.err, m.status = true, nil, "Creating governed delivery Run…"
	return func() tea.Msg {
		run, err := m.client.DeliverOutreachMessage(m.ctx, thread.InitiativeID, thread.ID, message.ID, kernelapi.DeliverOutreachMessageRequest{Scope: thread.Scope, IdempotencyKey: key}, key)
		return outreachDeliveryCreated{run: run, threadID: thread.ID, err: err}
	}
}

func (m *Model) selectedOutreachRecord() *runtime.OutreachThread {
	if m.outreachSelected < 0 || m.outreachSelected >= len(m.outreachThreads) {
		return nil
	}
	return m.outreachThreads[m.outreachSelected]
}

func (m *Model) selectedOutreachObservation() *runtime.SourceObservation {
	if m.outreachObservationSelected < 0 || m.outreachObservationSelected >= len(m.outreachObservations) {
		return nil
	}
	return m.outreachObservations[m.outreachObservationSelected]
}

func (m *Model) selectedOutreachAction() *capability.ModelAction {
	if m.outreachActionSelected < 0 || m.outreachActionSelected >= len(m.outreachActions) {
		return nil
	}
	return &m.outreachActions[m.outreachActionSelected]
}

func (m *Model) restoreOutreachSelection() {
	if len(m.outreachThreads) == 0 {
		m.outreachSelected, m.selectedOutreach = 0, ""
		return
	}
	for index, thread := range m.outreachThreads {
		if thread.ID == m.selectedOutreach {
			m.outreachSelected = index
			return
		}
	}
	m.outreachSelected = min(m.outreachSelected, len(m.outreachThreads)-1)
	m.selectedOutreach = m.outreachThreads[m.outreachSelected].ID
}

func (m *Model) moveOutreachSelection(delta int) {
	if len(m.outreachThreads) == 0 {
		return
	}
	m.outreachSelected = max(0, min(len(m.outreachThreads)-1, m.outreachSelected+delta))
	m.selectedOutreach = m.outreachThreads[m.outreachSelected].ID
}

func (m *Model) moveOutreachObservation(delta int) {
	if len(m.outreachObservations) == 0 {
		return
	}
	m.outreachObservationSelected = max(0, min(len(m.outreachObservations)-1, m.outreachObservationSelected+delta))
	m.outreachActionSelected = 0
}

func (m *Model) moveOutreachAction(delta int) {
	if len(m.outreachActions) == 0 {
		return
	}
	m.outreachActionSelected = max(0, min(len(m.outreachActions)-1, m.outreachActionSelected+delta))
}

func (m *Model) renderOutreachContent(width int) string {
	title := headerStyle.Render("Governed outreach")
	if m.loading {
		title += mutedStyle.Render("  refreshing…")
	}
	lines := []string{title, ""}
	initiative := m.selectedInitiativeRecord()
	if initiative == nil {
		return strings.Join(append(lines, mutedStyle.Render("Select an Initiative first, then press O.")), "\n")
	}
	lines = append(lines, compact("Initiative · "+initiative.Title, max(width-4, 24)))
	if len(m.outreachThreads) == 0 {
		lines = append(lines, mutedStyle.Render("No outreach threads exist for this Initiative."))
	} else {
		visible := max(3, min(len(m.outreachThreads), max(m.height-24, 5)))
		start := max(0, min(m.outreachSelected-visible/2, len(m.outreachThreads)-visible))
		for index := start; index < min(len(m.outreachThreads), start+visible); index++ {
			thread := m.outreachThreads[index]
			prefix, style := "  ", lipgloss.NewStyle().Foreground(text)
			if index == m.outreachSelected {
				prefix, style = "› ", selectedStyle
			}
			messageStatus := "no messages"
			if len(thread.Messages) > 0 {
				messageStatus = strings.ReplaceAll(string(thread.Messages[len(thread.Messages)-1].Status), "_", " ")
			}
			lines = append(lines, style.Render(compact(fmt.Sprintf("%s%-9s %-18s %s", prefix, thread.Status, messageStatus, thread.TargetURI), max(width-2, 30))))
		}
	}
	if thread := m.selectedOutreachRecord(); thread != nil {
		lines = append(lines, "", mutedStyle.Render("Selected thread"))
		lines = append(lines,
			compact("Evidence · "+thread.SourceObservationID+" · "+thread.TargetURI, max(width-4, 24)),
			mutedStyle.Render(compact("Agent · "+thread.AssignedAgentID, max(width-4, 24))),
			mutedStyle.Render(compact("Source policy · "+thread.SourcePolicyRef, max(width-4, 24))),
			mutedStyle.Render(compact("Approval policy · "+thread.ApprovalPolicyRef, max(width-4, 24))),
			mutedStyle.Render(compact("Identity · "+thread.Identity.DisplayName+" · "+thread.Identity.Affiliation+" · "+thread.Identity.ProfileRef, max(width-4, 24))),
			mutedStyle.Render(compact("Disclosure · "+thread.Identity.Disclosure, max(width-4, 24))),
		)
		start := max(0, len(thread.Messages)-5)
		for index := start; index < len(thread.Messages); index++ {
			message := thread.Messages[index]
			lines = append(lines, "", compact(fmt.Sprintf("%s · %s · %s", message.Direction, message.Intent, strings.ReplaceAll(string(message.Status), "_", " ")), max(width-4, 24)))
			lines = append(lines, compact(message.Body, max(width-6, 24)))
			if message.Capability != nil {
				lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("Skill %s@%s/%s · binding %s r%d", message.Capability.SkillID, message.Capability.SkillVersion, message.Capability.Action, message.Capability.BindingID, message.Capability.BindingRevision), max(width-6, 24))))
			}
			if message.RunID != "" || message.ActionCallID != "" || message.ApprovalID != "" {
				lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("Run %s · Action %s · Approval %s", valueOrDash(message.RunID), valueOrDash(message.ActionCallID), valueOrDash(message.ApprovalID)), max(width-6, 24))))
			}
			if message.Outcome != "" {
				lines = append(lines, mutedStyle.Render(compact("Outcome · "+message.Outcome, max(width-6, 24))))
			}
			if message.Receipt != nil {
				lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("Receipt · %s/%s · %s · %s", message.Receipt.Provider, message.Receipt.ExternalID, message.Receipt.Digest, relativeTime(message.Receipt.DeliveredAt)), max(width-6, 24))))
			}
		}
	}
	if m.supportsOutreach(kernelapi.OperationCreate) {
		lines = append(lines, "", mutedStyle.Render("Draft basis"))
		if observation := m.selectedOutreachObservation(); observation != nil {
			lines = append(lines, compact(fmt.Sprintf("[%d/%d] %s · %s", m.outreachObservationSelected+1, len(m.outreachObservations), observation.Summary, observation.SourceURI), max(width-4, 24)))
		} else {
			lines = append(lines, mutedStyle.Render("No eligible source observation is available."))
		}
		if action := m.selectedOutreachAction(); action != nil {
			lines = append(lines, mutedStyle.Render(compact(fmt.Sprintf("{%d/%d} %s@%s/%s · binding %s r%d", m.outreachActionSelected+1, len(m.outreachActions), action.SkillID, action.Version, action.Action, action.BindingID, action.BindingRevision), max(width-4, 24))))
		} else if len(m.outreachObservations) > 0 {
			lines = append(lines, mutedStyle.Render("The assigned Agent has no authorized external action with target/body semantics."))
		}
	}
	actions := []string{"r refresh"}
	if m.canCreateOutreachDraft() {
		actions = append(actions, "[ ] evidence", "{ } action", "n draft")
	}
	if m.supportsOutreach(kernelapi.OperationDeliver) && selectedOutreachHasDraft(m.selectedOutreachRecord()) {
		actions = append(actions, "D create delivery Run")
	}
	lines = append(lines, "", lipgloss.NewStyle().Foreground(accentSoft).Render(strings.Join(actions, "  ·  ")))
	return strings.Join(lines, "\n")
}

func selectedOutreachHasDraft(thread *runtime.OutreachThread) bool {
	if thread == nil {
		return false
	}
	for _, message := range thread.Messages {
		if message.Direction == runtime.OutreachMessageOutbound && message.Status == runtime.OutreachMessageDraft {
			return true
		}
	}
	return false
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}
