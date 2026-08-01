package runtime

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	inferschema "github.com/invopop/jsonschema"
)

const HostedTurnFormSchemaVersion = "openseal.hosted-turn-form/v1"

// HostedTurnForm is the model-facing representation of one hosted Turn. It
// keeps action arguments beside the selected capability so a model fills one
// schema-backed form. CompileHostedTurnForm owns the kernel-specific pointer
// and checkpoint representation; models never construct that wiring.
type HostedTurnForm struct {
	SchemaVersion          string                  `json:"schemaVersion"`
	SkillSelections        []HostedSkillSelection  `json:"skillSelections"`
	Decisions              []TurnDecision          `json:"decisions"`
	ProposedAction         *HostedTurnActionForm   `json:"proposedAction,omitempty"`
	ProposedFork           *TurnForkProposal       `json:"proposedFork,omitempty"`
	ProposedDelegation     *TurnDelegationProposal `json:"proposedDelegation,omitempty"`
	ProposedRunbook        *TurnRunbookProposal    `json:"proposedRunbook,omitempty"`
	OutputSummary          string                  `json:"outputSummary"`
	ContinuationCheckpoint map[string]interface{}  `json:"continuationCheckpoint"`
	NextRunStatus          AgentRunStatus          `json:"nextRunStatus"`
	WakeCondition          *WakeCondition          `json:"wakeCondition,omitempty"`
	RunOutput              map[string]interface{}  `json:"runOutput"`
	RunError               string                  `json:"runError"`
	CompletionEvidenceRefs []string                `json:"completionEvidenceRefs"`
	EvidenceClaims         []EvidenceClaim         `json:"evidenceClaims"`
}

type HostedTurnActionForm struct {
	Capability        string                     `json:"capability"`
	Summary           string                     `json:"summary"`
	IdempotencyKey    string                     `json:"idempotencyKey"`
	Arguments         map[string]interface{}     `json:"arguments"`
	EvidenceRefs      []string                   `json:"evidenceRefs,omitempty"`
	ExternalOperation *ExternalOperationIdentity `json:"externalOperation,omitempty"`
	ReviewContext     *ApprovalReviewContext     `json:"reviewContext,omitempty"`
}

// HostedTurnFormAuthority describes which non-Skill proposal families are
// actually available during one Turn. Supplying it lets a host remove
// impossible choices from the model-facing form while the kernel remains the
// final authority during compilation and materialization.
type HostedTurnFormAuthority struct {
	CanDelegate           bool
	CanInvokeRunbook      bool
	SkillPromptReferences []string
}

