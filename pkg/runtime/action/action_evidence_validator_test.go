package action

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// runHistoryFixture seeds terminal action calls into an in-memory store and reads
// them back through the same RunActionHistory Propose builds.
type runHistoryFixture struct {
	t     *testing.T
	store *sqlite.Store
	scope models.Scope
	base  time.Time
	next  int
}

func newRunHistoryFixture(t *testing.T) *runHistoryFixture {
	return &runHistoryFixture{t: t, store: newActionTestStore(t), scope: models.Scope{Kind: "tenant", ID: "one"}, base: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)}
}

// add stores call as completed after every call added before it.
func (f *runHistoryFixture) add(call models.ActionCall) {
	f.next++
	completed := f.base.Add(time.Duration(f.next) * time.Second)
	if call.RunID == "" {
		call.RunID = "run"
	}
	if call.Origin == "" {
		call.Origin = models.ActionOriginLoop
	}
	call.Scope, call.TurnID = f.scope, "step-1"
	call.CreatedAt, call.UpdatedAt, call.CompletedAt = completed, completed, &completed
	storetest.SeedActionCall(f.t, f.store, &call)
}

func (f *runHistoryFixture) history() agentrun.RunActionHistory {
	return agentrun.NewRunActionHistory(f.store, f.scope, "run")
}

func fillEvidenceAction() *skill.BoundAction {
	return &skill.BoundAction{Definition: &skill.Definition{ID: "browser", Version: "2"}, Action: skill.Action{
		Name: "commit", RequiredEvidence: []skill.ActionEvidenceRequirement{{Action: "fill", MatchingArguments: []string{"sessionId"}}},
	}}
}

func validateEvidence(history agentrun.RunActionHistory, arguments map[string]interface{}) error {
	_, err := RequiredActionEvidenceValidator{}.ValidateActionProposal(context.Background(), agentrun.ActionProposalValidationInput{
		Run: &models.AgentRun{ID: "run"}, Bound: fillEvidenceAction(), Arguments: arguments, History: history,
	})
	return err
}

func requireMissingFillEvidence(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "successful fill evidence") {
		t.Fatalf("missing evidence error = %v", err)
	}
	var recovery actionAdvanceRecoveryError
	if !errors.As(err, &recovery) {
		t.Fatalf("evidence failure is not recoverable by authoritative action advance: %T", err)
	}
	if prerequisite, required := recovery.actionAdvanceRecovery(); !required || prerequisite != "fill" {
		t.Fatalf("evidence recovery = %q, %v", prerequisite, required)
	}
}

func TestRequiredEvidenceFromSucceededRunCalls(t *testing.T) {
	f := newRunHistoryFixture(t)
	arguments := map[string]interface{}{"sessionId": "session-a"}
	// No history at all (validators called outside Propose) and an empty Run
	// history both lack the evidence; a model summary cannot stand in for it.
	requireMissingFillEvidence(t, validateEvidence(nil, arguments))
	requireMissingFillEvidence(t, validateEvidence(f.history(), arguments))

	f.add(models.ActionCall{ID: "fill", SkillID: "browser", SkillVersion: "2", Action: "fill", Status: models.ActionCallStatusSucceeded,
		Arguments: map[string]interface{}{"sessionId": "session-a", "value": "exact draft"}})
	// A later unrelated failure does not hide the earlier success.
	f.add(models.ActionCall{ID: "read", SkillID: "browser", SkillVersion: "2", Action: "read", Status: models.ActionCallStatusFailed})
	if err := validateEvidence(f.history(), arguments); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredEvidenceIgnoresVMFailedAndOtherRunCalls(t *testing.T) {
	f := newRunHistoryFixture(t)
	fill := func(id string, mutate func(*models.ActionCall)) {
		call := models.ActionCall{ID: id, SkillID: "browser", SkillVersion: "2", Action: "fill", Status: models.ActionCallStatusSucceeded,
			Arguments: map[string]interface{}{"sessionId": "session-a"}}
		mutate(&call)
		f.add(call)
	}
	fill("vm", func(c *models.ActionCall) { c.Origin = models.ActionOriginVM })
	fill("failed", func(c *models.ActionCall) { c.Status = models.ActionCallStatusFailed })
	fill("denied", func(c *models.ActionCall) { c.Status = models.ActionCallStatusDenied })
	fill("refused", func(c *models.ActionCall) { c.Status = models.ActionCallStatusRefused })
	fill("other-run", func(c *models.ActionCall) { c.RunID = "other" })
	fill("other-version", func(c *models.ActionCall) { c.SkillVersion = "1" })
	fill("other-skill", func(c *models.ActionCall) { c.SkillID = "forms" })
	fill("ready", func(c *models.ActionCall) { c.Status = models.ActionCallStatusReady; c.CompletedAt = nil })
	requireMissingFillEvidence(t, validateEvidence(f.history(), map[string]interface{}{"sessionId": "session-a"}))
}

func TestRequiredEvidenceMatchingArguments(t *testing.T) {
	f := newRunHistoryFixture(t)
	f.add(models.ActionCall{ID: "fill-a", SkillID: "browser", SkillVersion: "2", Action: "fill", Status: models.ActionCallStatusSucceeded,
		Arguments: map[string]interface{}{"sessionId": "session-a", "value": "exact draft"}})
	f.add(models.ActionCall{ID: "fill-c", SkillID: "browser", SkillVersion: "2", Action: "fill", Status: models.ActionCallStatusSucceeded,
		Arguments: map[string]interface{}{"sessionId": "session-c"}})
	history := f.history()
	for _, session := range []string{"session-a", "session-c"} {
		if err := validateEvidence(history, map[string]interface{}{"sessionId": session, "other": "ignored"}); err != nil {
			t.Fatalf("%s: %v", session, err)
		}
	}
	requireMissingFillEvidence(t, validateEvidence(history, map[string]interface{}{"sessionId": "session-b"}))
	requireMissingFillEvidence(t, validateEvidence(history, map[string]interface{}{}))
}
