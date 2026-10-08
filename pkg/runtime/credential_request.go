package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// SkillActionRequestCredential asks the user, through a durable in-chat card,
// to save a credential in the host's vault. It is not Skill setup: nothing
// about a binding changes. The request never carries a secret value.
const SkillActionRequestCredential = "request_credential"

// CredentialRequestWakeType parks the requesting Run until the user saves the
// credential or dismisses the request.
const CredentialRequestWakeType = "credential_request"

const (
	CredentialRequestKindWebsiteLogin = "website_login"
	CredentialRequestKindPaymentCard  = "payment_card"
)

const (
	CredentialRequestStatusPending   = "pending"
	CredentialRequestStatusResolved  = "resolved"
	CredentialRequestStatusDismissed = "dismissed"
)

var ErrCredentialRequestConflict = errors.New("credential request revision conflict")
var ErrInvalidCredentialRequest = errors.New("invalid credential request")

// CredentialRequest is a user interaction for one missing credential. The
// host decides what evidence completes it (a matching vault credential saved
// after the request); the kernel only records the outcome and wakes the Run.
type CredentialRequest struct {
	ID               string    `json:"id"`
	Scope            Scope     `json:"scope"`
	DeploymentID     string    `json:"deploymentId"`
	ConversationID   string    `json:"conversationId"`
	TriggerMessageID string    `json:"triggerMessageId"`
	RunID            string    `json:"runId"`
	ActionCallID     string    `json:"actionCallId"`
	Kind             string    `json:"kind"`
	Website          string    `json:"website,omitempty"`
	Reason           string    `json:"reason"`
	Status           string    `json:"status"`
	Revision         int64     `json:"revision"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	ResolvedBy       string    `json:"resolvedBy,omitempty"`
}

func (r *CredentialRequest) Validate() error {
	if r == nil || r.Scope.Validate() != nil || r.Revision < 1 || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return ErrInvalidCredentialRequest
	}
	for _, value := range []string{r.ID, r.DeploymentID, r.ConversationID, r.TriggerMessageID, r.RunID, r.ActionCallID} {
		if !validOpaqueIdentifier(value, 256) {
			return ErrInvalidCredentialRequest
		}
	}
	if strings.TrimSpace(r.Reason) == "" || utf8.RuneCountInString(r.Reason) > 1000 {
		return ErrInvalidCredentialRequest
	}
	switch r.Kind {
	case CredentialRequestKindWebsiteLogin:
		if origin, err := NormalizeCredentialRequestWebsite(r.Website); err != nil || origin != r.Website {
			return ErrInvalidCredentialRequest
		}
	case CredentialRequestKindPaymentCard:
		if r.Website != "" {
			if origin, err := NormalizeCredentialRequestWebsite(r.Website); err != nil || origin != r.Website {
				return ErrInvalidCredentialRequest
			}
		}
	default:
		return ErrInvalidCredentialRequest
	}
	switch r.Status {
	case CredentialRequestStatusPending:
		if r.ResolvedBy != "" {
			return ErrInvalidCredentialRequest
		}
	case CredentialRequestStatusResolved, CredentialRequestStatusDismissed:
		if strings.TrimSpace(r.ResolvedBy) == "" {
			return ErrInvalidCredentialRequest
		}
	default:
		return ErrInvalidCredentialRequest
	}
	return nil
}

// NormalizeCredentialRequestWebsite returns the exact http(s) origin
// (scheme://host[:port]) of a website, rejecting paths, queries and userinfo.
func NormalizeCredentialRequestWebsite(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("website must be an http(s) origin such as https://www.example.com")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("website must be an http(s) origin such as https://www.example.com")
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port == "" || (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return scheme + "://" + host, nil
	}
	return scheme + "://" + host + ":" + port, nil
}

type CredentialRequestStore interface {
	GetCredentialRequest(context.Context, Scope, string) (*CredentialRequest, error)
	ListCredentialRequests(context.Context, Scope, string, string) ([]*CredentialRequest, error)
	SaveCredentialRequest(context.Context, *CredentialRequest, int64) error
}

func cloneCredentialRequest(r *CredentialRequest) *CredentialRequest {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

type credentialRequestArguments struct {
	Kind    string `json:"kind"`
	Website string `json:"website,omitempty"`
	Reason  string `json:"reason"`
}

func credentialRequestAction() skill.Action {
	return skill.Action{Name: SkillActionRequestCredential,
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

func (d *SkillBindingActionDispatcher) requestCredential(ctx context.Context, input ActionDispatchInput, run *AgentRun, deploymentID string) (map[string]interface{}, error) {
	a, err := decodeCredentialRequestArguments(input.Arguments)
	if err != nil {
		return nil, err
	}
	store, ok := d.store.(CredentialRequestStore)
	if !ok {
		return nil, refuseInteraction("in-chat credential requests are unavailable on this host")
	}
	conversationID, messageID, err := d.skillSetupConversationOrigin(ctx, run, deploymentID)
	if err != nil {
		return nil, refuseInteraction("a credential can only be requested from a conversation with the user")
	}
	id := "credential-request:" + input.Call.ID
	if existing, err := store.GetCredentialRequest(ctx, run.Scope, id); err != nil {
		return nil, err
	} else if existing != nil {
		return credentialRequestResult(existing)
	}
	// The same unanswered card is reused rather than shown twice.
	current, err := store.ListCredentialRequests(ctx, run.Scope, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	for _, r := range current {
		if r.Status == CredentialRequestStatusPending && r.RunID == run.ID && r.Kind == a.Kind && r.Website == a.Website {
			return credentialRequestResult(r)
		}
	}
	now := time.Now().UTC()
	r := &CredentialRequest{ID: id, Scope: run.Scope, DeploymentID: deploymentID, ConversationID: conversationID, TriggerMessageID: messageID,
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
	if call == nil || call.Status != ActionCallStatusSucceeded || call.SkillID != SkillManagementSkillID || call.Action != SkillActionRequestCredential {
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
	return name == SkillActionRequestSetup || name == SkillActionRequestCredential
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