// CompileHostedTurnForm validates the selected action against the exact
// authorized contract and compiles its inline form into the canonical kernel
// response. Kernel control metadata already present in the proposal envelope
// may be projected into an action contract that declares the same field; domain
// arguments remain model-authored and authority is never widened.
func CompileHostedTurnForm(form HostedTurnForm, actions []capability.ModelAction) (*HostedTurnResponse, error) {
	if form.SchemaVersion != HostedTurnFormSchemaVersion {
		return nil, fmt.Errorf("hosted turn form schemaVersion must be %q", HostedTurnFormSchemaVersion)
	}
	decisions := append([]TurnDecision(nil), form.Decisions...)
	// Models can request governed actions, but they cannot mint approval IDs or
	// place a Run into approval lifecycle state. Action materialization and the
	// ApprovalCoordinator derive the exact wait condition when policy requires
	// one. Until then, approval language is only continuation intent.
	if form.NextRunStatus == AgentRunStatusWaitingForApproval {
		form.NextRunStatus = AgentRunStatusRunning
		form.WakeCondition = nil
		decisions = append(decisions, TurnDecision{
			Summary: "Continued the Run because approval state is derived from a governed action, not model output.",
		})
	}
	if err := ValidateHostedTurnLifecycle(form.NextRunStatus, form.WakeCondition); err != nil {
		return nil, err
	}
	checkpoint, err := cloneHostedTurnObject(form.ContinuationCheckpoint)
	if err != nil {
		return nil, err
	}
	response := &HostedTurnResponse{
		SkillSelections: form.SkillSelections, Decisions: decisions,
		ProposedFork: form.ProposedFork, ProposedDelegation: form.ProposedDelegation, ProposedRunbook: form.ProposedRunbook,
		OutputSummary: form.OutputSummary, ContinuationCheckpoint: checkpoint, NextRunStatus: form.NextRunStatus,
		WakeCondition: form.WakeCondition, RunOutput: form.RunOutput, RunError: form.RunError,
		CompletionEvidenceRefs: form.CompletionEvidenceRefs, EvidenceClaims: form.EvidenceClaims,
	}
	if form.ProposedAction == nil {
		return response, nil
	}
	selected, ok := exactHostedTurnAction(actions, form.ProposedAction.Capability)
	if !ok {
		return nil, fmt.Errorf("proposed action %q is not in the authorized catalog", form.ProposedAction.Capability)
	}
	if strings.TrimSpace(form.ProposedAction.Summary) == "" || strings.TrimSpace(form.ProposedAction.IdempotencyKey) == "" {
		return nil, errors.New("proposed action summary and idempotencyKey are required")
	}
	if form.ProposedAction.Arguments == nil {
		return nil, errors.New("proposed action arguments are required")
	}
	arguments := cloneHostedTurnObjectValue(form.ProposedAction.Arguments)
	projectHostedTurnActionControlArguments(arguments, selected.InputSchema, form.ProposedAction)
	if len(selected.InputSchema) > 0 {
		if err := runbook.ValidateInterfaceInput(selected.InputSchema, arguments); err != nil {
			return nil, fmt.Errorf("proposed action %q input does not match its authorized schema: %w", selected.Name, err)
		}
	}
	if checkpoint == nil {
		checkpoint = map[string]interface{}{}
		response.ContinuationCheckpoint = checkpoint
	}
	actionInputs, _ := checkpoint["actionInputs"].(map[string]interface{})
	if actionInputs == nil {
		actionInputs = map[string]interface{}{}
		checkpoint["actionInputs"] = actionInputs
	}
	actionInputs["proposed"] = arguments
	response.ProposedAction = &TurnAction{
		Type: "skill_action", Capability: selected.Name, Summary: strings.TrimSpace(form.ProposedAction.Summary),
		IdempotencyKey: strings.TrimSpace(form.ProposedAction.IdempotencyKey), InputRef: "/actionInputs/proposed",
		EvidenceRefs: append([]string(nil), form.ProposedAction.EvidenceRefs...), ExternalOperation: form.ProposedAction.ExternalOperation,
		ReviewContext: cloneApprovalReviewContext(form.ProposedAction.ReviewContext),
	}
	if selected.SideEffect == capability.SideEffectExternal && response.ProposedAction.ReviewContext == nil {
		return nil, errors.New("external proposed action reviewContext is required")
	}
	if err := validateApprovalReviewContext(response.ProposedAction.ReviewContext); err != nil {
		return nil, fmt.Errorf("proposed action reviewContext is invalid: %w", err)
	}
	return response, nil
}

// projectHostedTurnActionControlArguments removes a duplicated model burden:
// idempotency is already a required, kernel-governed field on the proposal
// envelope. When an authorized Skill contract also exposes that standard field,
// compile it from the envelope instead of asking the model to repeat it exactly.
// Arbitrary Skill inputs are deliberately untouched.
func projectHostedTurnActionControlArguments(arguments, inputSchema map[string]interface{}, proposal *HostedTurnActionForm) {
	if arguments == nil || proposal == nil {
		return
	}
	properties, _ := inputSchema["properties"].(map[string]interface{})
	if _, declared := properties["idempotencyKey"]; !declared {
		return
	}
	if value, exists := arguments["idempotencyKey"]; exists {
		if text, ok := value.(string); !ok || strings.TrimSpace(text) != "" {
			return
		}
	}
	arguments["idempotencyKey"] = strings.TrimSpace(proposal.IdempotencyKey)
}

func cloneHostedTurnObjectValue(value map[string]interface{}) map[string]interface{} {
	if value == nil {
		return nil
	}
	cloned, _ := cloneHostedTurnValue(value).(map[string]interface{})
	return cloned
}

