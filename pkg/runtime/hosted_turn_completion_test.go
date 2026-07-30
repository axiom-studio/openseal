package runtime

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestHostedActionInvocationContractsMakeExternalIdentityExplicit(t *testing.T) {
	contracts := ProjectHostedActionInvocationContracts([]capability.ModelAction{{
		Name: "browser.commit", ExternalOperationPolicy: capability.ExternalOperationRequired,
	}})
	if len(contracts) != 1 || contracts[0].ExternalOperation.Policy != capability.ExternalOperationRequired ||
		!hostedContractContains(contracts[0].ModelAuthoredFields, "externalOperation") ||
		contracts[0].ExternalOperation.Example == nil || contracts[0].CompletionEvidence == "" {
		t.Fatalf("contract = %#v", contracts)
	}
}

func TestHostedActionInvocationContractsForbidExternalIdentityForNonExternalActionsWithOmittedPolicy(t *testing.T) {
	contracts := ProjectHostedActionInvocationContracts([]capability.ModelAction{
		{Name: "browser.start", SideEffect: capability.SideEffectRead},
		{Name: "browser.commit", SideEffect: capability.SideEffectExternal},
	})
	if len(contracts) != 2 {
		t.Fatalf("contracts = %#v", contracts)
	}
	if contracts[0].ExternalOperation.Policy != capability.ExternalOperationForbidden ||
		!strings.Contains(contracts[0].ExternalOperation.Instruction, "Forbidden") {
		t.Fatalf("non-external contract = %#v", contracts[0])
	}
	if contracts[1].ExternalOperation.Policy != capability.ExternalOperationOptional {
		t.Fatalf("external contract = %#v", contracts[1])
	}
}

func TestHostedTurnCompletionRejectsFabricatedExternalURL(t *testing.T) {
	request := HostedTurnRequest{Goal: "Post a comment", ContinuationCheckpoint: checkpointTerminalAction(nil, &ActionCall{
		ID: "snapshot", SkillID: "browser", Action: "snapshot", Status: ActionCallStatusSucceeded,
		Output: map[string]interface{}{"url": "https://forum.example/thread/42"},
	}, nil)}
	response := &HostedTurnResponse{NextRunStatus: AgentRunStatusCompleted,
		OutputSummary: "Comment posted at https://forum.example/thread/42/comments/fabricated"}
	err := ValidateHostedTurnCompletion(request, response)
	if err == nil || !strings.Contains(err.Error(), "without model-visible evidence") {
		t.Fatalf("error = %v", err)
	}
}

func TestHostedTurnCompletionRequiresAndAcceptsExternalReceipt(t *testing.T) {
	checkpoint := checkpointTerminalAction(nil, &ActionCall{
		ID: "commit-1", SkillID: "browser", Action: "commit", Status: ActionCallStatusSucceeded,
		ExternalOperationDigest: "receipt-digest",
		Output:                  map[string]interface{}{"permalink": "https://forum.example/thread/42/comments/7"},
	}, nil)
	request := HostedTurnRequest{Goal: "Post a comment", ContinuationCheckpoint: checkpoint}
	response := &HostedTurnResponse{NextRunStatus: AgentRunStatusCompleted,
		OutputSummary: "Comment posted at https://forum.example/thread/42/comments/7"}
	if err := ValidateHostedTurnCompletion(request, response); err == nil || !strings.Contains(err.Error(), "without citing") {
		t.Fatalf("missing receipt error = %v", err)
	}
	response.CompletionEvidenceRefs = []string{"action-call:commit-1"}
	if err := ValidateHostedTurnCompletion(request, response); err != nil {
		t.Fatalf("receipt-backed completion = %v", err)
	}
}

func hostedContractContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
