package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/skills.sdk/executor"
)

const (
	slackIngressNodeType  = "slack.conversation.ingress"
	slackDeliveryNodeType = "slack.conversation.deliver"
	adapterEnvelopeKey    = "_opensealConversationAdapterRequest"
	slackConnectionKey    = "SLACK_CONNECTION"
	maxSlackResponseBytes = 1 << 20
)

type slackAdapter struct {
	signingSecret string
	baseURL       string
	client        *http.Client
	now           func() time.Time
}

func newSlackAdapter(signingSecret, baseURL string, client *http.Client) *slackAdapter {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://slack.com/api"
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &slackAdapter{
		signingSecret: strings.TrimSpace(signingSecret),
		baseURL:       strings.TrimRight(baseURL, "/"),
		client:        client,
		now:           time.Now,
	}
}

type slackIngressExecutor struct{ adapter *slackAdapter }

func (e *slackIngressExecutor) Type() string { return slackIngressNodeType }

func (e *slackIngressExecutor) Execute(
	ctx context.Context,
	step *executor.StepDefinition,
	_ executor.TemplateResolver,
) (*executor.StepResult, error) {
	output, err := e.adapter.ingress(ctx, step.Config)
	if err != nil {
		return nil, err
	}
	return &executor.StepResult{Output: output}, nil
}

type slackDeliveryExecutor struct{ adapter *slackAdapter }

func (e *slackDeliveryExecutor) Type() string { return slackDeliveryNodeType }

func (e *slackDeliveryExecutor) Execute(
	ctx context.Context,
	step *executor.StepDefinition,
	_ executor.TemplateResolver,
) (*executor.StepResult, error) {
	output, err := e.adapter.delivery(ctx, step.Config)
	if err != nil {
		return nil, err
	}
	return &executor.StepResult{Output: output}, nil
}

type adapterEnvelope struct {
	Operation string                                       `json:"operation"`
	Endpoint  *openseal.ExternalConversationEndpoint       `json:"endpoint"`
	Request   *openseal.ExternalConversationIngressRequest `json:"request,omitempty"`
	Delivery  *openseal.ExternalConversationDelivery       `json:"delivery,omitempty"`
	Message   *openseal.ChannelMessage                     `json:"message,omitempty"`
}

func decodeAdapterEnvelope(config map[string]interface{}) (*adapterEnvelope, error) {
	raw, ok := config[adapterEnvelopeKey]
	if !ok {
		return nil, errors.New("conversation adapter request is required")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, errors.New("conversation adapter request is invalid")
	}
	var envelope adapterEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil || envelope.Endpoint == nil {
		return nil, errors.New("conversation adapter request is invalid")
	}
	return &envelope, nil
}

type slackEventsEnvelope struct {
	Type           string               `json:"type"`
	Challenge      string               `json:"challenge"`
	TeamID         string               `json:"team_id"`
	APIAppID       string               `json:"api_app_id"`
	EventID        string               `json:"event_id"`
	EventTime      int64                `json:"event_time"`
	Authorizations []slackAuthorization `json:"authorizations"`
	Event          slackEvent           `json:"event"`
}

type slackAuthorization struct {
	UserID string `json:"user_id"`
	TeamID string `json:"team_id"`
	IsBot  bool   `json:"is_bot"`
}

type slackEvent struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	Timestamp   string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
}

