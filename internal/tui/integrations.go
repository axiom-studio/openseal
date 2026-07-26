package tui

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type conversationGatewaysLoaded struct {
	items []*runtime.ExternalConversationGatewayRegistration
	err   error
}

type conversationGatewayChanged struct {
	gateway *runtime.ExternalConversationGatewayRegistration
	action  string
	err     error
}

func isConversationGatewayMode(mode editorMode) bool {
	return mode == modeIntegrationCreate || mode == modeIntegrationActivate || mode == modeIntegrationPause || mode == modeIntegrationRetire
}

func mapConversationGatewayModeOperation(mode editorMode) string {
	if mode == modeIntegrationCreate {
		return kernelapi.OperationCreate
	}
	return kernelapi.OperationUpdate
}

func conversationGatewayClient(kernelClient client.KernelClient) client.ExternalConversationGatewayClient {
	value, _ := kernelClient.(client.ExternalConversationGatewayClient)
	return value
}

func (m *Model) supportsConversationGateway(operation string) bool {
	return m.ready && m.conversationGatewayClient != nil && m.conversationGatewayCapability.Supports(operation)
}

func (m *Model) conversationGatewayChoices() []kernelapi.ConversationGatewayAdapterChoice {
	if m.conversationGatewayCapability.Context == nil {
		return nil
	}
	choices := append([]kernelapi.ConversationGatewayAdapterChoice(nil), m.conversationGatewayCapability.Context.ConversationGatewayAdapters...)
	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].DisplayName != choices[j].DisplayName {
			return choices[i].DisplayName < choices[j].DisplayName
		}
		return choices[i].ID < choices[j].ID
	})
	return choices
}

func (m *Model) canCreateConversationGateway() bool {
	return m.supportsConversationGateway(kernelapi.OperationCreate) && len(m.conversationGatewayChoices()) > 0
}

func (m *Model) loadConversationGateways() tea.Cmd {
	if !m.supportsConversationGateway(kernelapi.OperationList) {
		return nil
	}
	client, ctx, scope := m.conversationGatewayClient, m.ctx, m.config.Scope
	return func() tea.Msg {
		items, err := client.ListExternalConversationGateways(ctx, runtime.ExternalConversationGatewayFilter{Scope: scope, Limit: 100})
		return conversationGatewaysLoaded{items: items, err: err}
	}
}

func (m *Model) selectedConversationGatewayRecord() *runtime.ExternalConversationGatewayRegistration {
	if m.conversationGatewaySelected < 0 || m.conversationGatewaySelected >= len(m.conversationGateways) {
		return nil
	}
	return m.conversationGateways[m.conversationGatewaySelected]
}

func (m *Model) selectedConversationGatewayChoice() *kernelapi.ConversationGatewayAdapterChoice {
	choices := m.conversationGatewayChoices()
	if m.conversationGatewayAdapterSelected < 0 || m.conversationGatewayAdapterSelected >= len(choices) {
		return nil
	}
	return &choices[m.conversationGatewayAdapterSelected]
}

func (m *Model) moveConversationGatewaySelection(delta int) {
	if len(m.conversationGateways) == 0 {
		return
	}
	m.conversationGatewaySelected = max(0, min(len(m.conversationGateways)-1, m.conversationGatewaySelected+delta))
	m.selectedConversationGateway = m.conversationGateways[m.conversationGatewaySelected].ID
}

func (m *Model) restoreConversationGatewaySelection() {
	if len(m.conversationGateways) == 0 {
		m.conversationGatewaySelected = 0
		m.selectedConversationGateway = ""
		return
	}
	for index, item := range m.conversationGateways {
		if item != nil && item.ID == m.selectedConversationGateway {
			m.conversationGatewaySelected = index
			return
		}
	}
	m.conversationGatewaySelected = min(m.conversationGatewaySelected, len(m.conversationGateways)-1)
	m.selectedConversationGateway = m.conversationGateways[m.conversationGatewaySelected].ID
}

func (m *Model) moveConversationGatewayAdapterChoice(delta int) {
	choices := m.conversationGatewayChoices()
	if len(choices) == 0 {
		return
	}
	m.conversationGatewayAdapterSelected = max(0, min(len(choices)-1, m.conversationGatewayAdapterSelected+delta))
}

