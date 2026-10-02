package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// workflowSubjectFromAction checks durable provenance before reading exactly
// one adapter-declared output field. Arguments and model-authored checkpoint
// results cannot establish the provider's external conversation identity.
func workflowSubjectFromAction(run *AgentRun, source *WorkflowEventSource, call *ActionCall) (WorkflowKnownSubject, error) {
	invalid := errors.New("workflow subject evidence must be a succeeded action from this originating conversation Run and exact current connector binding")
	if run == nil || run.Kind != RunKindConversation || source == nil || source.AdapterKind != "conversation" ||
		!source.EvidenceRequired || source.binding == nil || source.definition == nil || call == nil || !validWorkflowEvidenceActionID(call.ID) ||
		call.Status != ActionCallStatusSucceeded || call.Scope != run.Scope || call.RunID != run.ID ||
		call.DeploymentID != run.AssignedAgentID || call.BindingID != source.BindingID || call.BindingRevision != source.BindingRevision ||
		call.SkillID != source.SkillID || call.SkillVersion != source.SkillVersion {
		return WorkflowKnownSubject{}, invalid
	}
	binding := source.binding
	if binding.Disabled || binding.ID != source.BindingID || binding.Revision != source.BindingRevision ||
		binding.Scope.Kind != run.Scope.Kind || binding.Scope.ID != run.Scope.ID || binding.DeploymentID != run.AssignedAgentID ||
		binding.SkillID != source.SkillID || binding.SkillVersion != source.SkillVersion ||
		source.definition.ID != source.SkillID || source.definition.Version != source.SkillVersion ||
		!slices.Contains(binding.AllowedActions, call.Action) {
		return WorkflowKnownSubject{}, invalid
	}
	if _, declared := source.definition.Actions[call.Action]; !declared {
		return WorkflowKnownSubject{}, invalid
	}
	var declaration *capability.ConversationSubjectEvidence
	for i := range source.SubjectEvidence {
		if source.SubjectEvidence[i].Action == call.Action {
			declaration = &source.SubjectEvidence[i]
			break
		}
	}
	if declaration == nil {
		return WorkflowKnownSubject{}, errors.New("workflow subject evidence action is not declared by the selected conversation adapter")
	}
	subject, ok := projectWorkflowSubjectPath(call.Output, declaration.SubjectPath)
	if !ok || !validExternalConversationReference(subject, 512) {
		return WorkflowKnownSubject{}, errors.New("workflow subject evidence has no bounded canonical external conversation ID at its declared output path")
	}
	return WorkflowKnownSubject{ExternalConversationID: subject, ActionCallID: call.ID}, nil
}

// Subject paths traverse objects only. They never interpret array indexes,
// wildcards, embedded JSON, or fallback fields elsewhere in the output.
func projectWorkflowSubjectPath(output map[string]interface{}, path string) (string, bool) {
	if path == "" || len(path) > 256 || path != strings.TrimSpace(path) {
		return "", false
	}
	var current interface{} = output
	for _, key := range strings.Split(path, ".") {
		if key == "" || len(key) > 128 || key != strings.TrimSpace(key) || strings.ContainsAny(key, "*[]\r\n\t") {
			return "", false
		}
		object, ok := current.(map[string]interface{})
		if !ok {
			return "", false
		}
		current, ok = object[key]
		if !ok {
			return "", false
		}
	}
	subject, ok := current.(string)
	return subject, ok
}

func validWorkflowEvidenceActionID(value string) bool {
	return value == strings.TrimSpace(value) && validOpaqueIdentifier(value, 256)
}

func requireWorkflowSubjectEvidence(ctx context.Context, store runbookActionStore, run *AgentRun, source *WorkflowEventSource, actionID, subject string) error {
	if !validWorkflowEvidenceActionID(actionID) {
		return errors.New("selected conversation adapter requires subjectEvidenceActionCallId from a succeeded action in this originating chat Run")
	}
	// This is a scoped point lookup. No action history scan or provider call is
	// needed to establish an already persisted provider receipt.
	call, err := store.GetActionCall(ctx, run.Scope, actionID)
	if err != nil {
		return fmt.Errorf("workflow subject evidence action is unavailable: %w", err)
	}
	known, err := workflowSubjectFromAction(run, source, call)
	if err != nil {
		return err
	}
	if known.ActionCallID != actionID {
		return errors.New("workflow subject evidence does not match the requested persisted action call")
	}
	if subject != known.ExternalConversationID {
		return errors.New("workflow subject must exactly equal the external conversation ID proven by subjectEvidenceActionCallId")
	}
	return nil
}

