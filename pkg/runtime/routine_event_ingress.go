package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// Producers of routine events from conversation and callback ingress
// (SPEC §8). Each one asks Listening before it builds anything, so a message
// on a connection no routine listens to costs a map lookup and nothing else.

const (
	// routineConversationSourceCacheMax bounds the memo of conversation
	// SourceRefs. When it is full it starts over.
	routineConversationSourceCacheMax = 4096

	routineGitHubEventPrefix = "github."
)

type routineConversationSourceKey struct {
	deploymentID, bindingID, adapterID, installationID string
}

// routineConversationSourceRefs memoizes RoutineConversationSourceRef so the
// ingress path computes no hash and allocates nothing for a connection it
// has seen before.
var routineConversationSourceRefs = struct {
	sync.RWMutex
	refs map[routineConversationSourceKey]string
}{refs: map[routineConversationSourceKey]string{}}

func routineConversationSourceRefOf(deploymentID, bindingID, adapterID, installationID string) string {
	key := routineConversationSourceKey{deploymentID: deploymentID, bindingID: bindingID, adapterID: adapterID, installationID: installationID}
	routineConversationSourceRefs.RLock()
	ref, ok := routineConversationSourceRefs.refs[key]
	routineConversationSourceRefs.RUnlock()
	if ok {
		return ref
	}
	ref = RoutineConversationSourceRef(deploymentID, bindingID, adapterID, installationID)
	routineConversationSourceRefs.Lock()
	if len(routineConversationSourceRefs.refs) >= routineConversationSourceCacheMax {
		routineConversationSourceRefs.refs = make(map[routineConversationSourceKey]string, routineConversationSourceCacheMax)
	}
	routineConversationSourceRefs.refs[key] = ref
	routineConversationSourceRefs.Unlock()
	return ref
}

// routineConversationKind maps a conversation provider to its event kind
// without allocating.
func routineConversationKind(provider string) (RoutineEventKind, bool) {
	switch {
	case strings.EqualFold(provider, "slack"):
		return RoutineEventSlackMessage, true
	case strings.EqualFold(provider, "telegram"):
		return RoutineEventTelegramMessage, true
	}
	return "", false
}

// offerConversationRoutineEvent offers one verified Slack or Telegram message
// to the routines listening on its connection. Direct messages, other
// providers and other event types are skipped (E14). It runs before the
// endpoint's message selection, which governs only the chat reply.
func offerConversationRoutineEvent(
	ctx context.Context,
	offerer RoutineEventOfferer,
	scope Scope,
	deploymentID string,
	adapter ExternalConversationAdapterReference,
	provider, installationID string,
	event *NormalizedExternalConversationEvent,
) error {
	if offerer == nil || event == nil || event.Type != capability.ConversationEventMessageReceived || event.Direct || installationID == "" {
		return nil
	}
	kind, ok := routineConversationKind(provider)
	if !ok {
		return nil
	}
	sourceRef := routineConversationSourceRefOf(deploymentID, adapter.BindingID, adapter.AdapterID, installationID)
	if !offerer.Listening(scope, kind, sourceRef) {
		return nil
	}
	routineEvent := conversationRoutineEvent(scope, kind, sourceRef, event)
	if _, err := offerer.OfferRoutineEvent(ctx, routineEvent); err != nil {
		return fmt.Errorf("offer %s to routines: %w", kind, err)
	}
	return nil
}

type conversationRoutinePayload struct {
	Text              string                          `json:"text"`
	ExternalMessageID string                          `json:"externalMessageId"`
	ExternalThreadID  string                          `json:"externalThreadId,omitempty"`
	Participant       conversationRoutineParticipant  `json:"participant"`
	Attachments       []conversationRoutineAttachment `json:"attachments,omitempty"`
}

type conversationRoutineParticipant struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Username string `json:"username,omitempty"`
	IsBot    bool   `json:"isBot"`
}

