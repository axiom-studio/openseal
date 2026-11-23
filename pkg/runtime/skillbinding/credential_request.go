package skillbinding

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// CredentialRequestWakeType parks the requesting Run until the user saves the
// credential or dismisses the request.
const CredentialRequestWakeType = "credential_request"

// RequestableCredentialType is one host credential type a person saves by
// hand, which request_credential may ask for. The host supplies the list; the
// kernel knows no vault types of its own.
type RequestableCredentialType struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Website: a saved credential of this type is for one website, which
	// the request must name (a website login).
	Website bool `json:"website,omitempty"`
	// NamedFields: the person fills fields the request names (a key-value
	// credential), so the request must name at least one.
	NamedFields bool `json:"namedFields,omitempty"`
}

// CredentialTypesRef holds the host's requestable credential types, which
// hosts install after the engine (and its action dispatchers) exist.
type CredentialTypesRef struct {
	mu    sync.RWMutex
	types []RequestableCredentialType
}

func (r *CredentialTypesRef) Set(types []RequestableCredentialType) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.types = append([]RequestableCredentialType(nil), types...)
	r.mu.Unlock()
}

func (r *CredentialTypesRef) get() []RequestableCredentialType {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.types
}

// UseCredentialTypes gives the dispatcher the host's requestable credential
// types for request_credential.
func (d *ActionDispatcher) UseCredentialTypes(ref *CredentialTypesRef) {
	if d != nil {
		d.credentialTypes = ref
	}
}

type credentialRequestArguments struct {
	// Type is the credential type. The argument is not named credentialType:
	// argument names containing "credential" are treated as secrets and
	// never persisted.
	Type    string                          `json:"type"`
	Service string                          `json:"service,omitempty"`
	Website string                          `json:"website,omitempty"`
	Fields  []models.CredentialRequestField `json:"fields,omitempty"`
	Reason  string                          `json:"reason"`
}

func credentialRequestAction() skill.Action {
	return skill.Action{Name: agentrun.SkillActionRequestCredential,
		Description: "Ask the user to save a credential in their vault through a durable in-chat card; it is saved there and never shown in chat. Use it whenever work needs a secret the user has (an API key, token, key pair, cloud or database credentials, a website login or a payment card) and no connected app (mcp.*) or Skill covers the service. Never ask for secret values in chat and never write them into files, the workspace or the sandbox. " +
			"type is a vault credential type. When no specific type exactly fits, or you are unsure, use generic: name each value in fields and the service in service. Use a specific type only when it exactly matches: integration_access for one API key or token, aws_credentials, database, http_basic_auth for a website login (website set to the exact page origin, for example after the live browser reports no_matching_login), payment_card to add a card or raise its spend cap (no_payment_card, spend_cap_exceeded). " +
			`Example: {"type":"generic","service":"Alpaca","fields":[{"name":"API Key ID","secure":true},{"name":"Secret Key","secure":true}],"reason":"Add your Alpaca keys so I can pull market data"}. ` +
			"The reason is shown to the user. A pending request pauses this work until the user saves the credential or dismisses the card; then continue with the saved credential, or explain the dismissal. If the result is refused, explain the reason to the user and do not repeat the same request.",
		Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
			"type":    map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128, "description": "Vault credential type: generic (named fields) unless a specific type exactly matches."},
			"service": map[string]interface{}{"type": "string", "maxLength": models.MaxCredentialRequestServiceName, "description": "Service the credential is for, shown to the user, e.g. Alpaca."},
			"website": map[string]interface{}{"type": "string", "description": "Exact http(s) origin of the site, required for http_basic_auth, e.g. https://www.amazon.in.", "maxLength": 512},
			"fields": map[string]interface{}{"type": "array", "maxItems": models.MaxCredentialRequestFields, "description": "For generic only, required there: each value the user fills in.",
				"items": map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
					"name":   map[string]interface{}{"type": "string", "minLength": 1, "maxLength": models.MaxCredentialRequestFieldName, "description": "Field name, e.g. Secret Key."},
					"secure": map[string]interface{}{"type": "boolean", "description": "true hides the value as it is typed; keep true unless the value is clearly not secret."},
				}, "required": []interface{}{"name", "secure"}}},
			"reason": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1000}}, "required": []interface{}{"type", "reason"}},
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

func decodeCredentialRequestArguments(arguments map[string]interface{}, types []RequestableCredentialType) (credentialRequestArguments, error) {
	var a credentialRequestArguments
	if err := decodeSkillBindingArguments(arguments, &a); err != nil {
		return a, refuseInteraction("the credential request arguments do not match the action schema")
	}
	if strings.TrimSpace(a.Reason) == "" || utf8.RuneCountInString(a.Reason) > 1000 {
		return a, refuseInteraction("reason must contain between 1 and 1000 characters")
	}
	if len(types) == 0 {
		return a, refuseInteraction("this host has no credential types that can be requested in chat")
	}
	var requested *RequestableCredentialType
	accepted := make([]string, 0, len(types))
	for index := range types {
		accepted = append(accepted, types[index].ID+" ("+types[index].Label+")")
		if types[index].ID == a.Type {
			requested = &types[index]
		}
	}
	if requested == nil {
		return a, refuseInteraction("type must be one of: " + strings.Join(accepted, ", "))
	}
	a.Service = strings.TrimSpace(a.Service)
	if utf8.RuneCountInString(a.Service) > models.MaxCredentialRequestServiceName {
		return a, refuseInteraction("service must contain at most 80 characters")
	}
	if requested.Website && strings.TrimSpace(a.Website) == "" {
		return a, refuseInteraction("website is required for " + requested.ID)
	}
	if strings.TrimSpace(a.Website) != "" {
		origin, err := models.NormalizeCredentialRequestWebsite(a.Website)
		if err != nil {
			return a, refuseInteraction(err.Error())
		}
		a.Website = origin
	}
	for index := range a.Fields {
		a.Fields[index].Name = strings.TrimSpace(a.Fields[index].Name)
	}
	switch {
	case requested.NamedFields && len(a.Fields) == 0:
		return a, refuseInteraction("fields must name at least one value for " + requested.ID)
	case !requested.NamedFields && len(a.Fields) > 0:
		return a, refuseInteraction(requested.ID + " has fixed fields; omit fields, or use a type whose fields you name")
	}
	if err := models.ValidateCredentialRequestFields(a.Fields); err != nil {
		return a, refuseInteraction(err.Error())
	}
	return a, nil
}

