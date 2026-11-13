package runloop

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// GovernedToolOutcome is what the governed executor reports for one governed
// tool call of a step. The kernel tool executor applies it to the tool entry
// with one TransitionToolEntry (running -> State, Result, Wait, ActionCallID)
// and, at step end, asks PlanPark whether the Run parks.
type GovernedToolOutcome struct {
	ToolCallID string
	// State is completed, error, interrupted, awaiting_approval,
	// awaiting_event or running (a foreign live call lease; see RetryAt).
	State models.ToolEntryState
	// Result is set for terminal states only.
	Result *models.ToolResult
	// Wait is set for awaiting states: ApprovalID for awaiting_approval,
	// EventWaitID (the card request id) or Reason/RetryAt for awaiting_event.
	Wait *models.ToolEntryWait
	// ActionCallID is "" only for refusals before any row exists.
	ActionCallID string
	// Park is set for awaiting states only. It is never committed: the loop
	// parks the Run at step end with its own revision-fenced transition.
	Park *GovernedPark
	// RetryAt is set with State running: the call lease is held elsewhere;
	// the Run is requeued then.
	RetryAt *time.Time
	// Output is the sanitized succeeded output.
	Output map[string]interface{}
	// BudgetExhausted reports that the call was denied because the Run's
	// action budget is used up; the loop takes its budget exit.
	BudgetExhausted bool
	// Run is the Run as the executor's own writes left it (budget
	// reservation, settled budget, human intervention request, held
	// supersede), adopted only while each write was the next revision of the
	// one before; nil when the executor wrote nothing it could adopt. It is
	// never a fresh read, so a decision made while the Run was held still
	// fails the loop's step-end park CAS. The caller replaces its Run with it
	// before its next revision-fenced write.
	Run *models.AgentRun
}

// GovernedPark is the park a governed tool entry asks for. The kernel tool
// executor turns it into the loop's LoopPark{Status, Wake, Summary} with
// Committed false.
type GovernedPark struct {
	Status  models.AgentRunStatus
	Wake    *models.WakeCondition
	Summary string
}

// GovernedEvidenceRef is the model- and preview-facing reference of an
// action call; enrichApprovalPreview and evidenceRefs resolve this format.
func GovernedEvidenceRef(actionCallID string) string { return "action-call:" + actionCallID }

// Model-facing result texts of governed calls.
const (
	governedDuplicateNote          = "This exact operation was already performed earlier, so it was not sent again."
	governedInterruptedError       = "The previous operation may already have taken effect. It was not sent again. Check before retrying."
	governedConnectAccountError    = "An account sign-in was needed."
	governedExpiredReason          = "Nobody answered the approval request in time; the action was not carried out."
	governedRevertedError          = "The action was reverted."
	governedMaintenanceError       = "This tool is temporarily unavailable while it is being updated. Try again later or use another way."
	governedInFlightError          = "The same operation is already in progress, so it was not sent again."
	governedBudgetExhaustedReason  = "This run has used its action budget."
	governedMaintenanceRefusalCode = "skill_maintenance"
)