func projectWorkflowKnownSubjects(ctx context.Context, store runbookActionStore, run *AgentRun, sources []*WorkflowEventSource, explicitActionID string) error {
	evidenceSources := make([]*WorkflowEventSource, 0, len(sources))
	for _, source := range sources {
		if source != nil && source.AdapterKind == "conversation" && source.EvidenceRequired {
			evidenceSources = append(evidenceSources, source)
		}
	}
	if len(evidenceSources) == 0 {
		return nil
	}
	if explicitActionID != "" && !validWorkflowEvidenceActionID(explicitActionID) {
		return errors.New("subjectEvidenceActionCallId must be an exact bounded action call ID")
	}
	// At most two candidate identities: one explicit receipt and the latest
	// checkpoint receipt. Checkpoint contents only choose a lookup; the durable
	// ActionCall supplies every identity, success, authorization, and output fact.
	candidates := make([]string, 0, 2)
	if explicitActionID != "" {
		candidates = append(candidates, explicitActionID)
	}
	if run != nil {
		last, _ := run.Checkpoint["lastAction"].(map[string]interface{})
		for _, source := range evidenceSources {
			latestID, _, matched := workflowSubjectEvidenceHint(last, source)
			if matched && !slices.Contains(candidates, latestID) {
				candidates = append(candidates, latestID)
				break
			}
		}
	}
	for _, actionID := range candidates {
		call, err := store.GetActionCall(ctx, run.Scope, actionID)
		if errors.Is(err, ErrActionNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if call == nil || call.ID != actionID {
			continue
		}
		for _, source := range evidenceSources {
			known, err := workflowSubjectFromAction(run, source, call)
			if err == nil {
				source.KnownSubjects = append(source.KnownSubjects, known)
			}
		}
	}
	return nil
}

type workflowSubjectReceiptHint struct {
	actionCallID string
	action       string
}

// Hint metadata only skips unrelated receipts before a potentially large
// ActionCall read. It never supplies success, authorization, or subject facts.
func workflowSubjectEvidenceHint(hint map[string]interface{}, source *WorkflowEventSource) (string, string, bool) {
	if source == nil || !source.EvidenceRequired || hint == nil {
		return "", "", false
	}
	bindingID, _ := hint["bindingId"].(string)
	action, _ := hint["action"].(string)
	actionCallID, _ := hint["actionCallId"].(string)
	if bindingID != source.BindingID || !validWorkflowEvidenceActionID(actionCallID) {
		return "", "", false
	}
	for _, declaration := range source.SubjectEvidence {
		if action == declaration.Action {
			return actionCallID, action, true
		}
	}
	return "", "", false
}

// legacyWorkflowReceiptHints reads metadata from at most the latest 16
// canonical history entries plus lastAction, without copying result payloads.
// Qualifying receipt IDs are deduplicated and capped at 16 scoped ActionCall
// point lookups. Earlier recipients and earlier successful sends remain
// provable after another send or failure, without a durable history query.
func legacyWorkflowReceiptHints(run *AgentRun, source *WorkflowEventSource) []workflowSubjectReceiptHint {
	if run == nil || source == nil || len(source.SubjectEvidence) == 0 || len(source.SubjectEvidence) > 16 {
		return nil
	}
	result := make([]workflowSubjectReceiptHint, 0, maximumActionHistoryEntries)
	seenIDs := map[string]bool{}
	appendHint := func(hint map[string]interface{}) {
		id, action, matched := workflowSubjectEvidenceHint(hint, source)
		if matched && !seenIDs[id] && len(result) < maximumActionHistoryEntries {
			seenIDs[id] = true
			result = append(result, workflowSubjectReceiptHint{actionCallID: id, action: action})
		}
	}
	last, _ := run.Checkpoint["lastAction"].(map[string]interface{})
	appendHint(last)
	switch entries := run.Checkpoint[actionHistoryCheckpointKey].(type) {
	case []interface{}:
		for i := len(entries) - 1; i >= max(0, len(entries)-maximumActionHistoryEntries); i-- {
			hint, _ := entries[i].(map[string]interface{})
			appendHint(hint)
		}
	case []map[string]interface{}:
		for i := len(entries) - 1; i >= max(0, len(entries)-maximumActionHistoryEntries); i-- {
			appendHint(entries[i])
		}
	}
	return result
}

func requireLegacyWorkflowSubjectEvidence(ctx context.Context, store runbookActionStore, run *AgentRun, source *WorkflowEventSource, subject string) error {
	for _, hint := range legacyWorkflowReceiptHints(run, source) {
		call, err := store.GetActionCall(ctx, run.Scope, hint.actionCallID)
		if errors.Is(err, ErrActionNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if call == nil || call.ID != hint.actionCallID || call.Action != hint.action {
			continue
		}
		known, err := workflowSubjectFromAction(run, source, call)
		if err == nil && known.ExternalConversationID == subject {
			return nil
		}
	}
	return errors.New("legacy workflow subject requires a succeeded declared action receipt from this originating chat Run proving its exact external conversation ID")
}