// HostedTurnFormFromResponse is the inverse projection used by hosts, tests,
// and inspectors. It resolves the canonical action input pointer back into the
// same model-facing form without changing the response.
func HostedTurnFormFromResponse(response HostedTurnResponse) (HostedTurnForm, error) {
	form := HostedTurnForm{
		SchemaVersion: HostedTurnFormSchemaVersion, SkillSelections: response.SkillSelections, Decisions: response.Decisions,
		ProposedFork: response.ProposedFork, ProposedDelegation: response.ProposedDelegation, ProposedRunbook: response.ProposedRunbook,
		OutputSummary: response.OutputSummary, ContinuationCheckpoint: response.ContinuationCheckpoint,
		NextRunStatus: response.NextRunStatus, WakeCondition: response.WakeCondition, RunOutput: response.RunOutput,
		RunError: response.RunError, CompletionEvidenceRefs: response.CompletionEvidenceRefs, EvidenceClaims: response.EvidenceClaims,
	}
	if response.ProposedAction == nil {
		return form, nil
	}
	arguments, err := hostedTurnFormPointerObject(response.ContinuationCheckpoint, response.ProposedAction.InputRef)
	if err != nil {
		return HostedTurnForm{}, err
	}
	form.ProposedAction = &HostedTurnActionForm{
		Capability: response.ProposedAction.Capability, Summary: response.ProposedAction.Summary,
		IdempotencyKey: response.ProposedAction.IdempotencyKey, Arguments: arguments,
		EvidenceRefs: append([]string(nil), response.ProposedAction.EvidenceRefs...), ExternalOperation: response.ProposedAction.ExternalOperation,
		ReviewContext: cloneApprovalReviewContext(response.ProposedAction.ReviewContext),
	}
	return form, nil
}

// HostedTurnFormJSONSchema generates the provider-facing form schema from the
// Go contract, replaces the action union with the exact authorized input
// schemas, and can omit proposal families unavailable to this Turn.
func HostedTurnFormJSONSchema(actions []capability.ModelAction, authority ...HostedTurnFormAuthority) (map[string]interface{}, error) {
	inferred := (&inferschema.Reflector{
		Anonymous: true, ExpandedStruct: true,
		Namer: func(value reflect.Type) string {
			if value.Name() == "" {
				return fmt.Sprintf("anonymous_%x", sha256.Sum256([]byte(value.String())))
			}
			return path.Base(value.PkgPath()) + "_" + value.Name()
		},
	}).Reflect(HostedTurnForm{})
	payload, err := json.Marshal(inferred)
	if err != nil {
		return nil, err
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(payload, &schema); err != nil {
		return nil, err
	}
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return nil, errors.New("hosted turn form schema has no object properties")
	}
	properties["schemaVersion"] = map[string]interface{}{"type": "string", "const": HostedTurnFormSchemaVersion}
	properties["nextRunStatus"] = map[string]interface{}{
		"type": "string",
		"enum": []string{
			string(AgentRunStatusRunning),
			string(AgentRunStatusPaused),
			string(AgentRunStatusSleeping),
			string(AgentRunStatusWaitingForDependency),
			string(AgentRunStatusWaitingForAgent),
			string(AgentRunStatusWaitingForApproval),
			string(AgentRunStatusWaitingForEvent),
			string(AgentRunStatusCompleted),
			string(AgentRunStatusFailed),
		},
	}
	if len(authority) > 0 {
		promptBranches := make([]interface{}, 0, len(authority[0].SkillPromptReferences))
		seen := make(map[string]struct{}, len(authority[0].SkillPromptReferences))
		for _, raw := range authority[0].SkillPromptReferences {
			reference := strings.TrimSpace(raw)
			if reference == "" {
				return nil, errors.New("hosted turn form Skill prompt reference is required")
			}
			if _, duplicate := seen[reference]; duplicate {
				return nil, fmt.Errorf("hosted turn form Skill prompt reference %q is duplicated", reference)
			}
			seen[reference] = struct{}{}
			promptBranches = append(promptBranches, map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{
					"skillRef":    map[string]interface{}{"type": "string", "const": reference},
					"disposition": map[string]interface{}{"type": "string", "enum": []string{string(HostedSkillApplied), string(HostedSkillNotApplied)}},
					"summary":     map[string]interface{}{"type": "string", "minLength": 1},
				},
				"required": []string{"skillRef", "disposition", "summary"},
			})
		}
		skillSelections := map[string]interface{}{
			"type": "array", "minItems": len(promptBranches), "maxItems": len(promptBranches),
		}
		if len(promptBranches) == 0 {
			skillSelections["items"] = false
		} else {
			skillSelections["items"] = map[string]interface{}{"oneOf": promptBranches}
		}
		properties["skillSelections"] = skillSelections
	}
	branches := make([]interface{}, 0, len(actions))
	for _, action := range actions {
		branchProperties := map[string]interface{}{
			"capability":     map[string]interface{}{"type": "string", "const": action.Name},
			"summary":        map[string]interface{}{"type": "string", "minLength": 1},
			"idempotencyKey": map[string]interface{}{"type": "string", "minLength": 1},
			"arguments":      cloneHostedTurnValue(action.InputSchema),
			"evidenceRefs":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			"externalOperation": map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{
					"resource": map[string]interface{}{"type": "string"}, "operation": map[string]interface{}{"type": "string"},
				}, "required": []string{"resource", "operation"},
			},
			"reviewContext": approvalReviewContextJSONSchema(),
		}
		required := []string{"capability", "summary", "idempotencyKey", "arguments"}
		if action.SideEffect == capability.SideEffectExternal {
			required = append(required, "reviewContext")
		}
		branches = append(branches, map[string]interface{}{
			"type": "object", "additionalProperties": false, "properties": branchProperties,
			"required": required,
		})
	}
	if len(branches) == 0 {
		delete(properties, "proposedAction")
	} else {
		properties["proposedAction"] = map[string]interface{}{"oneOf": branches}
	}
	if len(authority) > 0 {
		if !authority[0].CanDelegate {
			delete(properties, "proposedDelegation")
			delete(properties, "proposedFork")
		}
		if !authority[0].CanInvokeRunbook {
			delete(properties, "proposedRunbook")
		}
	}
	return schema, nil
}

