package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func conversationGatewayCapabilityFixture() kernelapi.Capability {
	return kernelapi.ConversationGatewaysCapability(true, []kernelapi.ConversationGatewayAdapterChoice{{
		ID: "slack-primary", DisplayName: "Primary Slack workspace", DeploymentID: "research-agent", Provider: "slack",
		SkillID: "openseal.slack-conversations", SkillVersion: "1.2.0", SourceIdentity: "catalog:trusted-slack",
		BindingID: "slack-binding", BindingRevision: 7, AdapterID: "events",
	}})
}

func conversationGatewayRecordFixture() *runtime.ExternalConversationGatewayRegistration {
	now := time.Now().UTC()
	return &runtime.ExternalConversationGatewayRegistration{
		ID: "gateway-one", IngressRoute: "opaque-public-route", Name: "Product Slack", Status: runtime.ExternalConversationGatewayPaused,
		Revision: 4, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		Gateway: runtime.ExternalConversationIngressGateway{
			Scope: runtime.Scope{Kind: "local", ID: "default"}, DeploymentID: "research-agent", Provider: "slack",
			Adapter: runtime.ExternalConversationAdapterReference{SkillID: "openseal.slack-conversations", SkillVersion: "1.2.0", SourceIdentity: "catalog:trusted-slack", BindingID: "slack-binding", BindingRevision: 7, AdapterID: "events"},
		},
		Lifecycle: []runtime.ExternalConversationGatewayLifecycleEntry{{Revision: 4, Action: runtime.ExternalConversationGatewayCreated, Actor: runtime.ActivityActor{Type: "user", ID: "operator"}, Reason: "forward-ported connection", At: now}},
	}
}

func TestConversationIntegrationUsesTypedChoiceAndCreatesPaused(t *testing.T) {
	fake := &fakeKernelClient{}
	model := newModelWithClient(t, fake)
	model.ready = true
	model.section = sectionIntegrations
	model.conversationGatewayCapability = conversationGatewayCapabilityFixture()
	model.prepareConversationGatewayCreation()
	model.editor.SetValue("name: Customer feedback Slack\nreason: receive reviewed customer feedback events")

	message := model.submitConversationGatewayCreation()()
	if _, ok := message.(conversationGatewayChanged); !ok {
		t.Fatalf("message = %T", message)
	}
	if len(fake.conversationGatewayCreates) != 1 {
		t.Fatalf("creates = %#v", fake.conversationGatewayCreates)
	}
	request := fake.conversationGatewayCreates[0]
	if request.Status != runtime.ExternalConversationGatewayPaused || request.Name != "Customer feedback Slack" || request.Reason != "receive reviewed customer feedback events" ||
		request.Gateway.DeploymentID != "research-agent" || request.Gateway.Adapter.BindingID != "slack-binding" || request.Gateway.Adapter.BindingRevision != 7 || request.Gateway.Adapter.SourceIdentity != "catalog:trusted-slack" {
		t.Fatalf("create request = %#v", request)
	}
	view := model.renderConversationGatewaysContent(100)
	if !strings.Contains(view, "Primary Slack workspace") || strings.Contains(view, "slack-binding") || strings.Contains(view, "catalog:trusted-slack") {
		t.Fatalf("typed connection was not rendered safely:\n%s", view)
	}
}

func TestConversationIntegrationLifecycleUsesExactRevisionAndReason(t *testing.T) {
	item := conversationGatewayRecordFixture()
	fake := &fakeKernelClient{conversationGateways: []*runtime.ExternalConversationGatewayRegistration{item}}
	model := newModelWithClient(t, fake)
	model.ready = true
	model.section = sectionIntegrations
	model.conversationGatewayCapability = conversationGatewayCapabilityFixture()
	model.conversationGateways = fake.conversationGateways
	model.selectedConversationGateway = item.ID
	model.mode = modeIntegrationActivate
	model.editor.SetValue("provider callback was verified")

	message := model.submitConversationGatewayLifecycle(runtime.ExternalConversationGatewayActive, "activated")()
	if _, ok := message.(conversationGatewayChanged); !ok || len(fake.conversationGatewayUpdates) != 1 {
		t.Fatalf("message=%T updates=%#v", message, fake.conversationGatewayUpdates)
	}
	request := fake.conversationGatewayUpdates[0]
	if request.ExpectedRevision != 4 || request.Status == nil || *request.Status != runtime.ExternalConversationGatewayActive || request.Reason != "provider callback was verified" {
		t.Fatalf("update request = %#v", request)
	}
}

func TestConversationIntegrationRejectsSecretFieldsAndUnconfirmedRetirement(t *testing.T) {
	if _, _, err := parseConversationGatewayCreation("name: Slack\ntoken: do-not-store\nreason: test"); err == nil || !strings.Contains(err.Error(), "secrets") {
		t.Fatalf("secret field was accepted: %v", err)
	}
	if _, err := parseConversationGatewayLifecycleReason(modeIntegrationRetire, "retire\nno longer needed"); err == nil || !strings.Contains(err.Error(), "RETIRE") {
		t.Fatalf("unconfirmed retirement was accepted: %v", err)
	}
}

func TestConversationIntegrationFailsClosedOnOlderCapability(t *testing.T) {
	fake := &fakeKernelClient{}
	model := newModelWithClient(t, fake)
	old := kernelapi.Capability{ID: kernelapi.ConversationGatewaysCapabilityID, Version: "1", Available: true, Operations: []string{kernelapi.OperationList, kernelapi.OperationCreate, kernelapi.OperationUpdate}}
	model.Update(capabilitiesLoaded{document: kernelapi.NewCapabilityDocument(old)})
	if model.conversationGatewayCapability.Available || strings.Contains(model.renderPanelTabs(), "Integrations") || model.canCreateConversationGateway() {
		t.Fatal("conversation-gateways/v1 remained visible or operable")
	}
}
