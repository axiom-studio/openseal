package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/openseal"
)

func TestSlackIngressVerifiesSignatureAndNormalizesMentionedMessage(t *testing.T) {
	now := time.Unix(1_720_000_000, 0).UTC()
	adapter := newSlackAdapter("signing-secret", "", nil)
	adapter.now = func() time.Time { return now }
	body := []byte(`{
		"type":"event_callback","team_id":"T123","api_app_id":"A123","event_id":"Ev123",
		"event_time":1720000000,
		"authorizations":[{"user_id":"U-BOT","team_id":"T123","is_bot":true}],
		"event":{"type":"app_mention","user":"U123","text":"<@U-BOT> hello","channel":"C123",
			"channel_type":"channel","ts":"1720000000.123456"}
	}`)
	config := ingressConfig(now, body, &openseal.ExternalConversationEndpoint{
		ID: "endpoint", Provider: "slack", Address: "C123",
		Configuration: map[string]interface{}{"teamId": "T123", "appId": "A123"},
	})

	output, err := adapter.ingress(context.Background(), config)

	if err != nil || output["statusCode"] != http.StatusOK {
		t.Fatalf("ingress = %#v, %v", output, err)
	}
	encoded, _ := json.Marshal(output["events"])
	var events []openseal.NormalizedExternalConversationEvent
	if err := json.Unmarshal(encoded, &events); err != nil || len(events) != 1 {
		t.Fatalf("events = %s, %v", encoded, err)
	}
	event := events[0]
	if event.ID != "slack:message:T123:C123:1720000000.123456" || event.ExternalConversationID != "C123" ||
		event.ExternalThreadID != "1720000000.123456" || event.ExternalParticipantID != "U123" ||
		event.Text != "hello" || !event.MentionsEndpoint || event.ParticipantIsBot ||
		event.OccurredAt.Unix() != now.Unix() {
		t.Fatalf("normalized event = %#v", event)
	}
}

func TestSlackIngressAnswersChallengeAndRejectsReplaysOrWrongEndpoint(t *testing.T) {
	now := time.Unix(1_720_000_000, 0).UTC()
	adapter := newSlackAdapter("signing-secret", "", nil)
	adapter.now = func() time.Time { return now }
	challenge := []byte(`{"type":"url_verification","challenge":"verify-me"}`)
	output, err := adapter.ingress(context.Background(), ingressConfig(now, challenge,
		&openseal.ExternalConversationEndpoint{ID: "endpoint", Provider: "slack"}))
	if err != nil || output["statusCode"] != http.StatusOK ||
		!strings.Contains(output["body"].(string), "verify-me") {
		t.Fatalf("challenge = %#v, %v", output, err)
	}

	stale := ingressConfig(now.Add(-10*time.Minute), challenge,
		&openseal.ExternalConversationEndpoint{ID: "endpoint", Provider: "slack"})
	staleOutput, err := adapter.ingress(context.Background(), stale)
	if err != nil || staleOutput["statusCode"] != http.StatusUnauthorized {
		t.Fatalf("stale ingress = %#v, %v", staleOutput, err)
	}

	event := []byte(`{"type":"event_callback","team_id":"T123","event_id":"Ev1",
		"event":{"type":"message","user":"U1","text":"hello","channel":"C-other","ts":"1720000000.1"}}`)
	wrong := ingressConfig(now, event,
		&openseal.ExternalConversationEndpoint{ID: "endpoint", Provider: "slack", Address: "C123"})
	wrongOutput, err := adapter.ingress(context.Background(), wrong)
	if err != nil || len(wrongOutput["events"].([]interface{})) != 0 {
		t.Fatalf("wrong endpoint ingress = %#v, %v", wrongOutput, err)
	}
}

func TestSlackGatewayIngressReturnsVerifiedInstallationRoute(t *testing.T) {
	now := time.Unix(1_720_000_000, 0).UTC()
	adapter := newSlackAdapter("signing-secret", "", nil)
	adapter.now = func() time.Time { return now }
	body := []byte(`{
		"type":"event_callback","team_id":"T123","api_app_id":"A123","event_id":"Ev123",
		"event":{"type":"message","user":"U123","text":"hello","channel":"C123",
			"channel_type":"channel","ts":"1720000000.123456"}
	}`)
	config := ingressConfig(now, body, &openseal.ExternalConversationEndpoint{ID: "placeholder"})
	envelope := config[adapterEnvelopeKey].(map[string]interface{})
	envelope["operation"] = "gateway_ingress"
	delete(envelope, "endpoint")
	envelope["gateway"] = &openseal.ExternalConversationIngressGateway{
		Scope:        openseal.Scope{Kind: "platform", ID: "default"},
		DeploymentID: "slack-gateway", Provider: "slack",
	}

	output, err := adapter.ingress(context.Background(), config)

	if err != nil || output["statusCode"] != http.StatusOK {
		t.Fatalf("gateway ingress = %#v, %v", output, err)
	}
	encoded, _ := json.Marshal(output["events"])
	var events []openseal.ExternalConversationGatewayEvent
	if err := json.Unmarshal(encoded, &events); err != nil || len(events) != 1 ||
		events[0].InstallationID != "T123" || events[0].ApplicationID != "A123" ||
		events[0].Address != "C123" || events[0].Event.Text != "hello" {
		t.Fatalf("gateway events = %s, %v", encoded, err)
	}
}