func (a *slackAdapter) ingress(_ context.Context, config map[string]interface{}) (map[string]interface{}, error) {
	envelope, err := decodeAdapterEnvelope(config)
	if err != nil || envelope.Request == nil || envelope.Operation != "ingress" {
		return nil, errors.New("Slack ingress request is invalid")
	}
	if !a.verifySlackRequest(envelope.Request) {
		return map[string]interface{}{
			"statusCode": http.StatusUnauthorized, "contentType": "text/plain",
			"body": "invalid Slack signature",
		}, nil
	}
	var payload slackEventsEnvelope
	if err := json.Unmarshal(envelope.Request.Body, &payload); err != nil {
		return map[string]interface{}{
			"statusCode": http.StatusBadRequest, "contentType": "text/plain",
			"body": "invalid Slack event",
		}, nil
	}
	if payload.Type == "url_verification" {
		if strings.TrimSpace(payload.Challenge) == "" {
			return map[string]interface{}{"statusCode": http.StatusBadRequest}, nil
		}
		body, _ := json.Marshal(map[string]string{"challenge": payload.Challenge})
		return map[string]interface{}{
			"statusCode": http.StatusOK, "contentType": "application/json", "body": string(body),
		}, nil
	}
	if payload.Type != "event_callback" || strings.TrimSpace(payload.EventID) == "" {
		return map[string]interface{}{"statusCode": http.StatusOK, "events": []interface{}{}}, nil
	}
	if !slackEndpointMatches(envelope.Endpoint, payload) {
		return map[string]interface{}{"statusCode": http.StatusOK, "events": []interface{}{}}, nil
	}
	event, accepted := normalizeSlackEvent(payload)
	if !accepted {
		return map[string]interface{}{"statusCode": http.StatusOK, "events": []interface{}{}}, nil
	}
	return map[string]interface{}{
		"statusCode": http.StatusOK, "contentType": "application/json", "body": `{"ok":true}`,
		"events": []openseal.NormalizedExternalConversationEvent{event},
	}, nil
}

func (a *slackAdapter) verifySlackRequest(request *openseal.ExternalConversationIngressRequest) bool {
	if request == nil || a.signingSecret == "" {
		return false
	}
	timestamp := firstHeader(request.Headers, "X-Slack-Request-Timestamp")
	signature := firstHeader(request.Headers, "X-Slack-Signature")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || signature == "" {
		return false
	}
	occurred := time.Unix(seconds, 0)
	if delta := a.now().UTC().Sub(occurred); delta > 5*time.Minute || delta < -5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(a.signingSecret))
	_, _ = mac.Write([]byte("v0:" + timestamp + ":"))
	_, _ = mac.Write(request.Body)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

