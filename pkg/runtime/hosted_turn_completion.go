package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ValidateHostedTurnCompletion prevents a model-authored terminal response
// from becoming evidence. External effects must cite a succeeded, kernel-owned
// ActionCall receipt, and URLs must already be present in the model-visible
// evidence envelope. The policy is deliberately domain-neutral.
func ValidateHostedTurnCompletion(request HostedTurnRequest, response *HostedTurnResponse) error {
	if response == nil || response.NextRunStatus != AgentRunStatusCompleted {
		return nil
	}
	if len(response.EvidenceClaims) > 0 {
		snapshot, err := evidenceSnapshotForGrounding(request.InputContext)
		if err != nil {
			return err
		}
		// A grounded draft uses durable repair/review instead of a terminal
		// validation error when its human-readable report is incomplete.
		if snapshot == nil {
			report, ok := response.RunOutput["report"].(string)
			if !ok || strings.TrimSpace(report) == "" {
				return errors.New("evidence-backed completion requires the complete human-readable deliverable in runOutput.report; runOutput.summary, outputSummary, and evidenceClaims do not replace the report")
			}
		}
	}
	history := actionHistoryEntries(request.ContinuationCheckpoint)
	succeeded := make(map[string]map[string]interface{}, len(history))
	for _, entry := range history {
		if fmt.Sprint(entry["status"]) == string(ActionCallStatusSucceeded) {
			succeeded[fmt.Sprint(entry["actionCallId"])] = entry
		}
	}
	for _, reference := range response.CompletionEvidenceRefs {
		id := strings.TrimPrefix(strings.TrimSpace(reference), "action-call:")
		if id == reference || id == "" || succeeded[id] == nil {
			return fmt.Errorf("completion evidence %q does not reference a succeeded durable ActionCall", reference)
		}
	}

	output, _ := json.Marshal(struct {
		Summary string                 `json:"summary"`
		Run     map[string]interface{} `json:"run"`
	}{response.OutputSummary, response.RunOutput})
	input, _ := MarshalHostedTurnModelInput(request)
	for _, candidate := range hostedTurnURLPattern.FindAllString(string(output), -1) {
		candidate = strings.TrimRight(candidate, ".,);]}")
		if !strings.Contains(string(input), candidate) {
			return fmt.Errorf("completed Turn returned URL %q without model-visible evidence", candidate)
		}
	}

	if !claimsExternalMutation(string(output)) {
		return nil
	}
	for _, reference := range response.CompletionEvidenceRefs {
		entry := succeeded[strings.TrimPrefix(strings.TrimSpace(reference), "action-call:")]
		if strings.TrimSpace(fmt.Sprint(entry["externalOperationDigest"])) != "" {
			return nil
		}
	}
	return errors.New("completed Turn claimed an external mutation without citing a succeeded external ActionCall receipt")
}