func (d *ActionDispatcher) requestCredential(ctx context.Context, input agentrun.ActionDispatchInput, run *models.AgentRun, member string, umbrella models.ObjectiveOwner) (map[string]interface{}, error) {
	a, err := decodeCredentialRequestArguments(input.Arguments, d.credentialTypes.get())
	if err != nil {
		return nil, err
	}
	store, ok := d.store.(models.CredentialRequestStore)
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
	now := time.Now().UTC()
	r := &models.CredentialRequest{ID: id, Scope: run.Scope, Owner: umbrella, DeploymentID: umbrella.ID, AgentID: member, ConversationID: conversationID, TriggerMessageID: messageID,
		RunID: run.ID, ActionCallID: input.Call.ID, CredentialType: a.Type, Service: a.Service, Website: a.Website, Fields: a.Fields, Reason: strings.TrimSpace(a.Reason),
		Status: models.CredentialRequestStatusPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
	for _, existing := range current {
		if existing.RunID == run.ID && existing.Status == models.CredentialRequestStatusPending && models.SameCredentialAsked(existing, r) {
			return credentialRequestResult(existing)
		}
	}
	if err := store.SaveCredentialRequest(ctx, r, 0); err != nil {
		if errors.Is(err, models.ErrCredentialRequestConflict) {
			if replay, readErr := store.GetCredentialRequest(ctx, run.Scope, id); readErr == nil && replay != nil {
				return credentialRequestResult(replay)
			}
		}
		return nil, err
	}
	return credentialRequestResult(r)
}

func credentialRequestResult(r *models.CredentialRequest) (map[string]interface{}, error) {
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

// PendingCredentialRequestFromAction returns the request a succeeded
// request_credential call created when it is still waiting for the user.
func PendingCredentialRequestFromAction(ctx context.Context, store interface{}, call *models.ActionCall) *models.CredentialRequest {
	if call == nil || call.Status != models.ActionCallStatusSucceeded || call.SkillID != models.SkillManagementSkillID || call.Action != agentrun.SkillActionRequestCredential {
		return nil
	}
	value, ok := call.Output["credentialRequest"].(map[string]interface{})
	if !ok {
		return nil
	}
	id, _ := value["id"].(string)
	requests, ok := store.(models.CredentialRequestStore)
	if !ok || id == "" {
		return nil
	}
	// Read the durable record: the user may have saved the credential between
	// dispatch and this commit, and a settled request must not park the Run.
	current, err := requests.GetCredentialRequest(ctx, call.Scope, id)
	if err != nil || current == nil || current.RunID != call.RunID || current.Status != models.CredentialRequestStatusPending {
		return nil
	}
	return current
}

// CredentialRequestResolutionInstruction is the durable guidance given to a
// Run woken by its credential request.
func CredentialRequestResolutionInstruction(r *models.CredentialRequest) string {
	subject := "the requested " + r.CredentialType + " credential"
	switch {
	case r.Website != "" && r.Service != "":
		subject += " for " + r.Service + " (" + r.Website + ")"
	case r.Website != "":
		subject += " for " + r.Website
	case r.Service != "":
		subject += " for " + r.Service
	}
	if r.Status == models.CredentialRequestStatusDismissed || r.Credential == nil {
		return "The user dismissed the request to save " + subject + ". Do not request it again in this reply; explain what remains blocked and offer an alternative."
	}
	return "The user saved " + subject + " in the vault as \"" + r.Credential.Name + "\" (credential id " + r.Credential.ID + "). Continue the task from its saved progress and retry the step that needed it (for a website login or card, call the live browser sign-in or card fill again, reopening the site first if the browser session closed). Never ask for its values in chat or write them into files."
}

// interactionRequestRefusal is a user-correctable reason an in-chat
// interaction could not be created. The model receives it as a result and
// explains it; it never ends the Run.
type interactionRequestRefusal struct{ message string }

func (e *interactionRequestRefusal) Error() string { return e.message }

func refuseInteraction(message string) error { return &interactionRequestRefusal{message: message} }

// IsInteractionRequestAction reports whether name asks the user for setup, a
// credential or a connection.
func IsInteractionRequestAction(name string) bool {
	return name == agentrun.SkillActionRequestSetup || name == agentrun.SkillActionRequestCredential || name == agentrun.SkillActionRequestConnection
}

// interactionRequestRefusalResult turns any failure to create an in-chat
// interaction into a model-visible result. A failed form request is a fact to
// explain to the user, not a reason to stop the attempt.
func interactionRequestRefusalResult(err error) map[string]interface{} {
	message := "The in-chat request could not be created right now."
	var refusal *interactionRequestRefusal
	var validation *models.SkillSetupValidationError
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

// InteractionRequestRefused reports whether call succeeded with a refusal
// instead of showing the user a card.
func InteractionRequestRefused(call *models.ActionCall) bool {
	if call == nil || call.Status != models.ActionCallStatusSucceeded {
		return false
	}
	_, refused := call.Output["refused"].(map[string]interface{})
	return refused
}
