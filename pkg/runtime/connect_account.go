package runtime

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// CredentialErrorPersonUnavailable is the error code of an action that uses
// each person's own account when no person asked for its run.
const CredentialErrorPersonUnavailable = "person_unavailable"

// connectAccountRequiredPrefix starts the error of a call parked on a
// connect_account card.
const connectAccountRequiredPrefix = "connect_account_required: "

// connectAccountRequestPrefix is the ID prefix of a connect_account card; the
// rest is the parked action call's ID.
const connectAccountRequestPrefix = "connect-account:"

// personUnavailableError fails an action whose per_person account has no
// person to act for. It is returned to the model like any failure, with no
// card.
type personUnavailableError struct{ message string }

func (e *personUnavailableError) Error() string { return e.message }

// actionCredentialRequirements are the bound action's credential
// requirements by slot.
func actionCredentialRequirements(bound *skill.BoundAction) map[string]skill.CredentialRequirement {
	if bound == nil || len(bound.Action.Credentials) == 0 {
		return nil
	}
	requirements := make(map[string]skill.CredentialRequirement, len(bound.Action.Credentials))
	for _, requirement := range bound.Action.Credentials {
		requirements[requirement.Name] = requirement
	}
	return requirements
}

func callUsesPersonCredential(call *models.ActionCall) bool {
	for _, reference := range call.CredentialRefs {
		if skill.IsPersonCredentialReference(reference) {
			return true
		}
	}
	return false
}

func personUnavailable(bound *skill.BoundAction, umbrella models.ObjectiveOwner) error {
	return &personUnavailableError{message: boundSkillName(bound) + " uses each person's own account, and no person asked for this run. Ask someone who manages " + umbrellaLabel(umbrella) + " to share an account with it."}
}

func boundSkillName(bound *skill.BoundAction) string {
	if bound != nil && bound.Definition != nil {
		if name := strings.TrimSpace(bound.Definition.Name); name != "" {
			return name
		}
		return bound.Definition.ID
	}
	return "This Skill"
}

// umbrellaLabel names an umbrella in user-facing text.
func umbrellaLabel(umbrella models.ObjectiveOwner) string {
	if umbrella.Type == models.OwnerTypeTeam {
		return "this Team"
	}
	return "this Agent"
}

func connectAccountSlots(bound *skill.BoundAction, names []string) []models.ConnectAccountSlot {
	sort.Strings(names)
	requirements := actionCredentialRequirements(bound)
	slots := make([]models.ConnectAccountSlot, 0, len(names))
	for _, name := range names {
		requirement, ok := requirements[name]
		if !ok {
			requirement = skill.CredentialRequirement{Name: name}
		}
		slots = append(slots, models.ConnectAccountSlot{Name: name, Requirement: requirement})
	}
	return slots
}

// ConnectAccountProvider is the account provider a connect_account card names:
// the OAuth provider of its first slot, or its Skill name.
func ConnectAccountProvider(r *models.SkillSetupRequest) string {
	if r == nil {
		return ""
	}
	for _, slot := range r.Slots {
		if slot.Requirement.OAuth2 != nil {
			if provider := strings.TrimSpace(slot.Requirement.OAuth2.Provider); provider != "" {
				return capitalizeFirst(provider)
			}
		}
	}
	return r.SkillName
}

