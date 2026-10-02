package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

// ResolvedExternalConversationRunbook pins an immutable portable Runbook to
// the deployed Agent that can execute it. The host may resolve this from an
// Agent definition, Team roster, or another authorized catalog, but must not
// substitute a different version at dispatch time.
type ResolvedExternalConversationRunbook struct {
	Definition      *runbook.Definition
	AssignedAgentID string
}

type ExternalConversationRunbookResolver interface {
	ResolveExternalConversationRunbook(
		context.Context,
		Scope,
		*ExternalConversationEndpoint,
		ExternalConversationHandler,
	) (*ResolvedExternalConversationRunbook, error)
}

type ExternalConversationRunbookEventDispatcher struct {
	runs              *RunCommandService
	resolver          ExternalConversationRunbookResolver
	executionPreparer AcceptedRunExecutionPreparer
}

func (d *ExternalConversationRunbookEventDispatcher) SetAcceptedRunExecutionPreparer(preparer AcceptedRunExecutionPreparer) {
	if d != nil {
		d.executionPreparer = preparer
	}
}

func NewExternalConversationRunbookEventDispatcher(
	store RunCommandStore,
	resolver ExternalConversationRunbookResolver,
) *ExternalConversationRunbookEventDispatcher {
	return &ExternalConversationRunbookEventDispatcher{runs: NewRunCommandService(store), resolver: resolver}
}