// renderGovernedResult renders a call that reached a terminal status as its
// tool entry state and result (section 03 §7.1). approval is the call's
// approval checkpoint when it has one; the approval outcome is read from its
// Status, never parsed from call.Error. settled is the settled card or human
// intervention text (section 03 §10.4), "" when none. A call that is not
// terminal (ready, waiting_for_approval, running) renders as ("", nil).
func renderGovernedResult(call *models.ActionCall, approval *models.ApprovalCheckpoint, settled string) (models.ToolEntryState, *models.ToolResult) {
	if call == nil {
		return "", nil
	}
	ref := GovernedEvidenceRef(call.ID)
	switch call.Status {
	case models.ActionCallStatusSucceeded:
		if call.DuplicateOfActionCallID != "" {
			return models.ToolEntryError, governedJSONResult(models.ToolResultDuplicateSuppressed, map[string]interface{}{
				"evidenceRef": ref, "status": string(models.ToolResultDuplicateSuppressed),
				"priorEvidenceRef": GovernedEvidenceRef(call.DuplicateOfActionCallID), "note": governedDuplicateNote,
			})
		}
		output, image := splitGovernedModelMedia(call)
		value := map[string]interface{}{"evidenceRef": ref, "output": output}
		if settled != "" {
			value["outcome"] = settled
		}
		result := governedJSONResult(models.ToolResultSucceeded, value)
		if image != nil {
			result.Content = append(result.Content, *image)
		}
		return models.ToolEntryCompleted, result
	case models.ActionCallStatusFailed:
		if call.RecoveredRunning && !governedReadOnly(call.SideEffect) {
			return models.ToolEntryInterrupted, governedJSONResult(models.ToolResultInterrupted, map[string]interface{}{
				"evidenceRef": ref, "status": string(models.ToolResultInterrupted), "error": governedInterruptedError,
			})
		}
		if skillbinding.IsParkedConnectAccountCall(call) {
			value := map[string]interface{}{"evidenceRef": ref, "status": string(models.ToolResultFailed), "error": governedConnectAccountError}
			if settled != "" {
				value["outcome"] = settled
			}
			return models.ToolEntryError, governedJSONResult(models.ToolResultFailed, value)
		}
		value := map[string]interface{}{"evidenceRef": ref, "status": string(models.ToolResultFailed)}
		putNonEmpty(value, "error", call.Error)
		putNonEmpty(value, "errorCode", call.ErrorCode)
		putNonEmpty(value, "failurePhase", string(call.FailurePhase))
		if len(call.ErrorDetails) > 0 {
			details := make(map[string]interface{}, len(call.ErrorDetails))
			for key, detail := range call.ErrorDetails {
				details[key] = detail
			}
			value["errorDetails"] = details
		}
		return models.ToolEntryError, governedJSONResult(models.ToolResultFailed, value)
	case models.ActionCallStatusDenied:
		return renderGovernedDenial(call, approval)
	case models.ActionCallStatusCanceled:
		return models.ToolEntryError, governedJSONResult(models.ToolResultCanceled, map[string]interface{}{
			"evidenceRef": ref, "status": string(models.ToolResultCanceled), "reason": call.Error,
		})
	case models.ActionCallStatusRefused:
		return models.ToolEntryError, governedJSONResult(models.ToolResultRefused, map[string]interface{}{
			"evidenceRef": ref, "status": string(models.ToolResultRefused), "code": call.ErrorCode, "error": call.Error,
		})
	case models.ActionCallStatusCompensating, models.ActionCallStatusCompensated:
		return models.ToolEntryError, governedJSONResult(models.ToolResultFailed, map[string]interface{}{
			"evidenceRef": ref, "status": string(models.ToolResultFailed), "error": governedRevertedError,
		})
	}
	return "", nil
}

// renderGovernedDenial renders a denied call by its approval outcome; a
// denial without a decided approval is a policy or budget denial.
func renderGovernedDenial(call *models.ActionCall, approval *models.ApprovalCheckpoint) (models.ToolEntryState, *models.ToolResult) {
	ref := GovernedEvidenceRef(call.ID)
	if approval != nil && approval.ID == call.ApprovalID {
		switch approval.Status {
		case models.ApprovalStatusRejected:
			return models.ToolEntryError, governedJSONResult(models.ToolResultDenied, map[string]interface{}{
				"evidenceRef": ref, "status": "rejected", "reason": approval.DecisionReason,
			})
		case models.ApprovalStatusChangesRequested:
			return models.ToolEntryError, governedJSONResult(models.ToolResultChangesRequested, map[string]interface{}{
				"evidenceRef": ref, "status": string(models.ToolResultChangesRequested), "guidance": approval.DecisionReason,
			})
		case models.ApprovalStatusSuperseded:
			return models.ToolEntryError, governedJSONResult(models.ToolResultSuperseded, map[string]interface{}{
				"evidenceRef": ref, "status": string(models.ToolResultSuperseded), "reason": action.ApprovalSupersededReason,
			})
		case models.ApprovalStatusExpired:
			return models.ToolEntryError, governedJSONResult(models.ToolResultExpired, map[string]interface{}{
				"evidenceRef": ref, "status": string(models.ToolResultExpired), "reason": governedExpiredReason,
			})
		case models.ApprovalStatusCanceled:
			return models.ToolEntryError, governedJSONResult(models.ToolResultCanceled, map[string]interface{}{
				"evidenceRef": ref, "status": string(models.ToolResultCanceled), "reason": call.Error,
			})
		}
	}
	if call.Error == action.BudgetExhaustedReason {
		return renderGovernedBudgetExhausted(call)
	}
	return models.ToolEntryError, governedJSONResult(models.ToolResultDenied, map[string]interface{}{
		"evidenceRef": ref, "status": string(models.ToolResultDenied), "reason": call.Error,
	})
}

