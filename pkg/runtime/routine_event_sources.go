package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	RunbookActionEventSources  = "event_sources"
	RunbookActionSendTestEvent = "send_test_event"

	// routineCallbackConsumer is the callback consumer whose subscriptions feed
	// routines. event_sources lists only registrations that have one.
	routineCallbackConsumer = "routines"

	routineEventSourcePageSize = 100
)

var (
	ErrRoutineEventsUnavailable = errors.New("event routines are not available here")
	ErrRoutineHooksUnavailable  = errors.New("webhooks are not available here")
)

// RoutineEventSourceChannel is one conversation of a Slack or Telegram
// connection that the umbrella has an active endpoint in. Only these can be
// named in a routine's channel or chat condition (E14).
type RoutineEventSourceChannel struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// RoutineEventSourceView is one event_sources entry: the host option plus the
// endpoint channels of a Slack or Telegram connection.
type RoutineEventSourceView struct {
	RoutineEventSourceOption
	Channels []RoutineEventSourceChannel `json:"channels,omitempty"`
}

// routineEventActions is what the event routine actions need from the host.
// The zero value means event routines are not available.
type routineEventActions struct {
	offerer        RoutineEventOfferer
	sources        RoutineEventSourceCatalog
	hooksAvailable bool
}

func newRoutineEventActions(offerer RoutineEventOfferer, sources RoutineEventSourceCatalog, hooksAvailable bool) routineEventActions {
	return routineEventActions{offerer: offerer, sources: sources, hooksAvailable: hooksAvailable}
}

func routineEventSourceKindOfProvider(provider string) (RoutineEventKind, bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "slack":
		return RoutineEventSlackMessage, true
	case "telegram":
		return RoutineEventTelegramMessage, true
	}
	return "", false
}

func routineConversationSourceLabel(kind RoutineEventKind, name, installationID string) string {
	provider := "Slack"
	if kind == RoutineEventTelegramMessage {
		provider = "Telegram"
	}
	name = strings.Join(strings.Fields(name), " ")
	switch {
	case name == "":
		name = provider + " " + installationID
	case !strings.Contains(strings.ToLower(name), strings.ToLower(provider)):
		if kind == RoutineEventTelegramMessage {
			name += " on Telegram"
		} else {
			name += " Slack"
		}
	}
	if len(name) > RoutineEventSourceLabelMax {
		name = truncateUTF8Bytes(name, RoutineEventSourceLabelMax)
	}
	return name
}

// routineConversationSources lists the umbrella's Slack and Telegram
// connections: one entry per connection SourceRef, from its active gateways
// with an installation and its active endpoints, each with the channel
// conversations of its active endpoints.
func routineConversationSources(ctx context.Context, store interface{}, scope Scope, owner ObjectiveOwner) ([]*RoutineEventSourceView, error) {
	views := map[string]*RoutineEventSourceView{}
	order := []string{}
	add := func(kind RoutineEventKind, sourceRef, label string) *RoutineEventSourceView {
		if view := views[sourceRef]; view != nil {
			if view.Label == "" {
				view.Label = label
			}
			return view
		}
		view := &RoutineEventSourceView{RoutineEventSourceOption: RoutineEventSourceOption{
			Kind: kind, Source: sourceRef, Label: label, Available: true, Fields: RoutineEventFilterFields(kind),
		}}
		views[sourceRef] = view
		order = append(order, sourceRef)
		return view
	}
	if gateways, ok := store.(ExternalConversationGatewayStore); ok {
		for offset := 0; ; offset += routineEventSourcePageSize {
			page, err := gateways.ListExternalConversationGateways(ctx, ExternalConversationGatewayFilter{
				Scope: scope, Owner: &owner, Statuses: []ExternalConversationGatewayStatus{ExternalConversationGatewayActive},
				Limit: routineEventSourcePageSize, Offset: offset,
			})
			if err != nil {
				return nil, err
			}
			for _, entry := range page {
				gateway := entry.Gateway
				kind, known := routineEventSourceKindOfProvider(gateway.Provider)
				if !known || entry.Status != ExternalConversationGatewayActive || gateway.Owner != owner || gateway.InstallationID == "" {
					continue
				}
				sourceRef := RoutineConversationSourceRef(gateway.DeploymentID, gateway.Adapter.BindingID, gateway.Adapter.AdapterID, gateway.InstallationID)
				add(kind, sourceRef, routineConversationSourceLabel(kind, entry.Name, gateway.InstallationID))
			}
			if len(page) < routineEventSourcePageSize {
				break
			}
		}
	}
	if endpoints, ok := store.(ExternalConversationEndpointStore); ok {
		for offset := 0; ; offset += routineEventSourcePageSize {
			page, err := endpoints.ListExternalConversationEndpoints(ctx, ExternalConversationEndpointFilter{
				Scope: scope, Owner: &owner, Statuses: []ExternalConversationEndpointStatus{ExternalConversationEndpointActive},
				Limit: routineEventSourcePageSize, Offset: offset,
			})
			if err != nil {
				return nil, err
			}
			for _, endpoint := range page {
				kind, known := routineEventSourceKindOfProvider(endpoint.Provider)
				if !known || endpoint.Status != ExternalConversationEndpointActive || endpoint.Owner != owner || endpoint.InstallationID == "" {
					continue
				}
				sourceRef := RoutineConversationSourceRef(endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID, endpoint.InstallationID)
				view := add(kind, sourceRef, "")
				// Direct conversations never feed routines (E14).
				address := strings.TrimSpace(endpoint.Address)
				if endpoint.Mode != capability.ConversationEndpointChannel || address == "" || endpoint.InstallationWide {
					continue
				}
				if !slices.ContainsFunc(view.Channels, func(channel RoutineEventSourceChannel) bool { return channel.ID == address }) {
					view.Channels = append(view.Channels, RoutineEventSourceChannel{ID: address, Name: strings.TrimSpace(endpoint.Name)})
				}
			}
			if len(page) < routineEventSourcePageSize {
				break
			}
		}
	}
	result := make([]*RoutineEventSourceView, 0, len(order))
	for _, sourceRef := range order {
		view := views[sourceRef]
		if view.Label == "" {
			view.Label = routineConversationSourceLabel(view.Kind, "", "connection")
		}
		sort.Slice(view.Channels, func(i, j int) bool { return view.Channels[i].ID < view.Channels[j].ID })
		result = append(result, view)
	}
	return result, nil
}