func firstHeader(headers map[string][]string, wanted string) string {
	for name, values := range headers {
		if strings.EqualFold(name, wanted) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func slackEndpointMatches(endpoint *openseal.ExternalConversationEndpoint, payload slackEventsEnvelope) bool {
	if endpoint == nil || endpoint.Provider != "slack" {
		return false
	}
	if endpoint.Address != "" && endpoint.Address != payload.Event.Channel {
		return false
	}
	if teamID := stringConfiguration(endpoint.Configuration, "teamId"); teamID != "" && teamID != payload.TeamID {
		return false
	}
	if appID := stringConfiguration(endpoint.Configuration, "appId"); appID != "" && appID != payload.APIAppID {
		return false
	}
	return true
}

func normalizeSlackEvent(payload slackEventsEnvelope) (openseal.NormalizedExternalConversationEvent, bool) {
	source := payload.Event
	if source.Type != "message" && source.Type != "app_mention" {
		return openseal.NormalizedExternalConversationEvent{}, false
	}
	switch source.Subtype {
	case "", "bot_message":
	default:
		return openseal.NormalizedExternalConversationEvent{}, false
	}
	if strings.TrimSpace(source.Channel) == "" || strings.TrimSpace(source.Timestamp) == "" {
		return openseal.NormalizedExternalConversationEvent{}, false
	}
	botUserID := ""
	for _, authorization := range payload.Authorizations {
		if authorization.IsBot && authorization.UserID != "" {
			botUserID = authorization.UserID
			break
		}
	}
	mention := ""
	if botUserID != "" {
		mention = "<@" + botUserID + ">"
	}
	text := strings.TrimSpace(source.Text)
	mentionsEndpoint := source.Type == "app_mention" || (mention != "" && strings.Contains(text, mention))
	if mention != "" {
		text = strings.TrimSpace(strings.ReplaceAll(text, mention, ""))
	}
	threadID := strings.TrimSpace(source.ThreadTS)
	if threadID == "" {
		threadID = strings.TrimSpace(source.Timestamp)
	}
	occurredAt := slackTimestamp(source.Timestamp)
	if occurredAt.IsZero() && payload.EventTime > 0 {
		occurredAt = time.Unix(payload.EventTime, 0).UTC()
	}
	participantID := strings.TrimSpace(source.User)
	if participantID == "" {
		participantID = strings.TrimSpace(source.BotID)
	}
	eventID := "slack:message:" + payload.TeamID + ":" + source.Channel + ":" + source.Timestamp
	if strings.TrimSpace(payload.TeamID) == "" {
		eventID = "slack:event:" + payload.EventID
	}
	return openseal.NormalizedExternalConversationEvent{
		ID: eventID, Type: openseal.ConversationEventMessageReceived,
		ExternalConversationID: source.Channel, ExternalThreadID: threadID,
		ExternalMessageID: source.Timestamp, ExternalParticipantID: participantID,
		ParticipantIsBot: source.BotID != "" || source.Subtype == "bot_message",
		Text:             text, MentionsEndpoint: mentionsEndpoint, Direct: source.ChannelType == "im",
		OrderingKey: source.Channel + ":" + source.Timestamp, OccurredAt: occurredAt,
		Attributes: map[string]interface{}{
			"teamId": payload.TeamID, "appId": payload.APIAppID, "channelType": source.ChannelType,
			"providerEventId": payload.EventID,
		},
	}, true
}

func slackTimestamp(value string) time.Time {
	secondsText, fractionText, found := strings.Cut(strings.TrimSpace(value), ".")
	seconds, err := strconv.ParseInt(secondsText, 10, 64)
	if err != nil {
		return time.Time{}
	}
	nanoseconds := int64(0)
	if found {
		fractionText += strings.Repeat("0", 9)
		nanoseconds, _ = strconv.ParseInt(fractionText[:9], 10, 64)
	}
	return time.Unix(seconds, nanoseconds).UTC()
}

func stringConfiguration(config map[string]interface{}, key string) string {
	value, _ := config[key].(string)
	return strings.TrimSpace(value)
}

type slackDeliveryResponse struct {
	OK        bool           `json:"ok"`
	Error     string         `json:"error"`
	Channel   string         `json:"channel"`
	Timestamp string         `json:"ts"`
	Messages  []slackMessage `json:"messages"`
}

type slackMessage struct {
	Timestamp string        `json:"ts"`
	Metadata  slackMetadata `json:"metadata"`
}

type slackMetadata struct {
	EventType    string            `json:"event_type"`
	EventPayload map[string]string `json:"event_payload"`
}

func (a *slackAdapter) delivery(ctx context.Context, config map[string]interface{}) (map[string]interface{}, error) {
	envelope, err := decodeAdapterEnvelope(config)
	if err != nil || envelope.Delivery == nil || envelope.Message == nil ||
		(envelope.Operation != "lookup" && envelope.Operation != "deliver") {
		return nil, errors.New("Slack delivery request is invalid")
	}
	token, _ := config[slackConnectionKey].(string)
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("Slack OAuth connection is unavailable")
	}
	if envelope.Operation == "lookup" {
		return a.lookup(ctx, token, envelope)
	}
	return a.deliver(ctx, token, envelope)
}

func (a *slackAdapter) deliver(ctx context.Context, token string, envelope *adapterEnvelope) (map[string]interface{}, error) {
	body := map[string]interface{}{
		"channel": envelope.Endpoint.Address,
		"text":    envelope.Message.Content,
		"metadata": map[string]interface{}{
			"event_type":    "openseal_conversation_delivery",
			"event_payload": map[string]string{"delivery_id": envelope.Delivery.ID},
		},
	}
	if threadID := strings.TrimSpace(envelope.Delivery.ExternalThreadID); threadID != "" {
		body["thread_ts"] = threadID
	}
	response, status, retryAfter, err := a.slackJSON(ctx, token, http.MethodPost, "/chat.postMessage", nil, body)
	if err != nil {
		return retryDelivery("slack_unavailable", "Slack could not be reached.", retryAfter), nil
	}
	if status == http.StatusTooManyRequests {
		return retryDelivery("rate_limited", "Slack asked the Skill to retry later.", retryAfter), nil
	}
	if status < 200 || status >= 300 {
		if status >= 500 {
			return retryDelivery("slack_unavailable", "Slack is temporarily unavailable.", retryAfter), nil
		}
		return failedDelivery("slack_rejected", "Slack rejected the message."), nil
	}
	var result slackDeliveryResponse
	if err := json.Unmarshal(response, &result); err != nil {
		return retryDelivery("invalid_response", "Slack returned an invalid response.", 0), nil
	}
	if !result.OK {
		if transientSlackError(result.Error) {
			return retryDelivery(safeSlackError(result.Error), "Slack asked the Skill to retry the message.", retryAfter), nil
		}
		return failedDelivery(safeSlackError(result.Error), "Slack rejected the message."), nil
	}
	if strings.TrimSpace(result.Timestamp) == "" {
		return retryDelivery("missing_acknowledgement", "Slack did not acknowledge the message.", 0), nil
	}
	return map[string]interface{}{
		"outcome": "delivered", "providerMessageId": result.Timestamp,
		"summary": "Slack accepted the message.",
	}, nil
}

func (a *slackAdapter) lookup(ctx context.Context, token string, envelope *adapterEnvelope) (map[string]interface{}, error) {
	path := "/conversations.history"
	query := url.Values{"channel": {envelope.Endpoint.Address}, "limit": {"100"}, "inclusive": {"true"}}
	if threadID := strings.TrimSpace(envelope.Delivery.ExternalThreadID); threadID != "" {
		path = "/conversations.replies"
		query.Set("ts", threadID)
	}
	response, status, _, err := a.slackJSON(ctx, token, http.MethodGet, path, query, nil)
	if err != nil || status < 200 || status >= 300 {
		return map[string]interface{}{"status": "unknown"}, nil
	}
	var result slackDeliveryResponse
	if json.Unmarshal(response, &result) != nil || !result.OK {
		return map[string]interface{}{"status": "unknown"}, nil
	}
	for _, message := range result.Messages {
		if message.Metadata.EventType == "openseal_conversation_delivery" &&
			message.Metadata.EventPayload["delivery_id"] == envelope.Delivery.ID &&
			strings.TrimSpace(message.Timestamp) != "" {
			return map[string]interface{}{"status": "found", "providerMessageId": message.Timestamp}, nil
		}
	}
	return map[string]interface{}{"status": "not_found"}, nil
}

func (a *slackAdapter) slackJSON(
	ctx context.Context,
	token, method, path string,
	query url.Values,
	body interface{},
) ([]byte, int, time.Duration, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	endpoint := a.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, 0, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, 0, 0, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxSlackResponseBytes+1))
	if err != nil || len(payload) > maxSlackResponseBytes {
		return nil, response.StatusCode, 0, errors.New("Slack response is invalid")
	}
	retryAfter := time.Duration(0)
	if seconds, parseErr := strconv.Atoi(strings.TrimSpace(response.Header.Get("Retry-After"))); parseErr == nil && seconds > 0 {
		retryAfter = time.Duration(seconds) * time.Second
	}
	return payload, response.StatusCode, retryAfter, nil
}

func retryDelivery(code, summary string, retryAfter time.Duration) map[string]interface{} {
	result := map[string]interface{}{"outcome": "retry", "errorCode": code, "summary": summary}
	if retryAfter > 0 {
		result["retryAfterMs"] = retryAfter.Milliseconds()
	}
	return result
}

func failedDelivery(code, summary string) map[string]interface{} {
	return map[string]interface{}{"outcome": "failed", "errorCode": code, "summary": summary}
}

func transientSlackError(code string) bool {
	switch strings.TrimSpace(code) {
	case "ratelimited", "internal_error", "fatal_error", "request_timeout", "service_unavailable":
		return true
	default:
		return false
	}
}

func safeSlackError(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 128 || strings.ContainsAny(code, "\r\n") {
		return "slack_rejected"
	}
	return code
}