// renderGovernedBudgetExhausted renders a call denied because the Run's
// action budget is used up.
func renderGovernedBudgetExhausted(call *models.ActionCall) (models.ToolEntryState, *models.ToolResult) {
	value := map[string]interface{}{"status": string(models.ToolResultDenied), "reason": governedBudgetExhaustedReason}
	if call != nil {
		value["evidenceRef"] = GovernedEvidenceRef(call.ID)
	}
	return models.ToolEntryError, governedJSONResult(models.ToolResultDenied, value)
}

// renderGovernedRejection renders a proposal the coordinator rejected before
// any row existed.
func renderGovernedRejection(rejection *action.ProposalRejection) (models.ToolEntryState, *models.ToolResult) {
	return models.ToolEntryError, governedJSONResult(models.ToolResultRefused, map[string]interface{}{
		"status": string(models.ToolResultRefused), "code": rejection.Code, "error": rejection.Message,
	})
}

// renderGovernedMaintenanceRefusal renders a proposal refused because its
// Skill runtime is under maintenance.
func renderGovernedMaintenanceRefusal() (models.ToolEntryState, *models.ToolResult) {
	return models.ToolEntryError, governedJSONResult(models.ToolResultRefused, map[string]interface{}{
		"status": string(models.ToolResultRefused), "code": governedMaintenanceRefusalCode, "error": governedMaintenanceError,
	})
}

// renderGovernedInFlight renders a proposal whose external operation is
// already in flight in priorActionCallID.
func renderGovernedInFlight(priorActionCallID string) (models.ToolEntryState, *models.ToolResult) {
	return models.ToolEntryError, governedJSONResult(models.ToolResultDuplicateSuppressed, map[string]interface{}{
		"status": string(models.ToolResultDuplicateSuppressed), "priorEvidenceRef": GovernedEvidenceRef(priorActionCallID),
		"error": governedInFlightError,
	})
}

// splitGovernedModelMedia returns the call's output without modelMedia and,
// when modelMedia is a valid image, one image part that references the call.
// The renderer resolves the bytes from the call (SPEC §3.6); persisted parts
// never carry them. Invalid media is dropped without a part.
func splitGovernedModelMedia(call *models.ActionCall) (map[string]interface{}, *models.ContentPart) {
	output := maps.Clone(call.Output)
	raw, present := output["modelMedia"]
	if !present {
		return output, nil
	}
	delete(output, "modelMedia")
	media, ok := raw.(map[string]interface{})
	if !ok {
		return output, nil
	}
	mediaType, _ := media["mediaType"].(string)
	detail, _ := media["detail"].(string)
	data, _ := media["contentBase64"].(string)
	mediaType, detail = strings.TrimSpace(mediaType), strings.TrimSpace(detail)
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return output, nil
	}
	switch detail {
	case "", "auto", "low", "high":
	default:
		return output, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data))
	if err != nil || len(decoded) == 0 || len(decoded) > models.MaxModelImageSourceBytes {
		return output, nil
	}
	digest := sha256.Sum256(decoded)
	return output, &models.ContentPart{
		Type: models.ContentPartImage, MediaType: mediaType, Detail: detail,
		SHA256: hex.EncodeToString(digest[:]), ActionCallID: call.ID,
	}
}

// governedReadOnly reports whether a side effect cannot have changed
// anything outside the call.
func governedReadOnly(effect skill.SideEffect) bool {
	return effect == skill.SideEffectNone || effect == skill.SideEffectRead
}

// governedJSONResult is a result with one json part holding value.
func governedJSONResult(status models.ToolResultStatus, value map[string]interface{}) *models.ToolResult {
	encoded, err := json.Marshal(value)
	if err != nil {
		// Output values come from decoded JSON; an unencodable value is a
		// kernel bug, reported to the model rather than lost.
		encoded, _ = json.Marshal(map[string]interface{}{"status": string(status), "error": "the action result could not be encoded"})
	}
	return &models.ToolResult{
		Status: status, IsError: status != models.ToolResultSucceeded,
		Content: []models.ContentPart{{Type: models.ContentPartJSON, Value: encoded}},
	}
}

func putNonEmpty(target map[string]interface{}, key, value string) {
	if value != "" {
		target[key] = value
	}
}