// routineCallbackRegistrationEventTypes returns the event types a GitHub
// registration of the umbrella feeds to routines, without "github.", or nil
// when it feeds none.
func routineCallbackRegistrationEventTypes(registration *CallbackRegistration, owner ObjectiveOwner) []string {
	if registration == nil || registration.Status != CallbackRegistrationActive || !strings.EqualFold(registration.Provider, "github") ||
		registration.DeploymentID != owner.ID || registration.Owner != owner {
		return nil
	}
	types := []string{}
	for _, subscription := range registration.Subscriptions {
		if subscription.Consumer != routineCallbackConsumer {
			continue
		}
		eventType := strings.TrimPrefix(strings.TrimSpace(subscription.EventType), "github.")
		if eventType != "" && !slices.Contains(types, eventType) {
			types = append(types, eventType)
		}
	}
	if len(types) == 0 {
		return nil
	}
	sort.Strings(types)
	return types
}

// routineCallbackSources lists the umbrella's GitHub callback registrations
// that have a routines subscription.
func routineCallbackSources(ctx context.Context, store interface{}, scope Scope, owner ObjectiveOwner) ([]*RoutineEventSourceView, error) {
	callbacks, ok := store.(CallbackRegistrationStore)
	if !ok {
		return nil, nil
	}
	result := []*RoutineEventSourceView{}
	for offset := 0; ; offset += routineEventSourcePageSize {
		page, err := callbacks.ListCallbackRegistrations(ctx, CallbackRegistrationFilter{
			Scope: scope, DeploymentID: owner.ID, Statuses: []CallbackRegistrationStatus{CallbackRegistrationActive},
			Limit: routineEventSourcePageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		for _, registration := range page {
			types := routineCallbackRegistrationEventTypes(registration, owner)
			if types == nil {
				continue
			}
			label := strings.Join(strings.Fields(registration.Name), " ")
			if !strings.Contains(strings.ToLower(label), "github") {
				label += " (GitHub)"
			}
			result = append(result, &RoutineEventSourceView{RoutineEventSourceOption: RoutineEventSourceOption{
				Kind: RoutineEventGitHub, Source: RoutineCallbackSourceRef(registration.ID), Label: truncateUTF8Bytes(label, RoutineEventSourceLabelMax),
				Available: true, Fields: RoutineEventFilterFields(RoutineEventGitHub), EventTypes: types,
			}})
		}
		if len(page) < routineEventSourcePageSize {
			break
		}
	}
	return result, nil
}

// routineGoogleSources lists the host's Google sources of actingUser. Only
// the person's own sign-in counts (E10): any option for another person is
// dropped.
func routineGoogleSources(ctx context.Context, catalog RoutineEventSourceCatalog, scope Scope, owner ObjectiveOwner, actingUser string) ([]*RoutineEventSourceView, error) {
	if catalog == nil || strings.TrimSpace(actingUser) == "" {
		return nil, nil
	}
	options, err := catalog.ListRoutineEventSources(ctx, scope, owner, actingUser)
	if err != nil {
		return nil, err
	}
	result := make([]*RoutineEventSourceView, 0, len(options))
	for _, option := range options {
		if option.Kind != RoutineEventGmailMessage && option.Kind != RoutineEventCalendar {
			continue
		}
		if option.Available && !routineGoogleSourceOf(option.Source, actingUser) {
			continue
		}
		option.Fields = RoutineEventFilterFields(option.Kind)
		result = append(result, &RoutineEventSourceView{RoutineEventSourceOption: option})
	}
	return result, nil
}

func routineGoogleSourceOf(sourceRef, userID string) bool {
	return userID != "" && strings.HasPrefix(sourceRef, routineGoogleSourcePrefix+userID+":") && validateRoutineSourceRef(RoutineEventGmailMessage, sourceRef) == nil
}

func routineWebhookSource() *RoutineEventSourceView {
	return &RoutineEventSourceView{RoutineEventSourceOption: RoutineEventSourceOption{
		Kind: RoutineEventWebhook, Source: RoutineEventSourceNew, Label: "A new webhook address", Available: true,
		Fields: RoutineEventFilterFields(RoutineEventWebhook),
	}}
}

// listRoutineEventSources is what event_sources returns and what create_task
// resolves a source against: the umbrella's connections, GitHub
// registrations, the person's own Google sign-in and a new webhook.
func listRoutineEventSources(ctx context.Context, store interface{}, events routineEventActions, scope Scope, owner ObjectiveOwner, actingUser string, kind RoutineEventKind) ([]*RoutineEventSourceView, error) {
	if kind != "" && !kind.Valid() {
		return nil, fmt.Errorf("unknown event kind %q", kind)
	}
	want := func(candidate RoutineEventKind) bool { return kind == "" || kind == candidate }
	result := []*RoutineEventSourceView{}
	if want(RoutineEventSlackMessage) || want(RoutineEventTelegramMessage) {
		conversations, err := routineConversationSources(ctx, store, scope, owner)
		if err != nil {
			return nil, err
		}
		for _, view := range conversations {
			if want(view.Kind) {
				result = append(result, view)
			}
		}
	}
	if want(RoutineEventGitHub) {
		callbacks, err := routineCallbackSources(ctx, store, scope, owner)
		if err != nil {
			return nil, err
		}
		result = append(result, callbacks...)
	}
	if want(RoutineEventGmailMessage) || want(RoutineEventCalendar) {
		google, err := routineGoogleSources(ctx, events.sources, scope, owner, actingUser)
		if err != nil {
			return nil, err
		}
		for _, view := range google {
			if want(view.Kind) {
				result = append(result, view)
			}
		}
	}
	if want(RoutineEventWebhook) && events.hooksAvailable {
		result = append(result, routineWebhookSource())
	}
	return result, nil
}

func dispatchRoutineEventSources(ctx context.Context, store runbookActionStore, events routineEventActions, run *AgentRun, arguments map[string]interface{}) (map[string]interface{}, error) {
	var args struct {
		Kind RoutineEventKind `json:"kind"`
	}
	if err := decodeScheduledTaskArguments(arguments, &args); err != nil {
		return nil, err
	}
	sources, err := listRoutineEventSources(ctx, store, events, run.Scope, run.Owner, run.ActingUser, args.Kind)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"sources": sources}, nil
}

// RoutineEventSourceChecker tells the dispatcher's hourly source check
// whether an event routine's Slack, Telegram or GitHub source still exists
// for its owner. Google and webhook sources are checked elsewhere (the host's
// sync loop and the hook row), so it reports them as present.
type RoutineEventSourceChecker struct {
	store interface{}
}

func NewRoutineEventSourceChecker(store interface{}) *RoutineEventSourceChecker {
	return &RoutineEventSourceChecker{store: store}
}

func (c *RoutineEventSourceChecker) SourceExists(ctx context.Context, scope Scope, owner ObjectiveOwner, kind RoutineEventKind, sourceRef string) (bool, error) {
	if c == nil || c.store == nil {
		return false, errors.New("routine source checker is not configured")
	}
	switch kind {
	case RoutineEventSlackMessage, RoutineEventTelegramMessage:
		views, err := routineConversationSources(ctx, c.store, scope, owner)
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(views, func(view *RoutineEventSourceView) bool {
			return view.Kind == kind && view.Source == sourceRef
		}), nil
	case RoutineEventGitHub:
		callbacks, ok := c.store.(CallbackRegistrationStore)
		id, prefixed := strings.CutPrefix(sourceRef, routineCallbackSourcePrefix)
		if !ok || !prefixed {
			return false, nil
		}
		registration, err := callbacks.GetCallbackRegistration(ctx, scope, id)
		if err != nil {
			return false, err
		}
		return routineCallbackRegistrationEventTypes(registration, owner) != nil, nil
	}
	return true, nil
}

// resolveRoutineEventTrigger turns a create_task trigger.event into the
// routine's trigger. The source always comes from the same listing as
// event_sources for this run's owner and person, never from the model's
// string alone. activationID names the routine a webhook's hook belongs to.
func resolveRoutineEventTrigger(ctx context.Context, store interface{}, events routineEventActions, run *AgentRun, activationID string, args *scheduledTaskEventArguments) (*RoutineEventTrigger, error) {
	if args == nil {
		return nil, errors.New("trigger.event is required")
	}
	if strings.TrimSpace(run.ActingUser) == "" {
		return nil, errors.New("Event routines need a signed-in person")
	}
	if !args.Kind.Valid() {
		return nil, fmt.Errorf("unknown event kind %q", args.Kind)
	}
	source := strings.TrimSpace(args.Source)
	trigger := &RoutineEventTrigger{
		Kind: args.Kind, Filter: cloneRoutineEventConditions(args.Filter), IncludeBots: args.IncludeBots,
		MaxPerHour: args.MaxPerHour,
	}
	if trigger.MaxPerHour == 0 {
		trigger.MaxPerHour = RoutineEventDefaultMaxPerHour
	}
	if args.MaximumOccurrences != nil {
		if *args.MaximumOccurrences < 1 || *args.MaximumOccurrences > 1000000 {
			return nil, errors.New("maximumOccurrences must be between 1 and 1000000")
		}
		trigger.MaximumOccurrences = *args.MaximumOccurrences
	}
	if args.Calendar != nil {
		calendar := *args.Calendar
		trigger.Calendar = &calendar
	}
	var channels []RoutineEventSourceChannel
	if args.Kind == RoutineEventWebhook {
		if source != RoutineEventSourceNew {
			return nil, errors.New("a webhook routine's source must be \"new\"")
		}
		if !events.hooksAvailable {
			return nil, ErrRoutineHooksUnavailable
		}
		hookID := HookID(run.Scope, activationID)
		trigger.SourceRef, trigger.SourceLabel, trigger.Webhook = HookSourceRef(hookID), "Webhook", &RoutineWebhook{HookID: hookID}
	} else {
		sources, err := listRoutineEventSources(ctx, store, events, run.Scope, run.Owner, run.ActingUser, args.Kind)
		if err != nil {
			return nil, err
		}
		index := slices.IndexFunc(sources, func(view *RoutineEventSourceView) bool { return view.Source == source })
		if index < 0 {
			if args.Kind == RoutineEventGmailMessage || args.Kind == RoutineEventCalendar {
				return nil, errors.New("Gmail and Calendar routines use only your own Google sign-in; use a source from event_sources")
			}
			return nil, fmt.Errorf("unknown event source %q; use a source from event_sources", source)
		}
		option := sources[index]
		if !option.Available {
			switch option.Reason {
			case RoutineEventSourceNeedsSignIn:
				return nil, errors.New("this source needs the person's sign-in first")
			case RoutineEventSourceSharedSignInNotAllowed:
				return nil, errors.New("Gmail and Calendar routines use only your own Google sign-in, not a shared one")
			}
			return nil, errors.New("this event source is not available")
		}
		if (args.Kind == RoutineEventGmailMessage || args.Kind == RoutineEventCalendar) && !routineGoogleSourceOf(option.Source, run.ActingUser) {
			return nil, errors.New("Gmail and Calendar routines use only your own Google sign-in")
		}
		trigger.SourceRef, trigger.SourceLabel, channels = option.Source, option.Label, option.Channels
	}
	if err := trigger.Validate(); err != nil {
		return nil, err
	}
	if field := routineEventPlaceField(trigger.Kind); field != "" {
		if err := pinRoutineEventChannels(trigger, field, channels); err != nil {
			return nil, err
		}
	}
	return trigger, nil
}

// pinRoutineEventChannels requires every channel or chat a Slack or Telegram
// routine names to be a conversation the umbrella has an active endpoint in
// on that connection (E14), and labels unlabeled conditions with the
// endpoint's name.
func pinRoutineEventChannels(trigger *RoutineEventTrigger, field string, channels []RoutineEventSourceChannel) error {
	find := func(value string) *RoutineEventSourceChannel {
		value = strings.TrimSpace(value)
		for index := range channels {
			if strings.EqualFold(channels[index].ID, value) {
				return &channels[index]
			}
		}
		return nil
	}
	for index := range trigger.Filter {
		condition := &trigger.Filter[index]
		if condition.Field != field || condition.Not || (condition.Op != RoutineFilterEquals && condition.Op != RoutineFilterOneOf) {
			continue
		}
		values := condition.Values
		if condition.Op == RoutineFilterEquals {
			values = []string{condition.Value}
		}
		names := make([]string, 0, len(values))
		for _, value := range values {
			channel := find(value)
			if channel == nil {
				return fmt.Errorf("%s %q is not a conversation this chat's Agent or Team is set up in on that connection; use a channel from event_sources", field, strings.TrimSpace(value))
			}
			names = append(names, channel.Name)
		}
		if strings.TrimSpace(condition.Label) == "" && !slices.Contains(names, "") {
			condition.Label = truncateUTF8Bytes(strings.Join(names, ", "), RoutineEventConditionLabelMax)
		}
	}
	return nil
}

func cloneRoutineEventConditions(values []RoutineEventCondition) []RoutineEventCondition {
	if values == nil {
		return nil
	}
	result := make([]RoutineEventCondition, len(values))
	for index, condition := range values {
		condition.Values = append([]string(nil), condition.Values...)
		if len(condition.Values) == 0 {
			condition.Values = nil
		}
		result[index] = condition
	}
	return result
}

// routineEventPreview is the event part of a routine's brief card.
func routineEventPreview(trigger *RoutineEventTrigger) map[string]interface{} {
	if trigger == nil {
		return nil
	}
	filter := make([]interface{}, 0, len(trigger.Filter))
	for _, condition := range trigger.Filter {
		item := map[string]interface{}{"field": condition.Field, "op": string(condition.Op)}
		if condition.Value != "" {
			item["value"] = condition.Value
		}
		if len(condition.Values) > 0 {
			values := make([]interface{}, 0, len(condition.Values))
			for _, value := range condition.Values {
				values = append(values, value)
			}
			item["values"] = values
		}
		if condition.Not {
			item["not"] = true
		}
		if condition.Label != "" {
			item["label"] = condition.Label
		}
		filter = append(filter, item)
	}
	preview := map[string]interface{}{
		"kind": string(trigger.Kind), "sourceLabel": trigger.SourceLabel,
		"when": DescribeRoutineEventTrigger(trigger), "whoCanTrigger": DescribeRoutineEventTriggerAudience(trigger),
		"filter": filter, "maxPerHour": trigger.MaxPerHour, "webhook": trigger.Kind == RoutineEventWebhook,
	}
	if trigger.MaximumOccurrences > 0 {
		preview["maximumOccurrences"] = trigger.MaximumOccurrences
	}
	if trigger.Calendar != nil {
		calendar := map[string]interface{}{"on": trigger.Calendar.On}
		if trigger.Calendar.Minutes > 0 {
			calendar["minutes"] = trigger.Calendar.Minutes
		}
		preview["calendar"] = calendar
	}
	return preview
}

// routineEventSourcesAction lists what the chat's Agent or Team can listen to.
func routineEventSourcesAction() skill.Action {
	kinds := make([]interface{}, 0, len(RoutineEventKinds))
	for _, kind := range RoutineEventKinds {
		kinds = append(kinds, string(kind))
	}
	return skill.Action{
		Name:        RunbookActionEventSources,
		Description: "List what this chat's Agent or Team can listen to: Slack and Telegram connections, GitHub repositories, your own Google sign-in for Gmail and Calendar, and a new webhook URL. Use a source's `source` value in create_task trigger.event.source. A source with available false needs the person first (for example their Google sign-in); tell them, and use that skill's sign-in step before creating the routine.",
		Risk:        skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
			"kind": map[string]interface{}{"type": "string", "enum": kinds},
		}},
		OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"sources": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}},
		}, "required": []interface{}{"sources"}},
	}
}