type conversationRoutineAttachment struct {
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// conversationRoutineEvent maps a normalized conversation message to a
// routine event (SPEC §3.2, §8.1).
func conversationRoutineEvent(scope Scope, kind RoutineEventKind, sourceRef string, event *NormalizedExternalConversationEvent) *RoutineEvent {
	senderName := routineEventFieldValue(event.ParticipantDisplayName)
	username := ""
	if kind == RoutineEventTelegramMessage {
		username = routineEventFieldValue(conversationAttributeString(event.Attributes, "participantUsername"))
	}
	if senderName == "" {
		senderName = username
	}
	sender := routineEventFieldValue(event.ExternalParticipantID)
	text := strings.ToValidUTF8(event.Text, "")
	fields := map[string][]string{
		"sender": {sender},
		"text":   {truncateUTF8Bytes(text, RoutineEventTextFieldMaxBytes)},
		"bot":    {strconv.FormatBool(event.ParticipantIsBot)},
	}
	if senderName != "" {
		fields["senderName"] = []string{senderName}
	}
	conversation := routineEventFieldValue(event.ExternalConversationID)
	switch kind {
	case RoutineEventSlackMessage:
		fields["channel"] = []string{conversation}
		thread := strings.TrimSpace(event.ExternalThreadID)
		fields["inThread"] = []string{strconv.FormatBool(thread != "" && thread != strings.TrimSpace(event.ExternalMessageID))}
	case RoutineEventTelegramMessage:
		fields["chat"] = []string{conversation}
		if title := routineEventFieldValue(conversationAttributeString(event.Attributes, "chatTitle")); title != "" {
			fields["chatTitle"] = []string{title}
		}
		if username != "" {
			fields["senderUsername"] = []string{username}
		}
	}
	payload := conversationRoutinePayload{
		Text: text, ExternalMessageID: event.ExternalMessageID, ExternalThreadID: event.ExternalThreadID,
		Participant: conversationRoutineParticipant{ID: event.ExternalParticipantID, Name: event.ParticipantDisplayName, Username: username, IsBot: event.ParticipantIsBot},
	}
	for _, attachment := range event.Attachments {
		payload.Attachments = append(payload.Attachments, conversationRoutineAttachment{Name: attachment.Name, MimeType: attachment.MediaType})
	}
	who := senderName
	if who == "" {
		who = sender
	}
	summary := routineEventOneLine(text)
	if summary == "" && len(event.Attachments) > 0 {
		summary = "sent an attachment"
	}
	return &RoutineEvent{
		Scope: scope, ID: routineEventIDOf(event.ID), Kind: kind, SourceRef: sourceRef, OccurredAt: event.OccurredAt.UTC(),
		Subject: truncateUTF8Bytes(conversation, RoutineEventSubjectMaxBytes),
		Summary: routineEventSummary(who + ": " + summary), Fields: fields, Payload: routineEventPayload(payload),
	}
}

func conversationAttributeString(attributes map[string]interface{}, key string) string {
	value, _ := attributes[key].(string)
	return strings.TrimSpace(value)
}

// RoutineCallbackConsumer is the "routines" callback consumer: it offers
// GitHub callback events to the routines listening on their registration.
type RoutineCallbackConsumer struct {
	offerer RoutineEventOfferer
}

func NewRoutineCallbackConsumer(offerer RoutineEventOfferer) *RoutineCallbackConsumer {
	return &RoutineCallbackConsumer{offerer: offerer}
}

var _ CallbackEventConsumer = (*RoutineCallbackConsumer)(nil)

// ConsumeCallbackEvent offers one GitHub event. Events of other providers
// are not routine events and are skipped. An offer error fails the receipt,
// so the callback worker retries it; dedupe absorbs the retry.
func (c *RoutineCallbackConsumer) ConsumeCallbackEvent(ctx context.Context, registration *CallbackRegistration, _ CallbackSubscription, event EventEnvelope) error {
	if c == nil || c.offerer == nil {
		return errors.New("routine events are not available")
	}
	if registration == nil || !strings.EqualFold(registration.Provider, "github") {
		return nil
	}
	sourceRef := RoutineCallbackSourceRef(registration.ID)
	if !c.offerer.Listening(registration.Scope, RoutineEventGitHub, sourceRef) {
		return nil
	}
	if _, err := c.offerer.OfferRoutineEvent(ctx, gitHubRoutineEvent(registration.Scope, sourceRef, event)); err != nil {
		return fmt.Errorf("offer github.event to routines: %w", err)
	}
	return nil
}

// gitHubRoutineEvent maps a GitHub callback envelope to a routine event. The
// payload shape is the GitHub skill's callback adapter output.
func gitHubRoutineEvent(scope Scope, sourceRef string, event EventEnvelope) *RoutineEvent {
	eventName := strings.TrimPrefix(strings.TrimSpace(event.Type), routineGitHubEventPrefix)
	kind, _, _ := strings.Cut(eventName, ".")
	payload := event.Payload
	repository := routineEventFieldValue(routineCallbackString(routineCallbackObject(payload, "repository"), "fullName"))
	if repository == "" {
		owner := routineCallbackString(event.Attributes, "owner")
		name := routineCallbackString(event.Attributes, "repository")
		if owner != "" && name != "" {
			repository = routineEventFieldValue(owner + "/" + name)
		}
	}
	action := routineEventFieldValue(routineCallbackString(event.Attributes, "action"))
	fields := map[string][]string{"event": {routineEventFieldValue(eventName)}}
	add := func(field, value string) {
		if value = routineEventFieldValue(value); value != "" {
			fields[field] = []string{value}
		}
	}
	add("repository", repository)
	add("action", action)
	add("sender", routineCallbackString(payload, "sender"))
	title, branch, conclusion := "", "", ""
	var labels []string
	switch kind {
	case "pull_request":
		pullRequest := routineCallbackObject(payload, "pullRequest")
		title, branch = routineCallbackString(pullRequest, "title"), routineCallbackString(pullRequest, "head")
	case "issues", "issue_comment":
		issue := routineCallbackObject(payload, "issue")
		title = routineCallbackString(issue, "title")
		labels = routineCallbackLabelNames(issue["labels"])
	case "workflow_run":
		run := routineCallbackObject(payload, "workflowRun")
		title, branch, conclusion = routineCallbackString(run, "name"), routineCallbackString(run, "branch"), routineCallbackString(run, "conclusion")
	}
	add("title", title)
	add("branch", branch)
	add("conclusion", conclusion)
	if len(labels) > 0 {
		fields["labels"] = labels
	}
	number := routineCallbackInteger(event.Attributes["number"])
	var encoded json.RawMessage
	if len(payload) > 0 {
		encoded = routineEventPayload(payload)
	}
	return &RoutineEvent{
		Scope: scope, ID: routineEventIDOf(event.ID), Kind: RoutineEventGitHub, SourceRef: sourceRef, OccurredAt: event.OccurredAt.UTC(),
		Subject: truncateUTF8Bytes(strings.ToValidUTF8(event.Subject, ""), RoutineEventSubjectMaxBytes),
		Summary: routineEventSummary(gitHubRoutineSummary(kind, action, repository, number, title, conclusion)),
		Fields:  fields, Payload: encoded,
	}
}

// gitHubRoutineSummary is a plain line like "Issue opened in acme/api: #12
// Fix login".
func gitHubRoutineSummary(kind, action, repository string, number int64, title, conclusion string) string {
	if repository == "" {
		repository = "GitHub"
	}
	verb := strings.ReplaceAll(action, "_", " ")
	item := ""
	if number > 0 {
		item = "#" + strconv.FormatInt(number, 10)
	}
	if title = routineEventOneLine(title); title != "" {
		item = strings.TrimSpace(item + " " + title)
	}
	var line string
	switch kind {
	case "issues":
		line = "Issue " + verb + " in " + repository
	case "pull_request":
		switch action {
		case "synchronize":
			verb = "updated"
		case "ready_for_review":
			verb = "marked ready for review"
		}
		line = "Pull request " + verb + " in " + repository
	case "issue_comment":
		if action == "created" {
			verb = "added"
		}
		line = "Comment " + verb + " in " + repository
	case "workflow_run":
		if action == "requested" {
			verb = "started"
		}
		line = "Workflow run " + verb + " in " + repository
		if conclusion != "" {
			item = strings.TrimSpace(item + " (" + routineEventOneLine(conclusion) + ")")
		}
	default:
		line = "GitHub " + strings.TrimSpace(kind+" "+verb) + " in " + repository
	}
	if item != "" {
		line += ": " + item
	}
	return line
}

func routineCallbackObject(values map[string]interface{}, key string) map[string]interface{} {
	value, _ := values[key].(map[string]interface{})
	return value
}

func routineCallbackString(values map[string]interface{}, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func routineCallbackInteger(value interface{}) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case json.Number:
		number, _ := typed.Int64()
		return number
	}
	return 0
}