// ValidateHostedTurnLifecycle keeps model-authored lifecycle intent inside the
// canonical durable Run state machine. A generic "waiting" state is
// deliberately unsupported: the model must select the exact wait reason and
// supply the wake source the kernel will persist.
func ValidateHostedTurnLifecycle(status AgentRunStatus, wake *WakeCondition) error {
	switch status {
	case AgentRunStatusRunning, AgentRunStatusPaused, AgentRunStatusCompleted, AgentRunStatusFailed:
		if wake != nil {
			return fmt.Errorf("hosted turn status %s cannot include a wakeCondition", status)
		}
		return nil
	case AgentRunStatusSleeping, AgentRunStatusWaitingForDependency, AgentRunStatusWaitingForAgent,
		AgentRunStatusWaitingForApproval, AgentRunStatusWaitingForEvent:
		if wake == nil || strings.TrimSpace(wake.Type) == "" {
			return fmt.Errorf("hosted turn status %s requires a concrete wakeCondition", status)
		}
		return nil
	default:
		return fmt.Errorf("hosted turn nextRunStatus %q is not a supported model outcome", status)
	}
}

func exactHostedTurnAction(actions []capability.ModelAction, name string) (capability.ModelAction, bool) {
	for _, action := range actions {
		if action.Name == strings.TrimSpace(name) {
			return action, true
		}
	}
	return capability.ModelAction{}, false
}

func cloneHostedTurnObject(value map[string]interface{}) (map[string]interface{}, error) {
	if value == nil {
		return nil, nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var cloned map[string]interface{}
	if err := json.Unmarshal(payload, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

func cloneHostedTurnValue(value interface{}) interface{} {
	payload, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned interface{}
	if json.Unmarshal(payload, &cloned) != nil {
		return value
	}
	return cloned
}

func hostedTurnFormPointerObject(checkpoint map[string]interface{}, reference string) (map[string]interface{}, error) {
	reference = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(reference), "#"))
	if !strings.HasPrefix(reference, "/") {
		return nil, errors.New("action inputRef must be a JSON Pointer into continuationCheckpoint")
	}
	var current interface{} = checkpoint
	for _, encoded := range strings.Split(strings.TrimPrefix(reference, "/"), "/") {
		object, ok := current.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("action inputRef %q traverses a non-object", reference)
		}
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		current, ok = object[segment]
		if !ok {
			return nil, fmt.Errorf("action inputRef %q does not exist", reference)
		}
	}
	object, ok := current.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("action inputRef %q must resolve to an object", reference)
	}
	return object, nil
}
