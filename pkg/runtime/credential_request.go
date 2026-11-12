package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// CredentialRequestWakeType parks the requesting Run until the user saves the
// credential or dismisses the request.
const CredentialRequestWakeType = "credential_request"

type credentialRequestArguments struct {
	Kind    string `json:"kind"`
	Website string `json:"website,omitempty"`
	Reason  string `json:"reason"`
}

func credentialRequestAction() skill.Action {
	return skill.Action{Name: agentrun.SkillActionRequestCredential,
		Description: "Ask the user to save a credential in their vault through a durable in-chat card, for example when the live browser reports no_matching_login (no saved login for the current site), no_payment_card, or spend_cap_exceeded. Use kind website_login with website set to the exact page origin the browser reported (for example https://www.amazon.in), or kind payment_card to add a card or raise its spend cap. The reason is shown to the user, for example \"Add your amazon.in login so I can sign in and finish the order\". This action never accepts or returns secret values; never ask for passwords, card numbers or codes in chat. A pending request pauses this work until the user saves the credential or dismisses the card; then continue (retry the sign-in or card fill) or explain the dismissal. If the result is refused, explain the reason to the user and do not repeat the same request.",
		Risk:        skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
			"kind":    map[string]interface{}{"type": "string", "enum": []interface{}{CredentialRequestKindWebsiteLogin, CredentialRequestKindPaymentCard}},
			"website": map[string]interface{}{"type": "string", "description": "Exact http(s) origin of the site, required for website_login, e.g. https://www.amazon.in.", "maxLength": 512},
			"reason":  map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1000}}, "required": []interface{}{"kind", "reason"}},
		OutputSchema: interactionRequestOutputSchema("credentialRequest")}
}

// interactionRequestOutputSchema describes a created interaction or a refusal
// the model must explain to the user.
func interactionRequestOutputSchema(key string) map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
		key:       map[string]interface{}{"type": "object"},
		"refused": map[string]interface{}{"type": "object"},
	}}
}

func decodeCredentialRequestArguments(arguments map[string]interface{}) (credentialRequestArguments, error) {
	var a credentialRequestArguments
	if err := decodeSkillBindingArguments(arguments, &a); err != nil {
		return a, refuseInteraction("the credential request arguments do not match the action schema")
	}
	if strings.TrimSpace(a.Reason) == "" || utf8.RuneCountInString(a.Reason) > 1000 {
		return a, refuseInteraction("reason must contain between 1 and 1000 characters")
	}
	switch a.Kind {
	case CredentialRequestKindWebsiteLogin:
		origin, err := NormalizeCredentialRequestWebsite(a.Website)
		if err != nil {
			return a, refuseInteraction(err.Error())
		}
		a.Website = origin
	case CredentialRequestKindPaymentCard:
		if strings.TrimSpace(a.Website) != "" {
			origin, err := NormalizeCredentialRequestWebsite(a.Website)
			if err != nil {
				return a, refuseInteraction(err.Error())
			}
			a.Website = origin
		}
	default:
		return a, refuseInteraction("kind must be website_login or payment_card")
	}
	return a, nil
}