// createConnectAccountRequest saves (or replays) the connect_account card for
// a parked call. The card belongs to the run's proven conversation; without
// one the call fails as person_unavailable.
func createConnectAccountRequest(ctx context.Context, store models.KernelStore, run *models.AgentRun, call *models.ActionCall, bound *skill.BoundAction, umbrella models.ObjectiveOwner, slots []models.ConnectAccountSlot, mode string) (*models.SkillSetupRequest, error) {
	id := connectAccountRequestPrefix + call.ID
	if existing, err := store.GetSkillSetupRequest(ctx, run.Scope, id); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	conversationID, messageID, err := skillSetupConversationOrigin(ctx, store, run, run.AssignedAgentID, umbrella)
	if err != nil {
		return nil, &personUnavailableError{message: boundSkillName(bound) + " needs an account connected, and this run has no conversation to ask in. Ask someone who manages " + umbrellaLabel(umbrella) + " to connect it."}
	}
	requestedFor := ""
	if mode == models.SkillSetupModePerPerson {
		requestedFor = run.ActingUser
	}
	skillName := boundSkillName(bound)
	if len(skillName) > 256 {
		skillName = models.TruncateRunes(skillName, 64)
	}
	now := time.Now().UTC()
	r := &models.SkillSetupRequest{
		ID: id, Scope: run.Scope, Owner: umbrella, DeploymentID: umbrella.ID, AgentID: run.AssignedAgentID,
		ConversationID: conversationID, TriggerMessageID: messageID, RunID: run.ID, ActionCallID: call.ID,
		Kind: models.SkillSetupKindConnectAccount, SkillID: call.SkillID, SkillVersion: call.SkillVersion, SkillName: skillName,
		BindingID: call.BindingID, BindingRevision: call.BindingRevision,
		Slots: slots, Mode: mode, RequestedFor: requestedFor,
		Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if bound != nil && bound.Definition != nil {
		r.SourceIdentity = skill.DefinitionSourceIdentity(bound.Definition)
	}
	provider := ConnectAccountProvider(r)
	if mode == models.SkillSetupModePerPerson {
		r.Reason = capitalizeFirst(umbrellaLabel(umbrella)) + " uses each person's own " + provider + " account."
	} else {
		r.Reason = "The " + provider + " account shared with " + umbrellaLabel(umbrella) + " is no longer available."
	}
	if err := store.SaveSkillSetupRequest(ctx, r, 0); err != nil {
		if errors.Is(err, models.ErrSkillSetupConflict) {
			if replay, readErr := store.GetSkillSetupRequest(ctx, run.Scope, id); readErr == nil && replay != nil {
				return replay, nil
			}
		}
		return nil, err
	}
	return r, nil
}

func capitalizeFirst(value string) string {
	first, size := utf8.DecodeRuneInString(value)
	if size == 0 {
		return value
	}
	return string(unicode.ToUpper(first)) + value[size:]
}

// isParkedConnectAccountCall reports whether call failed before dispatch only
// because its card asks someone to connect an account.
func isParkedConnectAccountCall(call *models.ActionCall) bool {
	if call == nil || call.Status != models.ActionCallStatusFailed || call.FailurePhase != models.ActionFailureBeforeDispatch || call.SkillID == models.SkillManagementSkillID {
		return false
	}
	value, _ := call.Output["setupRequest"].(map[string]interface{})
	kind, _ := value["kind"].(string)
	id, _ := value["id"].(string)
	return kind == models.SkillSetupKindConnectAccount && id == connectAccountRequestPrefix+call.ID
}

// pendingConnectAccountRequestFromAction returns the pending card a parked
// call waits for. A card settled before this commit does not park the run.
func pendingConnectAccountRequestFromAction(ctx context.Context, store models.SkillSetupRequestStore, call *models.ActionCall) *models.SkillSetupRequest {
	if store == nil || !isParkedConnectAccountCall(call) {
		return nil
	}
	current, err := store.GetSkillSetupRequest(ctx, call.Scope, connectAccountRequestPrefix+call.ID)
	if err != nil || current == nil || current.Kind != models.SkillSetupKindConnectAccount || current.RunID != call.RunID || current.Status != "pending" {
		return nil
	}
	return current
}

// IsExactConnectAccountWait reports whether the run waits for this card.
func IsExactConnectAccountWait(run *models.AgentRun, requestID string) bool {
	return run != nil && run.Status == models.AgentRunStatusWaitingForEvent && run.WakeCondition != nil &&
		run.WakeCondition.Type == models.ConnectAccountWakeType && run.WakeCondition.Reference == requestID
}

// ConnectAccountResolutionInstruction is the durable guidance given to a run
// woken by its connect_account card.
func ConnectAccountResolutionInstruction(r *models.SkillSetupRequest) string {
	provider := ConnectAccountProvider(r)
	switch {
	case r.Status == "dismissed":
		return "The person asked chose not to connect " + provider + ". Continue without it or explain what is blocked."
	case r.Mode == models.SkillSetupModeShared:
		return "The shared " + provider + " account is ready. Retry the step that needed it."
	default:
		return "The person you act for connected " + provider + ". Retry the step that needed it."
	}
}

// connectAccountWaitSummary describes a run parked on its card.
func connectAccountWaitSummary(r *models.SkillSetupRequest) string {
	who := "the person asking"
	if r.Mode == models.SkillSetupModeShared {
		who = "someone who manages " + umbrellaLabel(r.Owner)
	}
	return "Waiting for " + who + " to connect " + ConnectAccountProvider(r)
}
