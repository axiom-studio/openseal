package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// ApprovalNotificationStore is the durable intersection needed to project a
// canonical approval into an external conversation. No provider-specific
// fields or credentials cross this boundary.
type ApprovalNotificationStore interface {
	ExternalConversationStore
	ActionStore
	PortfolioStore
}

type ApprovalNotificationWorker struct {
	store         ApprovalNotificationStore
	conversations *ConversationService
	transport     *ExternalConversationTransportService
	now           func() time.Time
}

const approvalNotificationPostAttempts = 5

func NewApprovalNotificationWorker(store ApprovalNotificationStore, transport *ExternalConversationTransportService) *ApprovalNotificationWorker {
	return &ApprovalNotificationWorker{store: store, conversations: NewConversationService(store), transport: transport, now: time.Now}
}

// ProcessScope idempotently materializes pending approvals as canonical
// approval_request messages and durable endpoint deliveries.
func (w *ApprovalNotificationWorker) ProcessScope(ctx context.Context, scope Scope, limit int) (int, error) {
	if w == nil || w.store == nil || w.transport == nil {
		return 0, errors.New("approval notification worker is not configured")
	}
	// Pending checkpoints are deadline-bearing work and must never queue behind
	// historical approved checkpoints. Approved outcomes are read newest-first
	// so a fresh decision is projected even when the tenant has a large audit
	// history. Delivery idempotency makes repeated reconciliation safe.
	pending, err := w.store.ListApprovals(ctx, ApprovalFilter{Scope: scope, Status: []ApprovalStatus{ApprovalStatusPending}, Limit: limit})
	if err != nil {
		return 0, err
	}
	approved, err := w.store.ListApprovals(ctx, ApprovalFilter{Scope: scope, Status: []ApprovalStatus{ApprovalStatusApproved}, Limit: limit, NewestFirst: true})
	if err != nil {
		return 0, err
	}
	approvals := append(pending, approved...)
	processed := 0
	var processErrors []error
	now := w.now().UTC()
	for _, approval := range approvals {
		if approval.Status == ApprovalStatusApproved {
			call, callErr := w.store.GetActionCall(ctx, approval.Scope, approval.ActionCallID)
			if callErr != nil {
				processErrors = append(processErrors, fmt.Errorf("load approved action %s: %w", approval.ActionCallID, callErr))
				continue
			}
			if !terminalApprovalActionStatus(call.Status) {
				continue
			}
			for _, destination := range approval.Destinations {
				if err := w.notifyOutcome(ctx, approval, call, destination); err != nil {
					processErrors = append(processErrors, fmt.Errorf("notify approval outcome %s at endpoint %s: %w", approval.ID, destination.EndpointID, err))
					continue
				}
				processed++
			}
			continue
		}
		if !now.Before(approval.ExpiresAt) {
			if err := w.resolveTimeout(ctx, approval, now); err != nil {
				processErrors = append(processErrors, fmt.Errorf("resolve approval timeout %s: %w", approval.ID, err))
			}
			continue
		}
		for _, destination := range approval.Destinations {
			if err := w.notify(ctx, approval, destination); err != nil {
				processErrors = append(processErrors, fmt.Errorf("notify approval %s at endpoint %s: %w", approval.ID, destination.EndpointID, err))
				continue
			}
			processed++
		}
	}
	return processed, errors.Join(processErrors...)
}

func terminalApprovalActionStatus(status ActionCallStatus) bool {
	switch status {
	case ActionCallStatusSucceeded, ActionCallStatusFailed, ActionCallStatusDenied,
		ActionCallStatusCanceled, ActionCallStatusCompensated:
		return true
	default:
		return false
	}
}

func (w *ApprovalNotificationWorker) notifyOutcome(ctx context.Context, approval *ApprovalCheckpoint, call *ActionCall, destination ApprovalDestination) error {
	original, err := findDeliveredApprovalNotification(ctx, w.store, approval, destination)
	if err != nil {
		return err
	}
	// A destination that never received the request must not receive a
	// context-free terminal message during recovery or historical backfill.
	// Once the request delivery succeeds, a later idempotent pass will project
	// the outcome.
	if original == nil {
		return nil
	}
	if err := enqueueApprovalCardUpdate(ctx, w.store, w.transport, approval, call, destination, ""); err != nil {
		return err
	}
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, approval.Scope, strings.TrimSpace(destination.EndpointID))
	if err != nil {
		return err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive {
		return fmt.Errorf("%w: approval endpoint is unavailable", ErrInvalidExternalConversation)
	}
	conversation, _, err := w.conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: approval.Scope, Owner: endpoint.Owner, Title: endpoint.Name + " approvals",
		IdempotencyKey: "approval-notifications:" + endpoint.ID,
	})
	if err != nil {
		return err
	}
	phase := approvalCardPhase(approval, call)
	posted, err := w.postOutcomeMessage(ctx, approval, call, conversation.ID, "approval-outcome:"+approval.ID+":"+phase)
	if err != nil {
		return err
	}
	_, err = w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: approval.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
		ConversationID: conversation.ID, ChannelMessageID: posted.Message.ID,
		IdempotencyKey: "approval-outcome-delivery:" + approval.ID + ":" + endpoint.ID + ":" + phase,
	})
	return err
}