func (d *SkillBindingActionDispatcher) requestCredential(ctx context.Context, input agentrun.ActionDispatchInput, run *AgentRun, member string, umbrella ObjectiveOwner) (map[string]interface{}, error) {
	a, err := decodeCredentialRequestArguments(input.Arguments)
	if err != nil {
		return nil, err
	}
	store, ok := d.store.(CredentialRequestStore)
	if !ok {
		return nil, refuseInteraction("in-chat credential requests are unavailable on this host")
	}
	conversationID, messageID, err := d.skillSetupConversationOrigin(ctx, run, member, umbrella)
	if err != nil {
		return nil, refuseInteraction("a credential can only be requested from a conversation with the user")
	}
	id := "credential-request:" + input.Call.ID
	if existing, err := store.GetCredentialRequest(ctx, run.Scope, id); err != nil {
		return nil, err
	} else if existing != nil {
		return credentialRequestResult(existing)
	}
	// The same unanswered card of this run is reused rather than shown twice.
	// Another run's card is not: a card resumes only the run that made it,
	// and settling one settles its siblings.
	current, err := store.ListCredentialRequests(ctx, run.Scope, umbrella.ID, conversationID)
	if err != nil {
		return nil, err
	}
	for _, r := range current {
		if r.RunID == run.ID && r.Status == CredentialRequestStatusPending && r.Kind == a.Kind && r.Website == a.Website {
			return credentialRequestResult(r)
		}
	}
	now := time.Now().UTC()
	r := &CredentialRequest{ID: id, Scope: run.Scope, Owner: umbrella, DeploymentID: umbrella.ID, AgentID: member, ConversationID: conversationID, TriggerMessageID: messageID,
		RunID: run.ID, ActionCallID: input.Call.ID, Kind: a.Kind, Website: a.Website, Reason: strings.TrimSpace(a.Reason),
		Status: CredentialRequestStatusPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveCredentialRequest(ctx, r, 0); err != nil {
		if errors.Is(err, ErrCredentialRequestConflict) {
			if replay, readErr := store.GetCredentialRequest(ctx, run.Scope, id); readErr == nil && replay != nil {
				return credentialRequestResult(replay)
			}
		}
		return nil, err
	}
	return credentialRequestResult(r)
}

func credentialRequestResult(r *CredentialRequest) (map[string]interface{}, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var value map[string]interface{}
	if err = json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return map[string]interface{}{"credentialRequest": value}, nil
}

// pendingCredentialRequestFromAction returns the request a succeeded
// request_credential call created when it is still waiting for the user.
func pendingCredentialRequestFromAction(ctx context.Context, store interface{}, call *ActionCall) *CredentialRequest {
	if call == nil || call.Status != ActionCallStatusSucceeded || call.SkillID != SkillManagementSkillID || call.Action != agentrun.SkillActionRequestCredential {
		return nil
	}
	value, ok := call.Output["credentialRequest"].(map[string]interface{})
	if !ok {
		return nil
	}
	id, _ := value["id"].(string)
	requests, ok := store.(CredentialRequestStore)
	if !ok || id == "" {
		return nil
	}
	// Read the durable record: the user may have saved the credential between
	// dispatch and this commit, and a settled request must not park the Run.
	current, err := requests.GetCredentialRequest(ctx, call.Scope, id)
	if err != nil || current == nil || current.RunID != call.RunID || current.Status != CredentialRequestStatusPending {
		return nil
	}
	return current
}

// IsExactCredentialRequestWait reports whether the Run waits for this request.
func IsExactCredentialRequestWait(run *AgentRun, requestID string) bool {
	return run != nil && run.Status == AgentRunStatusWaitingForEvent && run.WakeCondition != nil &&
		run.WakeCondition.Type == CredentialRequestWakeType && run.WakeCondition.Reference == requestID
}

// CredentialRequestResolutionInstruction is the durable guidance given to a
// Run woken by its credential request.
func CredentialRequestResolutionInstruction(r *CredentialRequest) string {
	subject := "the requested card"
	if r.Kind == CredentialRequestKindWebsiteLogin {
		subject = "a login for " + r.Website
	}
	if r.Status == CredentialRequestStatusDismissed {
		return "The user dismissed the request to save " + subject + ". Do not request it again in this reply; explain what remains blocked and offer an alternative."
	}
	if r.Kind == CredentialRequestKindWebsiteLogin {
		return "The user saved " + subject + " in the vault. Continue the task from its saved progress: call live-browser-sign-in again on that site (reopen the site first if the browser session closed). Never ask for the password in chat."
	}
	return "The user saved or updated their card in the vault. Continue the task from its saved progress: call live-browser-fill-payment-card again on the checkout (reopen the site first if the browser session closed)."
}

// interactionRequestRefusal is a user-correctable reason an in-chat
// interaction could not be created. The model receives it as a result and
// explains it; it never ends the Run.
type interactionRequestRefusal struct{ message string }

func (e *interactionRequestRefusal) Error() string { return e.message }

func refuseInteraction(message string) error { return &interactionRequestRefusal{message: message} }

func isInteractionRequestAction(name string) bool {
	return name == agentrun.SkillActionRequestSetup || name == agentrun.SkillActionRequestCredential || name == agentrun.SkillActionRequestConnection
}

// interactionRequestRefusalResult turns any failure to create an in-chat
// interaction into a model-visible result. A failed form request is a fact to
// explain to the user, not a reason to stop the attempt.
func interactionRequestRefusalResult(err error) map[string]interface{} {
	message := "The in-chat request could not be created right now."
	var refusal *interactionRequestRefusal
	var validation *SkillSetupValidationError
	switch {
	case errors.As(err, &refusal):
		message = refusal.message
	case errors.As(err, &validation):
		message = validation.Error()
	}
	return map[string]interface{}{"refused": map[string]interface{}{
		"message":     message,
		"instruction": "Nothing was shown to the user. Explain this briefly in your reply and what the user can do instead. Do not repeat the same request in this reply.",
	}}
}

func interactionRequestRefused(call *ActionCall) bool {
	if call == nil || call.Status != ActionCallStatusSucceeded {
		return false
	}
	_, refused := call.Output["refused"].(map[string]interface{})
	return refused
}

func cloneCredentialReferences(input map[string]skill.CredentialReference) map[string]skill.CredentialReference {
	if input == nil {
		return nil
	}
	result := make(map[string]skill.CredentialReference, len(input))
	for name, reference := range input {
		result[name] = reference
	}
	return result
}
