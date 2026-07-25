package runtime

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type externalConversationIngressHostStub struct {
	request ExternalConversationIngressHostRequest
	result  *ExternalConversationIngressHostResult
}

func (h *externalConversationIngressHostStub) NormalizeExternalConversation(
	_ context.Context,
	request ExternalConversationIngressHostRequest,
) (*ExternalConversationIngressHostResult, error) {
	h.request = request
	return h.result, nil
}

func TestExternalConversationIngressUsesExactSkillAdapterAndPersistsVerifiedEvents(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	now := time.Now().UTC()
	host := &externalConversationIngressHostStub{result: &ExternalConversationIngressHostResult{
		StatusCode: http.StatusOK, ContentType: "application/json", Body: []byte(`{"ok":true}`),
		Events: []NormalizedExternalConversationEvent{{
			ID: "Ev-ingress", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "C123", ExternalMessageID: "171.001",
			ExternalParticipantID: "U123", Text: "Hello", OrderingKey: "C123:171.001", OccurredAt: now,
		}},
	}}
	service := NewExternalConversationTransportService(store, catalog)
	request := ExternalConversationIngressRequest{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Method: http.MethodPost,
		Headers: map[string][]string{
			"X-Slack-Signature": {"v0=signature"}, "X-Slack-Request-Timestamp": {"1710000000"},
		},
		Body: []byte(`{"type":"event_callback"}`),
	}

	first, err := service.NormalizeExternalConversationIngress(ctx, request, host)

	if err != nil || len(first.Received) != 1 || !first.Received[0].Accepted || first.Received[0].Replayed {
		t.Fatalf("first ingress = %#v, %v", first, err)
	}
	if host.request.Endpoint.ID != endpoint.ID || host.request.Adapter.Adapter.Provider != "slack" ||
		host.request.Adapter.Binding.ID != endpoint.Adapter.BindingID {
		t.Fatalf("exact ingress adapter = %#v", host.request)
	}
	replayed, err := service.NormalizeExternalConversationIngress(ctx, request, host)
	if err != nil || len(replayed.Received) != 1 || !replayed.Received[0].Replayed ||
		replayed.Received[0].Item.ID != first.Received[0].Item.ID {
		t.Fatalf("replayed ingress = %#v, %v", replayed, err)
	}
	items, err := store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Limit: 10,
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("durable inbox = %#v, %v", items, err)
	}
}

func TestExternalConversationIngressRejectsSensitiveForwardedHeaders(t *testing.T) {
	request := &ExternalConversationIngressRequest{
		Scope: Scope{Kind: "tenant", ID: "one"}, EndpointID: "endpoint", Method: http.MethodPost,
		Headers: map[string][]string{"Authorization": {"Bearer secret"}}, Body: []byte(`{}`),
	}
	if err := request.Validate(); err == nil {
		t.Fatal("provider ingress accepted a forwarded authorization header")
	}
}