// callbackLabelNames reads GitHub's labels list, objects with a name or
// plain strings, as at most 16 values.
func routineCallbackLabelNames(value interface{}) []string {
	items, _ := value.([]interface{})
	names := []string{}
	for _, item := range items {
		name := ""
		switch typed := item.(type) {
		case string:
			name = typed
		case map[string]interface{}:
			name, _ = typed["name"].(string)
		}
		if name = routineEventFieldValue(name); name != "" {
			names = append(names, name)
		}
		if len(names) == RoutineEventMaxFieldValues {
			break
		}
	}
	return names
}

// routineEventIDOf keeps a producer's event ID when it is a valid routine
// event ID and otherwise uses a stable digest of it.
func routineEventIDOf(id string) string {
	if ValidRoutineEventID(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// routineEventFieldValue is a filterable value: valid UTF-8, trimmed, at most
// the field value limit.
func routineEventFieldValue(value string) string {
	return truncateUTF8Bytes(strings.TrimSpace(strings.ToValidUTF8(value, "")), RoutineEventFieldValueMaxBytes)
}

func routineEventOneLine(value string) string {
	return strings.Join(strings.Fields(strings.ToValidUTF8(value, "")), " ")
}

// routineEventSummary is one line of at most 280 bytes.
func routineEventSummary(value string) string {
	value = truncateUTF8Bytes(routineEventOneLine(value), RoutineEventSummaryMaxBytes)
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return "New event"
	}
	return value
}

// routineEventPayload encodes a payload object, or nothing when it does not
// fit the payload limit; the event's fields still carry its facts.
func routineEventPayload(value interface{}) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil || len(data) > RoutineEventPayloadMaxBytes || len(data) == 0 || data[0] != '{' {
		return nil
	}
	return data
}

