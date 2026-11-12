package models

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestRefusedStatusValidatesWithoutExternalDigest(t *testing.T) {
	now := time.Now().UTC()
	call := &ActionCall{
		ID: "refused", Scope: Scope{Kind: "tenant", ID: "t"}, RunID: "run", TurnID: "step-3", DeploymentID: "agent",
		SkillID: "repo", SkillVersion: "1.0.0", Action: "read_file", Origin: ActionOriginLoop, Execution: ActionExecutionLoop,
		Status: ActionCallStatusRefused, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
		Arguments: map[string]interface{}{}, DuplicateOfActionCallID: "prior", MaxAttempts: 1, Revision: 1,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
	}
	if err := call.Validate(); err != nil {
		t.Fatalf("refused duplicate without digest: %v", err)
	}
	call.Status = ActionCallStatusFailed
	if err := call.Validate(); err == nil {
		t.Fatal("a non-refused duplicate without an external operation digest validated")
	}
}

func TestActionCallDurableJSONPreservesEmptyArgumentObject(t *testing.T) {
	call := ActionCall{Arguments: map[string]interface{}{}}
	payload, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ActionCall
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Arguments == nil {
		t.Fatalf("empty action arguments were lost from durable JSON: %s", payload)
	}
}

func TestNestedApprovalResolutionNeverUpdatesRun(t *testing.T) {
	now := time.Now().UTC()
	scope := Scope{Kind: "tenant", ID: "one"}
	call := &ActionCall{
		ID: "nested", Scope: scope, RunID: "run", TurnID: "step-1", DeploymentID: "agent", SkillID: "repo", SkillVersion: "1.0.0", Action: "push",
		Origin: ActionOriginVM, Status: ActionCallStatusReady, ApprovalID: "approval-1", MaxAttempts: 1, Revision: 2, CreatedAt: now, UpdatedAt: now,
	}
	approval := &ApprovalCheckpoint{
		ID: "approval-1", Scope: scope, RunID: "run", ActionCallID: call.ID, Status: ApprovalStatusApproved, Summary: "Push",
		EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "user"}}, ExpiresAt: now.Add(time.Hour), Revision: 2,
		DecisionID: "decision", DecisionBy: &ApprovalPrincipal{Type: "user", ID: "user"}, DecidedAt: &now,
	}
	event := &ActivityEvent{ID: "event", Scope: scope, EventType: "approval.approved", Severity: ActivitySeverityInfo, RunID: "run",
		Actor: ActivityActor{Type: "user", ID: "user"}, Summary: "Approved", Visibility: ActivityVisibilityScope, CreatedAt: now}
	run := &AgentRun{ID: "run", Scope: scope}
	if err := ValidateApprovalResolutionRecord(ApprovalResolutionRecord{Approval: approval, Call: call, Run: run, Event: event}); err == nil {
		t.Fatal("a nested call's approval resolution updated its Run")
	}
	if err := ValidateApprovalResolutionRecord(ApprovalResolutionRecord{Approval: approval, Call: call, Event: event}); err != nil {
		t.Fatalf("nested resolution without Run = %v", err)
	}
}