func (w *ApprovalNotificationWorker) resolveTimeout(ctx context.Context, approval *ApprovalCheckpoint, now time.Time) error {
	coordinator := NewApprovalCoordinator(w.store, w.store, EligibleApprovalAuthorizer{})
	coordinator.now = func() time.Time { return now }
	resolved, err := coordinator.ResolveTimeout(ctx, approval.Scope, approval.ID, approval.Revision, approval.ID)
	if err != nil {
		return err
	}
	if resolved == nil || resolved.Approval == nil || (resolved.Approval.Status != ApprovalStatusExpired && resolved.Approval.Status != ApprovalStatusApproved) {
		return errors.New("approval timeout did not reach a terminal state")
	}
	for _, destination := range approval.Destinations {
		if err := w.updateTimeoutCard(ctx, resolved.Approval, destination); err != nil {
			return err
		}
	}
	return nil
}

func (w *ApprovalNotificationWorker) updateTimeoutCard(ctx context.Context, approval *ApprovalCheckpoint, destination ApprovalDestination) error {
	call, err := w.store.GetActionCall(ctx, approval.Scope, approval.ActionCallID)
	if err != nil {
		return err
	}
	return enqueueApprovalCardUpdate(ctx, w.store, w.transport, approval, call, destination, "")
}

func enqueueApprovalCardUpdate(
	ctx context.Context,
	store ApprovalNotificationStore,
	transport *ExternalConversationTransportService,
	approval *ApprovalCheckpoint,
	call *ActionCall,
	destination ApprovalDestination,
	providerApproverID string,
) error {
	if approval == nil || call == nil {
		return errors.New("approval and action are required")
	}
	_, resolved, err := transport.resolveActiveEndpoint(ctx, approval.Scope, destination.EndpointID)
	if err != nil {
		return err
	}
	if !containsConversationDeliveryOperation(resolved.Adapter.Delivery.Operations, capability.ConversationDeliveryMessageUpdate) {
		return nil
	}
	delivery, err := findDeliveredApprovalNotification(ctx, store, approval, destination)
	if err != nil {
		return err
	}
	if delivery == nil {
		// A decision can arrive only after the provider has rendered the original
		// message, but expiry can race its delivery. Canonical state remains
		// authoritative when there is not yet a remote card to update.
		return nil
	}
	parameters := map[string]interface{}{
		"providerMessageId": delivery.ProviderMessageID,
		"approval":          approvalNotificationPayload(approval, call),
	}
	if strings.TrimSpace(providerApproverID) != "" {
		parameters["providerApproverId"] = strings.TrimSpace(providerApproverID)
		parameters["approval"].(map[string]interface{})["providerApproverId"] = strings.TrimSpace(providerApproverID)
	}
	_, err = transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: approval.Scope, EndpointID: destination.EndpointID,
		Operation:      capability.ConversationDeliveryMessageUpdate,
		ConversationID: delivery.ConversationID, ChannelMessageID: delivery.ChannelMessageID,
		ExternalThreadID: delivery.ExternalThreadID,
		Parameters:       parameters,
		IdempotencyKey:   "approval-card-update:" + approval.ID + ":" + destination.EndpointID + ":" + approvalCardPhase(approval, call),
	})
	return err
}

func findDeliveredApprovalNotification(
	ctx context.Context,
	store ApprovalNotificationStore,
	approval *ApprovalCheckpoint,
	destination ApprovalDestination,
) (*ExternalConversationDelivery, error) {
	deliveries, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{
		Scope: approval.Scope, EndpointID: destination.EndpointID,
		Statuses: []ExternalConversationDeliveryStatus{ExternalConversationDeliveryDelivered}, Limit: 1000,
	})
	if err != nil {
		return nil, err
	}
	for _, delivery := range deliveries {
		if delivery.Operation != capability.ConversationDeliveryMessageSend {
			continue
		}
		projected, _ := delivery.Parameters["approval"].(map[string]interface{})
		if projected != nil && projected["id"] == approval.ID && strings.TrimSpace(delivery.ProviderMessageID) != "" {
			return delivery, nil
		}
	}
	return nil, nil
}