// PauseRoutine pauses one event routine for a system reason (needs_sign_in,
// source_removed, unverified) and drops its waiting events. A routine that
// is retired, or already paused for the same reason, is left as it is; it
// returns whether it paused.
func (d *RoutineEventDispatcher) PauseRoutine(ctx context.Context, scope Scope, activationID string, reason RoutinePausedReason) (bool, error) {
	if d == nil || d.store == nil {
		return false, errors.New("routine event dispatcher is not configured")
	}
	if !validRoutinePausedReason(reason) {
		return false, fmt.Errorf("unknown pause reason %q", reason)
	}
	if err := validateRoutineEventScopeActivation(scope, activationID); err != nil {
		return false, err
	}
	activationID = strings.TrimSpace(activationID)
	for attempt := 0; attempt < 3; attempt++ {
		activation, err := d.store.GetRunbookActivation(ctx, scope, activationID)
		if err != nil {
			return false, err
		}
		if activation == nil {
			return false, ErrRunbookActivationNotFound
		}
		if activation.Task == nil || activation.Task.Event == nil {
			return false, ErrRoutineNotEventRoutine
		}
		if activation.Status == RunbookActivationRetired ||
			(activation.Status == RunbookActivationPaused && activation.PausedReason == reason) {
			return false, nil
		}
		_, err = NewRunbookActivationService(d.store).Update(ctx, scope, activation.ID, UpdateRunbookActivationRequest{
			ExpectedRevision: activation.Revision, Status: RunbookActivationPaused, PausedReason: reason,
		})
		if errors.Is(err, ErrRunbookActivationRevision) {
			continue
		}
		if err != nil {
			return false, err
		}
		_, err = d.closeOut(ctx, scope, activation.ID, RoutineEventDroppedPaused)
		return true, err
	}
	return false, ErrRunbookActivationRevision
}

// PauseRoutinesForSource pauses every active event routine of the scope that
// listens to this kind and SourceRef, for example when a person's Google
// sign-in is revoked. It returns how many it paused.
func (d *RoutineEventDispatcher) PauseRoutinesForSource(ctx context.Context, scope Scope, kind RoutineEventKind, sourceRef string, reason RoutinePausedReason) (int, error) {
	if d == nil || d.store == nil {
		return 0, errors.New("routine event dispatcher is not configured")
	}
	activations, err := d.store.ListRoutineEventActivations(ctx, scope, kind, sourceRef)
	if err != nil {
		return 0, err
	}
	paused := 0
	for _, activation := range activations {
		if activation == nil {
			continue
		}
		done, err := d.PauseRoutine(ctx, scope, activation.ID, reason)
		if err != nil {
			return paused, err
		}
		if done {
			paused++
		}
	}
	return paused, nil
}
