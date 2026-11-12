package runtime_test

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

func TestSkillBindingTargetRefusesInvalidTargets(t *testing.T) {
	f := newSkillTargetFixture(t)
	run := f.chatRun(t, "finn")
	for target, want := range map[string]string{
		"finn": "the Agent you are", "old": "retired", "platform": "cannot be changed in chat", "nobody": "does not exist",
	} {
		if _, err := f.validator.ValidateActionProposal(f.ctx, agentrun.ActionProposalValidationInput{
			Run: run, Bound: f.bound(t, "finn", runtime.SkillActionUpsertBinding), Arguments: readerUpsertArguments(target),
		}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("target %q = %v, want %q", target, err, want)
		}
	}
	// Only a reply in the builder's own chat may act for another Agent.
	teamRun := *run
	teamRun.Owner = models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "ops-team"}
	if _, err := runtime.SkillActionTarget(f.ctx, f.catalog, f.agents, &teamRun, "finn", readerUpsertArguments("researcher")); err == nil {
		t.Fatal("a Team chat Run acted for another Agent")
	}
	taskRun := *run
	taskRun.Context = map[string]interface{}{models.ConversationTaskContextKey: "task-1"}
	if _, err := runtime.SkillActionTarget(f.ctx, f.catalog, f.agents, &taskRun, "finn", readerUpsertArguments("researcher")); err == nil {
		t.Fatal("a conversation task Run acted for another Agent")
	}
	// A host without the Agent registry refuses every target.
	bare, _ := runtime.NewSkillBindingActionValidator(f.catalog, f.store)
	if _, err := bare.ValidateActionProposal(f.ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.bound(t, "finn", runtime.SkillActionUpsertBinding), Arguments: readerUpsertArguments("researcher"),
	}); err == nil {
		t.Fatal("a validator without the Agent registry accepted a target")
	}
}

func TestSkillBindingTargetIgnoresBlankTarget(t *testing.T) {
	f := newSkillTargetFixture(t)
	run := f.chatRun(t, "writer")
	for _, blank := range []string{"", " ", "\t\n"} {
		target, err := runtime.SkillActionTarget(f.ctx, f.catalog, f.agents, run, "writer", map[string]interface{}{runtime.SkillActionTargetAgentArgument: blank})
		if err != nil || target != nil {
			t.Fatalf("blank target %q = %#v, %v; want none", blank, target, err)
		}
	}
}
