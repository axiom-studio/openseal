package externalconversationtest

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
)

// IngressHostStub records the ingress request and answers it with Result.
type IngressHostStub struct {
	Request externalconversation.ExternalConversationIngressHostRequest
	Result  *externalconversation.ExternalConversationIngressHostResult
}

// GatewayHostStub records the gateway request and answers it with Result.
type GatewayHostStub struct {
	Request externalconversation.ExternalConversationGatewayHostRequest
	Result  *externalconversation.ExternalConversationGatewayHostResult
}

// CreateActiveGateway creates the gateway paused and then activates it, as
// an operator does after reviewing the provider route.
func CreateActiveGateway(t *testing.T, ctx context.Context, service *externalconversation.ExternalConversationGatewayService, request externalconversation.CreateExternalConversationGatewayRequest) *models.ExternalConversationGatewayRegistration {
	t.Helper()
	request.Status = models.ExternalConversationGatewayPaused
	created, err := service.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	active := models.ExternalConversationGatewayActive
	activated, err := service.Update(ctx, request.Gateway.Scope, created.ID, externalconversation.UpdateExternalConversationGatewayRequest{
		ExpectedRevision: created.Revision, Status: &active,
		Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "activate reviewed provider route",
	})
	if err != nil {
		t.Fatal(err)
	}
	return activated
}

func (h *GatewayHostStub) NormalizeExternalConversationGateway(
	_ context.Context,
	request externalconversation.ExternalConversationGatewayHostRequest,
) (*externalconversation.ExternalConversationGatewayHostResult, error) {
	h.Request = request
	return h.Result, nil
}

func (h *IngressHostStub) NormalizeExternalConversation(
	_ context.Context,
	request externalconversation.ExternalConversationIngressHostRequest,
) (*externalconversation.ExternalConversationIngressHostResult, error) {
	h.Request = request
	return h.Result, nil
}