func approvalCardPhase(approval *ApprovalCheckpoint, call *ActionCall) string {
	if approval.Status != ApprovalStatusApproved {
		return string(approval.Status)
	}
	switch call.Status {
	case ActionCallStatusSucceeded:
		return "succeeded"
	case ActionCallStatusFailed, ActionCallStatusDenied:
		return "failed"
	case ActionCallStatusCanceled, ActionCallStatusCompensated:
		return "canceled"
	default:
		return "going-ahead"
	}
}

func (w *ApprovalNotificationWorker) notify(ctx context.Context, approval *ApprovalCheckpoint, destination ApprovalDestination) error {
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, approval.Scope, strings.TrimSpace(destination.EndpointID))
	if err != nil {
		return err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive {
		return fmt.Errorf("%w: approval endpoint is unavailable", ErrInvalidExternalConversation)
	}
	call, err := w.store.GetActionCall(ctx, approval.Scope, approval.ActionCallID)
	if err != nil {
		return err
	}
	conversationKey := "approval-notifications:" + endpoint.ID
	conversation, _, err := w.conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: approval.Scope, Owner: endpoint.Owner, Title: endpoint.Name + " approvals", IdempotencyKey: conversationKey,
	})
	if err != nil {
		return err
	}
	messageKey := "approval-request:" + approval.ID
	posted, err := w.postApprovalMessage(ctx, approval, call, conversation.ID, messageKey)
	if err != nil {
		return fmt.Errorf("post canonical approval message: %w", err)
	}
	_, err = w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: approval.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
		ConversationID: conversation.ID, ChannelMessageID: posted.Message.ID,
		Parameters:     map[string]interface{}{"approval": approvalNotificationPayload(approval, call)},
		IdempotencyKey: "approval-delivery:" + approval.ID + ":" + endpoint.ID,
	})
	if err != nil {
		return fmt.Errorf("enqueue approval delivery: %w", err)
	}
	return nil
}

func approvalNotificationPayload(approval *ApprovalCheckpoint, call *ActionCall) map[string]interface{} {
	proposedAction := cloneMap(approval.ProposedAction)
	if reviewContext := approvalReviewContext(approval.ContinuationCheckpoint); len(reviewContext) > 0 {
		explicit, _ := proposedAction["reviewContext"].(map[string]interface{})
		for key, value := range reviewContext {
			if _, exists := explicit[key]; !exists {
				if explicit == nil {
					explicit = make(map[string]interface{})
				}
				explicit[key] = value
			}
		}
		if len(explicit) > 0 {
			proposedAction["reviewContext"] = explicit
		}
	}
	payload := map[string]interface{}{
		"id": approval.ID, "revision": approval.Revision, "actionCallId": approval.ActionCallID,
		"risk": approval.Risk, "summary": approval.Summary, "status": approval.Status,
		"policyReason": approval.PolicyReason, "proposedAction": proposedAction,
		"expiresAt": approval.ExpiresAt,
	}
	if approval.TimeoutDecision != "" {
		payload["timeoutDecision"] = approval.TimeoutDecision
	}
	if call != nil {
		payload["invocationDigest"] = call.InvocationDigest
		payload["actionStatus"] = call.Status
		if strings.TrimSpace(call.Error) != "" {
			payload["actionError"] = call.Error
		}
	}
	if approval.DecisionBy != nil {
		payload["decisionBy"] = map[string]interface{}{"type": approval.DecisionBy.Type, "id": approval.DecisionBy.ID}
	}
	if approval.DecidedAt != nil {
		payload["decidedAt"] = approval.DecidedAt.UTC()
	}
	if strings.TrimSpace(approval.DecisionReason) != "" {
		payload["decisionReason"] = approval.DecisionReason
	}
	return payload
}

// approvalReviewContext projects the explicit, approval-owned state that the
// Agent persisted alongside the proposed action. This is where a prepared
// public draft can remain when the final external action only addresses an
// already-filled UI control. The projection is capability-neutral, bounded,
// and secret-safe so every approval surface receives the same review facts.
func approvalReviewContext(checkpoint map[string]interface{}) map[string]interface{} {
	if len(checkpoint) == 0 {
		return nil
	}
	review := make(map[string]interface{})
	for _, field := range []string{"state", "output"} {
		facts, _ := checkpoint[field].(map[string]interface{})
		if len(facts) == 0 {
			continue
		}
		projected, _ := boundedApprovalReviewValue(facts, 0, new(int)).(map[string]interface{})
		for key, value := range projected {
			// State is the canonical approval-owned value when both
			// projections use the same field name. Output fills facts that
			// are otherwise absent, such as a prepared artifact or draft.
			if _, exists := review[key]; !exists {
				review[key] = value
			}
		}
	}
	if len(review) == 0 {
		return nil
	}
	return review
}