// DispatchExternalConversationRunbook resolves one exact Runbook trigger and
// creates its canonical event Run idempotently. Runbook execution then uses
// the ordinary Agent Run worker, checkpoints, governed actions, delegation,
// waits, budgets, and approvals rather than a conversation-specific executor.
func (d *ExternalConversationRunbookEventDispatcher) DispatchExternalConversationRunbook(
	ctx context.Context,
	handler ExternalConversationHandler,
	req ExternalConversationDispatchRequest,
) (*ExternalConversationDispatchResult, error) {
	if d == nil || d.runs == nil || d.resolver == nil {
		return nil, errors.New("external conversation Runbook dispatcher is not configured")
	}
	if req.Endpoint == nil || req.Conversation == nil || req.Message == nil ||
		req.Endpoint.Handler != handler || handler.Kind != ExternalConversationHandlerRunbook ||
		strings.TrimSpace(req.IdempotencyKey) == "" || len(strings.TrimSpace(req.IdempotencyKey)) > 256 {
		return nil, fmt.Errorf("%w: exact Runbook dispatch context is required", ErrInvalidExternalConversation)
	}
	if err := handler.Validate(req.Endpoint.Owner); err != nil {
		return nil, err
	}
	if err := req.Event.Validate(); err != nil {
		return nil, err
	}
	if req.Event.Scope != req.Endpoint.Scope {
		return nil, fmt.Errorf("%w: event and endpoint scopes differ", ErrInvalidExternalConversation)
	}
	input := map[string]interface{}{
		"conversationId":   req.Conversation.ID,
		"triggerMessageId": req.Message.ID,
		"endpointId":       req.Endpoint.ID,
		"event": map[string]interface{}{
			"id": req.Event.ID, "type": req.Event.Type, "source": req.Event.Source,
			"subject": req.Event.Subject, "occurredAt": req.Event.OccurredAt.UTC().Format(timeRFC3339Nano),
			"attributes": cloneMap(req.Event.Attributes), "payload": cloneMap(req.Event.Payload),
		},
	}
	visibility := ActivityVisibilityTeam
	if req.Endpoint.Owner.Type == OwnerTypeAgent {
		visibility = ActivityVisibilityPrivate
	}
	requestForEntrypoint := func(entrypoint string) CreateAgentRunRequest {
		return CreateAgentRunRequest{
			Scope: req.Endpoint.Scope, Kind: RunKindConversation, Owner: req.Endpoint.Owner,
			AssignedAgentID: handler.AssignedAgentID, Entrypoint: entrypoint,
			ConcurrencyKey: req.Conversation.ID,
			Goal:           "Handle a conversation message through Runbook " + handler.ID,
			Source:         RunSourceEvent, Context: input,
			Plan: map[string]interface{}{"runbook": map[string]interface{}{
				"id": handler.ID, "version": handler.Version, "trigger": handler.Trigger,
			}},
			IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
			Actor:          ActivityActor{Type: "service", ID: externalConversationActorID}, Visibility: visibility,
		}
	}
	// Recover only an exact previously accepted delivery before consulting the
	// current deployment. Its entrypoint comes from the saved immutable method;
	// all caller-supplied work remains checked by the canonical fingerprint.
	accepted, err := d.runs.store.GetAgentRun(ctx, req.Endpoint.Scope, runIDForIdempotencyKey(req.Endpoint.Scope, strings.TrimSpace(req.IdempotencyKey)))
	if err != nil {
		return nil, err
	}
	if accepted != nil {
		pin, err := AcceptedRunExecutionForRun(accepted)
		if err != nil || accepted.AssignedAgentID != handler.AssignedAgentID || accepted.Owner != req.Endpoint.Owner || strings.TrimSpace(accepted.Entrypoint) == "" ||
			(pin != nil && (pin.RunbookID != handler.ID || pin.RunbookVersion != handler.Version)) {
			return nil, ErrRunIdempotency
		}
		replayed, err := d.runs.FindCreatedAgentRun(ctx, requestForEntrypoint(accepted.Entrypoint))
		if err != nil {
			return nil, err
		}
		if replayed != nil && replayed.Run != nil {
			return &ExternalConversationDispatchResult{RunID: replayed.Run.ID}, nil
		}
	}
	resolved, err := d.resolver.ResolveExternalConversationRunbook(ctx, req.Endpoint.Scope, req.Endpoint, handler)
	if err != nil {
		return nil, err
	}
	if err := validateResolvedExternalConversationRunbook(resolved, handler, req.Event.Type); err != nil {
		return nil, err
	}
	trigger := resolved.Definition.Triggers[handler.Trigger]
	if contract, ok := resolved.Definition.Interfaces[trigger.Entrypoint]; ok {
		if err := runbook.ValidateInterfaceInput(contract.InputSchema, input); err != nil {
			return nil, fmt.Errorf("%w: Runbook trigger input: %v", ErrInvalidExternalConversation, err)
		}
	}
	result, err := createAgentRunWithAcceptedExecution(ctx, d.runs, requestForEntrypoint(trigger.Entrypoint), d.executionPreparer)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Run == nil {
		return nil, errors.New("Runbook dispatch did not create a canonical Run")
	}
	return &ExternalConversationDispatchResult{RunID: result.Run.ID}, nil
}

func validateResolvedExternalConversationRunbook(
	resolved *ResolvedExternalConversationRunbook,
	handler ExternalConversationHandler,
	eventType string,
) error {
	if resolved == nil || resolved.Definition == nil || !validAgentReference(strings.TrimSpace(resolved.AssignedAgentID), 256) {
		return fmt.Errorf("%w: Runbook resolver returned an incomplete executable binding", ErrInvalidExternalConversation)
	}
	if diagnostics := runbook.Validate(resolved.Definition); len(diagnostics) > 0 {
		return fmt.Errorf("%w: resolved Runbook is invalid at %s", ErrInvalidExternalConversation, diagnostics[0].Path)
	}
	if resolved.Definition.ID != handler.ID || resolved.Definition.Version != handler.Version || resolved.AssignedAgentID != handler.AssignedAgentID {
		return fmt.Errorf("%w: resolved Runbook identity drifted", ErrExternalConversationConflict)
	}
	trigger, ok := resolved.Definition.Triggers[handler.Trigger]
	if !ok || trigger.Kind != runbook.TriggerEvent || trigger.EventType != eventType {
		return fmt.Errorf("%w: Runbook trigger does not accept event %q", ErrInvalidExternalConversation, eventType)
	}
	return nil
}

const timeRFC3339Nano = "2006-01-02T15:04:05.999999999Z07:00"
