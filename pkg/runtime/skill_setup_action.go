package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/axiom-studio/openseal/pkg/skill"
	"slices"
	"sort"
	"strings"
	"time"
)

const SkillActionRequestSetup = "request_setup"
const SkillActionListSetupRequests = "list_setup_requests"

type skillSetupArguments struct {
	RequiredActions []string `json:"requiredActions,omitempty"`
	EnablePrompt    bool     `json:"enablePrompt,omitempty"`
	Kind            string   `json:"kind"`
	SkillID         string   `json:"skillId"`
	SkillVersion    string   `json:"skillVersion"`
	SourceIdentity  string   `json:"sourceIdentity,omitempty"`
	BindingID       string   `json:"bindingId,omitempty"`
	Reason          string   `json:"reason"`
}

func skillSetupAction() skill.Action {
	return skill.Action{Name: SkillActionRequestSetup,
		Description: "Ask the user to install, configure, or reauthorize an exact Skill through a durable in-chat setup form. Use discover first for exact Skill identities. This action creates a request only; it does not grant access, install anything, or verify a connection. Never request secrets in chat or invent authorization URLs. Use reauthorize for an existing binding whose credentials need replacement. Use configure to connect an installed Skill or change its configuration. Use install for a verified discoverable Skill that needs installation.",
		Risk:        skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
			"requiredActions": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1}, "maxItems": 32, "uniqueItems": true},
			"enablePrompt":    map[string]interface{}{"type": "boolean"},
			"kind":            map[string]interface{}{"type": "string", "enum": []interface{}{"install", "configure", "reauthorize"}},
			"skillId":         map[string]interface{}{"type": "string", "minLength": 1}, "skillVersion": map[string]interface{}{"type": "string", "minLength": 1},
			"sourceIdentity": map[string]interface{}{"type": "string"}, "bindingId": map[string]interface{}{"type": "string"},
			"reason": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1000}}, "required": []interface{}{"kind", "skillId", "skillVersion", "reason"}},
		OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"setupRequest": map[string]interface{}{"type": "object"}}, "required": []interface{}{"setupRequest"}}}
}
func decodeSkillSetupArguments(arguments map[string]interface{}) (skillSetupArguments, error) {
	var a skillSetupArguments
	if err := decodeSkillBindingArguments(arguments, &a); err != nil {
		return a, err
	}
	if a.SkillID == "" || a.SkillVersion == "" || strings.TrimSpace(a.Reason) == "" || len(a.Reason) > 1000 || strings.HasPrefix(a.SkillID, "openseal.") {
		return a, ErrInvalidSkillSetup
	}
	switch a.Kind {
	case "install", "configure", "reauthorize":
	default:
		return a, ErrInvalidSkillSetup
	}
	sort.Strings(a.RequiredActions)
	a.RequiredActions = slices.Compact(a.RequiredActions)
	return a, nil
}
func (d *SkillBindingActionDispatcher) requestSkillSetup(ctx context.Context, input ActionDispatchInput, run *AgentRun, deploymentID string) (map[string]interface{}, error) {
	a, err := decodeSkillSetupArguments(input.Arguments)
	if err != nil {
		return nil, err
	}
	conversationID, _ := run.Context["conversationId"].(string)
	messageID, _ := run.Context["triggerMessageId"].(string)
	if run.Kind != RunKindConversation || conversationID == "" || messageID == "" {
		return nil, errors.New("Skill setup requests require an Agent conversation")
	}
	id := "skill-setup:" + input.Call.ID
	existing, err := d.store.GetSkillSetupRequest(ctx, run.Scope, id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return skillSetupResult(existing)
	}
	// Resolve exact identity against the host's authorized catalog, never model prose.
	if d.discovery == nil {
		return nil, errors.New("authorized Skill discovery is unavailable")
	}
	var candidate *skill.DiscoveryCandidate
	cursor := ""
	for pageNumber := 0; pageNumber < 20; pageNumber++ {
		request := skill.DiscoveryRequest{Scope: skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, DeploymentID: deploymentID, Query: a.SkillID, Limit: skill.MaximumDiscoveryLimit, Cursor: cursor}
		page, err := d.discovery.DiscoverSkills(ctx, request)
		if err != nil {
			return nil, err
		}
		page, err = skill.NormalizeDiscoveryPage(request, page)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item.ID == a.SkillID && item.Version == a.SkillVersion && item.SourceIdentity == a.SourceIdentity {
				copy := item
				candidate = &copy
				break
			}
		}
		if candidate != nil || page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, errors.New("Skill discovery cursor did not advance")
		}
		cursor = page.NextCursor
	}
	if candidate == nil || candidate.Readiness == skill.DiscoveryReadinessUnavailable {
		return nil, errors.New("requested Skill is not available in the authorized catalog")
	}
	if a.Kind != "install" && candidate.Readiness != skill.DiscoveryReadinessBindable {
		return nil, errors.New("Skill must be installed before configuration")
	}
	for _, name := range a.RequiredActions {
		found := false
		for _, action := range candidate.Actions {
			if action.Name == name {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("requested action is not supplied by the selected Skill")
		}
	}
	if a.EnablePrompt && !candidate.PromptAvailable {
		return nil, errors.New("selected Skill does not supply prompt instructions")
	}
	if a.BindingID == "" {
		bindings, err := d.catalog.ListBindings(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, deploymentID)
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings {
			if binding.SkillID == a.SkillID && binding.SkillVersion == a.SkillVersion && binding.SourceIdentity == a.SourceIdentity && !binding.Disabled {
				if a.BindingID != "" {
					return nil, errors.New("more than one account matches; choose the exact bindingId")
				}
				a.BindingID = binding.ID
			}
		}
	}
	if a.Kind == "reauthorize" && a.BindingID == "" {
		return nil, errors.New("no current binding exists; request configuration first")
	}
	var bindingRevision int64
	if a.BindingID != "" {
		binding, err := d.catalog.GetBinding(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, deploymentID, a.BindingID)
		if err != nil {
			return nil, err
		}
		if binding == nil || binding.SkillID != a.SkillID || binding.SkillVersion != a.SkillVersion || binding.SourceIdentity != a.SourceIdentity {
			return nil, errors.New("setup target does not match this Agent's binding")
		}
		bindingRevision = binding.Revision
	}
	// Repeated model requests for the same pending interaction reuse its identity.
	pending, err := d.store.ListSkillSetupRequests(ctx, run.Scope, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	for _, r := range pending {
		if r.Status == "pending" && r.Kind == a.Kind && r.SkillID == a.SkillID && r.SkillVersion == a.SkillVersion && r.SourceIdentity == a.SourceIdentity && r.BindingID == a.BindingID && r.BindingRevision == bindingRevision && slices.Equal(r.RequiredActions, a.RequiredActions) && r.EnablePrompt == a.EnablePrompt {
			return skillSetupResult(r)
		}
	}
	reference, digest := "", ""
	if a.Kind == "install" {
		for _, check := range candidate.Compatibility {
			if check.Requirement == "installation" && !check.Compatible {
				reference = check.Reference
			}
			if check.Requirement == "source_digest" && check.Compatible {
				digest = check.Reference
			}
		}
		if candidate.Readiness == skill.DiscoveryReadinessNeedsInstallation && (reference == "" || digest == "" || a.SourceIdentity == "") {
			return nil, errors.New("Skill installation requires a verified source receipt")
		}
	}
	now := time.Now().UTC()
	r := &SkillSetupRequest{InstallationReference: reference, SourceDigest: digest, RequiredActions: a.RequiredActions, EnablePrompt: a.EnablePrompt, ID: id, Scope: run.Scope, DeploymentID: deploymentID, ConversationID: conversationID, TriggerMessageID: messageID, RunID: run.ID, ActionCallID: input.Call.ID, Kind: a.Kind, SkillID: a.SkillID, SkillVersion: a.SkillVersion, SourceIdentity: a.SourceIdentity, SkillName: candidate.Name, BindingID: a.BindingID, BindingRevision: bindingRevision, Reason: a.Reason, Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := d.store.SaveSkillSetupRequest(ctx, r, 0); err != nil {
		if errors.Is(err, ErrSkillSetupConflict) {
			if replay, readErr := d.store.GetSkillSetupRequest(ctx, run.Scope, id); readErr == nil && replay != nil {
				return skillSetupResult(replay)
			}
		}
		return nil, err
	}
	return skillSetupResult(r)
}
func skillSetupResult(r *SkillSetupRequest) (map[string]interface{}, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var value map[string]interface{}
	if err = json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return map[string]interface{}{"setupRequest": value}, nil
}

func skillListSetupRequestsAction() skill.Action {
	return skill.Action{Name: SkillActionListSetupRequests, Description: "Read the current conversation's durable Skill setup requests and their pending, resolved, or dismissed status. Use this after the user reports completing setup; resolved means configuration was saved, not that a provider operation has succeeded. Retry the requested provider operation through its normal authorized action to verify it.", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 2}, InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false}, OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"requests": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}}}, "required": []interface{}{"requests"}, "additionalProperties": false}}
}
func (d *SkillBindingActionDispatcher) listSkillSetupRequests(ctx context.Context, run *AgentRun, deploymentID string) (map[string]interface{}, error) {
	conversationID, _ := run.Context["conversationId"].(string)
	if run.Kind != RunKindConversation || conversationID == "" {
		return nil, errors.New("Skill setup requests require an Agent conversation")
	}
	requests, err := d.store.ListSkillSetupRequests(ctx, run.Scope, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	values := make([]interface{}, 0, len(requests))
	for _, r := range requests {
		value, err := skillSetupResult(r)
		if err != nil {
			return nil, err
		}
		values = append(values, value["setupRequest"])
	}
	return map[string]interface{}{"requests": values}, nil
}