const (
	maxApprovalReviewDepth = 4
	maxApprovalReviewItems = 64
	maxApprovalReviewText  = 20_000
)

func boundedApprovalReviewValue(value interface{}, depth int, items *int) interface{} {
	if depth > maxApprovalReviewDepth || items == nil || *items >= maxApprovalReviewItems {
		return nil
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for key, child := range typed {
			if *items >= maxApprovalReviewItems {
				break
			}
			*items++
			if sensitiveFieldName(key) {
				continue
			}
			if projected := boundedApprovalReviewValue(child, depth+1, items); projected != nil {
				result[key] = projected
			}
		}
		return result
	case []interface{}:
		result := make([]interface{}, 0, len(typed))
		for _, child := range typed {
			if *items >= maxApprovalReviewItems {
				break
			}
			*items++
			if projected := boundedApprovalReviewValue(child, depth+1, items); projected != nil {
				result = append(result, projected)
			}
		}
		return result
	case string:
		if len(typed) > maxApprovalReviewText {
			return typed[:maxApprovalReviewText] + "…"
		}
		return typed
	case bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, nil:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func (w *ApprovalNotificationWorker) postApprovalMessage(
	ctx context.Context,
	approval *ApprovalCheckpoint,
	call *ActionCall,
	conversationID string,
	messageKey string,
) (*ChannelMessageCommitResult, error) {
	request := PostChannelMessageRequest{
		Scope: approval.Scope, ConversationID: conversationID,
		Sender:            ConversationParticipant{Type: ConversationParticipantService, ID: "approval-coordinator"},
		SenderDisplayName: "Approval coordinator", Intent: MessageIntentApprovalRequest,
		Content: approvalNotificationText(approval, call), Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: messageKey,
	}
	for range approvalNotificationPostAttempts {
		conversation, err := w.conversations.GetConversation(ctx, approval.Scope, conversationID)
		if err != nil {
			return nil, err
		}
		request.ExpectedRevision = conversation.Revision
		posted, err := w.conversations.PostChannelMessage(ctx, request)
		if err == nil {
			return posted, nil
		}
		if !errors.Is(err, ErrRevisionConflict) {
			return nil, err
		}
	}
	return nil, ErrRevisionConflict
}

func (w *ApprovalNotificationWorker) postOutcomeMessage(
	ctx context.Context,
	approval *ApprovalCheckpoint,
	call *ActionCall,
	conversationID string,
	messageKey string,
) (*ChannelMessageCommitResult, error) {
	content := "Completed: " + strings.TrimSpace(approval.Summary) + "."
	if call.Status != ActionCallStatusSucceeded {
		content = "Failed: " + strings.TrimSpace(approval.Summary) + "."
		if strings.TrimSpace(call.Error) != "" {
			content += " " + strings.TrimSpace(call.Error)
		}
	}
	request := PostChannelMessageRequest{
		Scope: approval.Scope, ConversationID: conversationID,
		Sender:            ConversationParticipant{Type: ConversationParticipantService, ID: "approval-coordinator"},
		SenderDisplayName: "Approval coordinator", Intent: MessageIntentUpdate,
		Content: content, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		IdempotencyKey: messageKey,
	}
	for range approvalNotificationPostAttempts {
		conversation, err := w.conversations.GetConversation(ctx, approval.Scope, conversationID)
		if err != nil {
			return nil, err
		}
		request.ExpectedRevision = conversation.Revision
		posted, err := w.conversations.PostChannelMessage(ctx, request)
		if err == nil {
			return posted, nil
		}
		if !errors.Is(err, ErrRevisionConflict) {
			return nil, err
		}
	}
	return nil, ErrRevisionConflict
}

func approvalNotificationText(approval *ApprovalCheckpoint, call *ActionCall) string {
	deadline := "Expires"
	if approval.TimeoutDecision == ApprovalTimeoutApprove {
		deadline = "Auto-approves"
	}
	return fmt.Sprintf("Approval required: %s (%s.%s, risk %s). %s %s.",
		approval.Summary, call.SkillID, call.Action, approval.Risk, deadline, approval.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
}