func (m *Model) prepareConversationGatewayCreation() {
	if !m.canCreateConversationGateway() {
		m.status = "Install and authorize a conversation Skill before adding an integration."
		return
	}
	m.mode = modeIntegrationCreate
	m.editor.Reset()
	m.editor.SetValue("name: Team messages\nreason: connect reviewed provider message routing")
	m.editor.Placeholder = "name: Team messages\nreason: why this connection is needed"
	m.focusComposerEditor()
}

func parseConversationGatewayCreation(value string) (string, string, error) {
	fields := map[string]string{}
	for _, raw := range strings.Split(value, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return "", "", errors.New("use name: and reason: fields")
		}
		key, fieldValue := strings.ToLower(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])
		if strings.Contains(key, "secret") || strings.Contains(key, "token") || strings.Contains(key, "credential") || strings.Contains(key, "password") {
			return "", "", errors.New("credentials and secrets never belong in integration setup")
		}
		if key != "name" && key != "reason" {
			return "", "", fmt.Errorf("unknown integration field %q; use only name and reason", key)
		}
		if _, exists := fields[key]; exists {
			return "", "", fmt.Errorf("integration field %q was provided more than once", key)
		}
		fields[key] = fieldValue
	}
	name, reason := fields["name"], fields["reason"]
	if name == "" || reason == "" {
		return "", "", errors.New("a connection name and audit reason are required")
	}
	if len(name) > 160 || len(reason) > 2_000 {
		return "", "", errors.New("the connection name or reason is too long")
	}
	return name, reason, nil
}

func (m *Model) submitConversationGatewayCreation() tea.Cmd {
	choice := m.selectedConversationGatewayChoice()
	if choice == nil || !m.canCreateConversationGateway() {
		m.err = errors.New("select an authorized conversation connection")
		return nil
	}
	name, reason, err := parseConversationGatewayCreation(m.editor.Value())
	if err != nil {
		m.err, m.status = err, "Review the connection details and try again."
		return nil
	}
	request := kernelapi.CreateExternalConversationGatewayRequest{
		Name: name,
		Gateway: runtime.ExternalConversationIngressGateway{
			Scope: m.config.Scope, DeploymentID: choice.DeploymentID, Provider: choice.Provider,
			Adapter: runtime.ExternalConversationAdapterReference{
				SkillID: choice.SkillID, SkillVersion: choice.SkillVersion, SourceIdentity: choice.SourceIdentity,
				BindingID: choice.BindingID, BindingRevision: choice.BindingRevision, AdapterID: choice.AdapterID,
			},
		},
		Status: runtime.ExternalConversationGatewayPaused,
		Reason: reason,
	}
	m.busy, m.err, m.status = true, nil, "Creating the reviewed connection in a paused state…"
	client, ctx := m.conversationGatewayClient, m.ctx
	return func() tea.Msg {
		gateway, createErr := client.CreateExternalConversationGateway(ctx, request)
		return conversationGatewayChanged{gateway: gateway, action: "created", err: createErr}
	}
}

func (m *Model) prepareConversationGatewayLifecycle(mode editorMode) {
	item := m.selectedConversationGatewayRecord()
	if item == nil || item.Status == runtime.ExternalConversationGatewayRetired || !m.supportsConversationGateway(kernelapi.OperationUpdate) {
		return
	}
	m.mode = mode
	m.editor.Reset()
	switch mode {
	case modeIntegrationActivate:
		m.editor.Placeholder = "Why is this connection ready to receive provider events?"
	case modeIntegrationPause:
		m.editor.Placeholder = "Why should provider event routing pause?"
	case modeIntegrationRetire:
		m.editor.Placeholder = "RETIRE\nWhy should this connection be permanently retired?"
	}
	m.focusComposerEditor()
}

func parseConversationGatewayLifecycleReason(mode editorMode, value string) (string, error) {
	value = strings.TrimSpace(value)
	if mode == modeIntegrationRetire {
		parts := strings.SplitN(value, "\n", 2)
		if strings.TrimSpace(parts[0]) != "RETIRE" || len(parts) != 2 {
			return "", errors.New("type RETIRE on the first line, followed by the audit reason")
		}
		value = strings.TrimSpace(parts[1])
	}
	if value == "" || len(value) > 2_000 {
		return "", errors.New("a concise audit reason is required")
	}
	return value, nil
}

