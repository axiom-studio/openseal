package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/axiom-studio/openseal/pkg/skill"
	"golang.org/x/mod/semver"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
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
		Description: "Ask the user to install, configure, or reauthorize an exact Skill through a durable in-chat setup form. Use discover first and copy the returned id and sourceIdentity exactly; publisher namespace spelling does not authorize a target. Request skillVersion latest; the host resolves and records the authorized current version. A verified existing account retains its binding ID and revision, while an older executable version requires a separately reviewed canonical binding upgrade before configuration or reauthorization can be completed. For example, configure the discovered Skill with skillVersion latest and omit requiredActions for a general account connection. For a general connection request, omit requiredActions, enablePrompt, and bindingId unless the user requested specific operations or a verified existing binding. Compatibility requirements and configuration fields are not action names. A successful pending request needs the user to complete its form. In an independent conversation task, preserve the unfinished work with waiting_for_event and wakeCondition type skill_setup referencing the exact saved setupRequest.id. In an ordinary foreground reply, finish without polling. If setup fails, explain the failure; do not repeat the same setup in this reply. This action creates a request only; it does not grant access, install anything, or verify a connection. Never request secrets in chat or invent authorization URLs. Use reauthorize for an existing binding whose credentials need replacement. Use configure to connect an installed Skill or change its configuration. Use install for a verified discoverable Skill that needs installation.",
		Risk:        skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
			"requiredActions": map[string]interface{}{"description": "Optional exact names from the discovered candidate actions[].name when specific provider operations are requested. Never put compatibility requirements, credential kinds, or configuration field names here. Omit for a general connection request.", "type": "array", "items": map[string]interface{}{"type": "string", "minLength": 1}, "maxItems": 32, "uniqueItems": true},
			"enablePrompt":    map[string]interface{}{"description": "Optional; only true when discovered promptAvailable is true and instructions are needed.", "type": "boolean"},
			"kind":            map[string]interface{}{"type": "string", "enum": []interface{}{"install", "configure", "reauthorize"}},
			"skillId":         map[string]interface{}{"type": "string", "minLength": 1}, "skillVersion": map[string]interface{}{"type": "string", "minLength": 1},
			"sourceIdentity": map[string]interface{}{"type": "string"}, "bindingId": map[string]interface{}{"description": "Optional verified existing binding ID. Omit for a new connection; the host finds an existing exact binding. Do not invent a new binding ID.", "type": "string"},
			"reason": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1000}}, "required": []interface{}{"kind", "skillId", "skillVersion", "reason"}},
		OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"setupRequest": map[string]interface{}{"type": "object"}}, "required": []interface{}{"setupRequest"}}}
}
func decodeSkillSetupArguments(arguments map[string]interface{}) (skillSetupArguments, error) {
	var a skillSetupArguments
	if err := decodeSkillBindingArguments(arguments, &a); err != nil {
		return a, invalidSkillSetupField("arguments", "must match the setup action schema")
	}
	if !validOpaqueIdentifier(a.SkillID, 256) {
		return a, invalidSkillSetupField("skillId", "must be copied from authorized discovery")
	}
	if !validOpaqueIdentifier(a.SkillVersion, 256) {
		return a, invalidSkillSetupField("skillVersion", "must be an exact discovered version or latest")
	}
	if strings.TrimSpace(a.Reason) == "" || utf8.RuneCountInString(a.Reason) > 1000 {
		return a, invalidSkillSetupField("reason", "must contain between 1 and 1000 characters")
	}
	switch a.Kind {
	case "install", "configure", "reauthorize":
	default:
		return a, invalidSkillSetupField("kind", "must be install, configure, or reauthorize")
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
	conversationID, messageID, err := d.skillSetupConversationOrigin(ctx, run, deploymentID)
	if err != nil {
		return nil, err
	}
	var task *ConversationTask
	if run.Kind == RunKindAgentWork && run.ParentRunID == "" {
		proof, ok := d.store.(ConversationTaskProofStore)
		if !ok {
			return nil, ErrInvalidSkillSetup
		}
		canonical, readErr := d.store.GetAgentRun(ctx, run.Scope, run.ID)
		if readErr != nil {
			return nil, readErr
		}
		task, err = conversationTaskSetupOrigin(ctx, proof, canonical)
		if err != nil {
			return nil, err
		}
		if task == nil || task.TargetAgentID != deploymentID || input.Call == nil {
			return nil, ErrInvalidSkillSetup
		}
		call, callErr := d.store.GetActionCall(ctx, run.Scope, input.Call.ID)
		if callErr != nil {
			return nil, callErr
		}
		if !taskSetupActionMatches(call, run, deploymentID) || call.Status != ActionCallStatusRunning ||
			input.Call.RunID != run.ID || input.Call.Scope != run.Scope {
			return nil, ErrInvalidSkillSetup
		}
	}
	id := "skill-setup:" + input.Call.ID
	existing, err := d.store.GetSkillSetupRequest(ctx, run.Scope, id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if task != nil && (existing.Validate() != nil || existing.ID != id || existing.Scope != run.Scope || existing.ActionCallID != input.Call.ID || existing.RunID != run.ID || existing.ConversationID != conversationID || existing.TriggerMessageID != messageID || existing.DeploymentID != deploymentID) {
			return nil, ErrInvalidSkillSetup
		}
		return skillSetupResult(existing)
	}
	// A new model turn must not restart an identical failed setup operation
	// inside the same reply. A later user-triggered run can retry after repair.
	failed, err := d.store.ListActionCalls(ctx, ActionFilter{Scope: run.Scope, RunID: run.ID, Status: []ActionCallStatus{ActionCallStatusFailed}})
	if err != nil {
		return nil, err
	}
	for _, call := range failed {
		if call.ID == input.Call.ID || call.DeploymentID != deploymentID || call.Action != SkillActionRequestSetup || call.SkillID != SkillManagementSkillID {
			continue
		}
		previous, decodeErr := decodeSkillSetupArguments(call.Arguments)
		if decodeErr == nil && previous.SkillID == a.SkillID && previous.SourceIdentity == a.SourceIdentity && previous.SkillVersion == a.SkillVersion && previous.Kind == a.Kind && previous.BindingID == a.BindingID {
			return nil, errors.New("this setup already failed in this reply; explain the failure and wait for the user to retry after it is fixed")
		}
	}
	scope := skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	// Account identity and the prospective executable target are separate.
	// Resolve an existing account before choosing latest so retained versions
	// never look like a missing account or silently create another connection.
	var currentBinding *skill.Binding
	if a.BindingID != "" {
		currentBinding, err = d.catalog.GetBinding(ctx, scope, deploymentID, a.BindingID)
		if err != nil {
			return nil, err
		}
		if currentBinding == nil || currentBinding.SkillID != a.SkillID || currentBinding.SourceIdentity != a.SourceIdentity {
			return nil, errors.New("setup account does not match this Agent's exact Skill source")
		}
	} else {
		bindings, listErr := d.catalog.ListBindings(ctx, scope, deploymentID)
		if listErr != nil {
			return nil, listErr
		}
		for _, binding := range bindings {
			if binding.SkillID != a.SkillID || binding.SourceIdentity != a.SourceIdentity || binding.Disabled {
				continue
			}
			if currentBinding != nil {
				return nil, errors.New("more than one account matches; choose the exact bindingId")
			}
			currentBinding = binding
			a.BindingID = binding.ID
		}
	}
	if a.Kind == "reauthorize" && currentBinding == nil {
		return nil, errors.New("no current binding exists; request configuration first")
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
			if item.ID == a.SkillID && (a.SkillVersion == "latest" || item.Version == a.SkillVersion) && item.SourceIdentity == a.SourceIdentity {
				if a.SkillVersion == "latest" {
					if a.Kind != "install" && item.Readiness != skill.DiscoveryReadinessBindable && !skillCandidateNeedsConfigurationSetup(&item) {
						continue
					}
					if !semver.IsValid("v" + strings.TrimPrefix(item.Version, "v")) {
						return nil, errors.New("latest Skill selection requires valid versions")
					}
					if candidate != nil && semver.Compare("v"+strings.TrimPrefix(item.Version, "v"), "v"+strings.TrimPrefix(candidate.Version, "v")) <= 0 {
						continue
					}
				}
				copy := item
				candidate = &copy
				if a.SkillVersion != "latest" {
					break
				}
			}
		}
		if candidate != nil && a.SkillVersion != "latest" || page.NextCursor == "" {
			break
		}
		if pageNumber == 19 {
			return nil, errors.New("Skill discovery exceeded the setup selection page limit")
		}
		if page.NextCursor == cursor {
			return nil, errors.New("Skill discovery cursor did not advance")
		}
		cursor = page.NextCursor
	}
	if candidate != nil {
		a.SkillVersion = candidate.Version
	}
	configurationSetup := skillCandidateNeedsConfigurationSetup(candidate) && a.Kind != "install"
	if candidate == nil || (candidate.Readiness == skill.DiscoveryReadinessUnavailable && !configurationSetup) {
		return nil, errors.New("requested Skill is not available in the authorized catalog")
	}
	if a.Kind != "install" && candidate.Readiness != skill.DiscoveryReadinessBindable && !configurationSetup {
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
	phase := SkillSetupPhaseConfiguration
	var bindingRevision int64
	bindingVersion := ""
	if currentBinding != nil {
		bindingRevision = currentBinding.Revision
		bindingVersion = currentBinding.SkillVersion
		if currentBinding.SkillVersion != a.SkillVersion {
			if currentBinding.Disabled {
				return nil, errors.New("a disabled account cannot be upgraded; enable its exact binding before selecting a new Skill version")
			}
			phase = SkillSetupPhaseBindingUpgrade
		}
	}
	// Repeated model requests for the same pending interaction reuse its identity.
	pending, err := d.store.ListSkillSetupRequests(ctx, run.Scope, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	for _, r := range pending {
		if task != nil && (r.RunID != run.ID || r.TriggerMessageID != messageID) {
			continue
		}
		if r.Status == "pending" && r.Kind == a.Kind && r.SkillID == a.SkillID && r.SkillVersion == a.SkillVersion && r.SourceIdentity == a.SourceIdentity && r.BindingID == a.BindingID && r.BindingRevision == bindingRevision && (r.Phase == phase || (r.Phase == "" && phase == SkillSetupPhaseConfiguration)) && (r.BindingVersion == bindingVersion || (r.BindingVersion == "" && phase == SkillSetupPhaseConfiguration)) && slices.Equal(r.RequiredActions, a.RequiredActions) && r.EnablePrompt == a.EnablePrompt {
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
	r := &SkillSetupRequest{InstallationReference: reference, SourceDigest: digest, RequiredActions: a.RequiredActions, EnablePrompt: a.EnablePrompt, ID: id, Scope: run.Scope, DeploymentID: deploymentID, ConversationID: conversationID, TriggerMessageID: messageID, RunID: run.ID, ActionCallID: input.Call.ID, Kind: a.Kind, SkillID: a.SkillID, SkillVersion: a.SkillVersion, SourceIdentity: a.SourceIdentity, SkillName: candidate.Name, BindingID: a.BindingID, BindingRevision: bindingRevision, Phase: phase, BindingVersion: bindingVersion, Reason: a.Reason, Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
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
	return skill.Action{Name: SkillActionListSetupRequests, Description: "Read the current conversation's durable Skill setup requests and their pending, resolved, or dismissed status. Use this once after the user reports completing setup; resolved means account configuration was saved, not that provider actions were authorized or verified. Inspect bindingAccess.enabledActions before choosing an operation. If discovery declares an action that is not enabled on the saved binding, request setup for that exact action and explain the access needed; do not call this a missing account or missing credential. A pending request needs user input: an independent conversation task should wait with wakeCondition type skill_setup referencing the exact saved request ID; an ordinary foreground reply should finish. Do not poll it. Retry the requested provider operation through its normal authorized action to verify it.", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 2}, InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false}, OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"requests": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}}}, "required": []interface{}{"requests"}, "additionalProperties": false}}
}
func (d *SkillBindingActionDispatcher) listSkillSetupRequests(ctx context.Context, run *AgentRun, deploymentID string) (map[string]interface{}, error) {
	conversationID, _, err := d.skillSetupConversationOrigin(ctx, run, deploymentID)
	if err != nil {
		return nil, err
	}
	var task *ConversationTask
	if run.Kind == RunKindAgentWork && run.ParentRunID == "" {
		proof, ok := d.store.(ConversationTaskProofStore)
		if !ok {
			return nil, ErrInvalidSkillSetup
		}
		canonical, readErr := d.store.GetAgentRun(ctx, run.Scope, run.ID)
		if readErr != nil {
			return nil, readErr
		}
		task, err = conversationTaskSetupOrigin(ctx, proof, canonical)
		if err != nil {
			return nil, err
		}
		if task == nil || task.TargetAgentID != deploymentID {
			return nil, ErrInvalidSkillSetup
		}
	}
	requests, err := d.store.ListSkillSetupRequests(ctx, run.Scope, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	values := make([]interface{}, 0, len(requests))
	for _, r := range requests {
		if task != nil && (r.RunID != run.ID || r.TriggerMessageID != task.SourceMessageID) {
			continue
		}
		value, err := skillSetupResult(r)
		if err != nil {
			return nil, err
		}
		item := value["setupRequest"].(map[string]interface{})
		bindingID := r.ResolvedBindingID
		if bindingID == "" {
			bindingID = r.BindingID
		}
		if bindingID != "" {
			binding, bindingErr := d.catalog.GetBinding(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, deploymentID, bindingID)
			if bindingErr != nil {
				return nil, bindingErr
			}
			if binding != nil && binding.SkillID == r.SkillID && binding.SkillVersion == r.SkillVersion && binding.SourceIdentity == r.SourceIdentity {
				enabledActions := []string{}
				if !binding.Disabled {
					enabledActions = append(enabledActions, binding.AllowedActions...)
				}
				sort.Strings(enabledActions)
				item["bindingAccess"] = map[string]interface{}{"enabled": !binding.Disabled, "enabledActions": enabledActions, "promptEnabled": !binding.Disabled && binding.EnablePrompt}
			}
		}
		values = append(values, item)
	}
	return map[string]interface{}{"requests": values}, nil
}

// Missing credentials or binding configuration block execution, but must not
// block the request that collects them. Other incompatibilities still reject setup; requesting setup
// never activates a binding or grants access.
func skillCandidateNeedsConfigurationSetup(candidate *skill.DiscoveryCandidate) bool {
	if candidate == nil || candidate.Readiness != skill.DiscoveryReadinessUnavailable {
		return false
	}
	missing := false
	for _, check := range candidate.Compatibility {
		if check.Compatible {
			continue
		}
		if check.Requirement != "binding_configuration" && (!strings.HasPrefix(check.Requirement, "credential:") || strings.TrimPrefix(check.Requirement, "credential:") == "") {
			return false
		}
		missing = true
	}
	return missing
}