func TestSlackDeliveryUsesOAuthMetadataAndAcknowledgementLookup(t *testing.T) {
	var posted map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer xoxb-connection-token" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/chat.postMessage":
			if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
				t.Error(err)
			}
			_, _ = io.WriteString(response, `{"ok":true,"channel":"C123","ts":"1720000001.123"}`)
		case "/conversations.replies":
			if request.URL.Query().Get("channel") != "C123" || request.URL.Query().Get("ts") != "1720000000.123" {
				t.Errorf("lookup query = %s", request.URL.RawQuery)
			}
			_, _ = io.WriteString(response, `{"ok":true,"messages":[
				{"ts":"1720000001.123","metadata":{"event_type":"openseal_conversation_delivery",
				"event_payload":{"delivery_id":"delivery-1"}}}]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	adapter := newSlackAdapter("signing-secret", server.URL, server.Client())
	config := deliveryConfig("deliver")

	delivered, err := adapter.delivery(context.Background(), config)

	if err != nil || delivered["outcome"] != "delivered" ||
		delivered["providerMessageId"] != "1720000001.123" {
		t.Fatalf("delivery = %#v, %v", delivered, err)
	}
	if posted["channel"] != "C123" || posted["text"] != "Agent reply" ||
		posted["thread_ts"] != "1720000000.123" {
		t.Fatalf("Slack post = %#v", posted)
	}
	metadata, _ := posted["metadata"].(map[string]interface{})
	payload, _ := metadata["event_payload"].(map[string]interface{})
	if metadata["event_type"] != "openseal_conversation_delivery" || payload["delivery_id"] != "delivery-1" {
		t.Fatalf("Slack metadata = %#v", metadata)
	}

	config[adapterEnvelopeKey].(map[string]interface{})["operation"] = "lookup"
	acknowledgement, err := adapter.delivery(context.Background(), config)
	if err != nil || acknowledgement["status"] != "found" ||
		acknowledgement["providerMessageId"] != "1720000001.123" {
		t.Fatalf("acknowledgement = %#v, %v", acknowledgement, err)
	}
}

func TestSlackDeliveryPreservesRetryAfterWithoutLeakingProviderBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Retry-After", "7")
		response.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(response, `{"ok":false,"error":"ratelimited","secret":"do-not-project"}`)
	}))
	defer server.Close()
	adapter := newSlackAdapter("", server.URL, server.Client())

	output, err := adapter.delivery(context.Background(), deliveryConfig("deliver"))

	if err != nil || output["outcome"] != "retry" || output["errorCode"] != "rate_limited" ||
		output["retryAfterMs"] != int64(7000) {
		t.Fatalf("retry = %#v, %v", output, err)
	}
	encoded, _ := json.Marshal(output)
	if strings.Contains(string(encoded), "do-not-project") {
		t.Fatalf("provider response leaked: %s", encoded)
	}
}

func ingressConfig(
	timestamp time.Time,
	body []byte,
	endpoint *openseal.ExternalConversationEndpoint,
) map[string]interface{} {
	return map[string]interface{}{
		adapterEnvelopeKey: map[string]interface{}{
			"operation": "ingress", "endpoint": endpoint,
			"request": &openseal.ExternalConversationIngressRequest{
				Scope: openseal.Scope{Kind: "tenant", ID: "1"}, EndpointID: endpoint.ID,
				Method: http.MethodPost,
				Headers: map[string][]string{
					"X-Slack-Request-Timestamp": {strconv.FormatInt(timestamp.Unix(), 10)},
					"X-Slack-Signature":         {signedSlackRequest("signing-secret", timestamp.Unix(), body)},
				},
				Body: body,
			},
		},
	}
}

func deliveryConfig(operation string) map[string]interface{} {
	now := time.Now().UTC()
	return map[string]interface{}{
		slackConnectionKey: "xoxb-connection-token",
		adapterEnvelopeKey: map[string]interface{}{
			"operation": operation,
			"endpoint": &openseal.ExternalConversationEndpoint{
				ID: "endpoint", Provider: "slack", Address: "C123",
			},
			"delivery": &openseal.ExternalConversationDelivery{
				ID: "delivery-1", ExternalThreadID: "1720000000.123",
			},
			"message": &openseal.ChannelMessage{
				ID: "message-1", Content: "Agent reply", CreatedAt: now,
			},
		},
	}
}

func signedSlackRequest(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "v0:%d:", timestamp)
	_, _ = mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}