func (m *Model) submitConversationGatewayLifecycle(status runtime.ExternalConversationGatewayStatus, action string) tea.Cmd {
	item := m.selectedConversationGatewayRecord()
	if item == nil || !m.supportsConversationGateway(kernelapi.OperationUpdate) {
		m.err = errors.New("select a manageable connection")
		return nil
	}
	reason, err := parseConversationGatewayLifecycleReason(m.mode, m.editor.Value())
	if err != nil {
		m.err, m.status = err, "Record the required audit reason and try again."
		return nil
	}
	request := kernelapi.UpdateExternalConversationGatewayRequest{ExpectedRevision: item.Revision, Status: &status, Reason: reason}
	m.busy, m.err, m.status = true, nil, fmt.Sprintf("%s connection…", strings.Title(action))
	client, ctx, scope, id := m.conversationGatewayClient, m.ctx, m.config.Scope, item.ID
	return func() tea.Msg {
		gateway, updateErr := client.UpdateExternalConversationGateway(ctx, scope, id, request)
		return conversationGatewayChanged{gateway: gateway, action: action, err: updateErr}
	}
}

func (m *Model) renderConversationGatewaysContent(width int) string {
	lines := []string{headerStyle.Render("Message integrations"), mutedStyle.Render("Connect verified provider events to authorized Agents without exposing credentials."), ""}
	choices := m.conversationGatewayChoices()
	if m.supportsConversationGateway(kernelapi.OperationCreate) {
		lines = append(lines, headerStyle.Render("Available connection"))
		if len(choices) == 0 {
			lines = append(lines, mutedStyle.Render("Install and authorize a conversation Skill on an Agent first."))
		} else {
			for index, choice := range choices {
				prefix, style := "  ", mutedStyle
				if index == m.conversationGatewayAdapterSelected {
					prefix, style = "› ", selectedStyle
				}
				lines = append(lines, style.Render(compact(prefix+choice.DisplayName+" · "+choice.Provider, max(width-8, 24))))
			}
			lines = append(lines, mutedStyle.Render("[ ] choose · n add paused"))
		}
		lines = append(lines, "")
	}
	lines = append(lines, headerStyle.Render("Connections"))
	if len(m.conversationGateways) == 0 {
		lines = append(lines, mutedStyle.Render("No message integrations are configured in this scope."))
		return strings.Join(lines, "\n")
	}
	for index, item := range m.conversationGateways {
		prefix, style := "  ", mutedStyle
		if index == m.conversationGatewaySelected {
			prefix, style = "› ", selectedStyle
		}
		lines = append(lines, style.Render(compact(fmt.Sprintf("%s%s · %s · %s · revision %d", prefix, item.Name, item.Gateway.Provider, item.Status, item.Revision), max(width-8, 24))))
	}
	item := m.selectedConversationGatewayRecord()
	if item == nil {
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "", headerStyle.Render(item.Name),
		fmt.Sprintf("Provider · %s", item.Gateway.Provider),
		fmt.Sprintf("Public route ID · %s", item.IngressRoute),
		mutedStyle.Render(fmt.Sprintf("Updated %s · optimistic revision %d", relativeTime(item.UpdatedAt), item.Revision)))
	if len(item.Lifecycle) > 0 {
		lines = append(lines, "", headerStyle.Render("Recent lifecycle"))
		start := max(0, len(item.Lifecycle)-3)
		for _, entry := range item.Lifecycle[start:] {
			lines = append(lines, compact(fmt.Sprintf("%s · %s · %s", entry.Action, entry.Actor.Type, entry.Reason), max(width-8, 24)))
		}
	}
	if item.Status != runtime.ExternalConversationGatewayRetired && m.supportsConversationGateway(kernelapi.OperationUpdate) {
		verb := "activate"
		if item.Status == runtime.ExternalConversationGatewayActive {
			verb = "pause"
		}
		lines = append(lines, "", lipgloss.NewStyle().Foreground(success).Render("p "+verb)+"  ·  "+lipgloss.NewStyle().Foreground(danger).Render("x retire")+"  ·  r refresh")
	}
	return strings.Join(lines, "\n")
}
