package skillbinding_test

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
)

func TestSkillBindingTargetRefusesInvalidTargets(t *testing.T) {
	f := skillbindingtest.NewTargetFixture(t)
	run := f.ChatRun(t, "finn")
	for target, want := range map[string]string{
		"finn": "the Agent you are", "old": "retired", "platform": "cannot be changed in chat", "nobody": "does not exist",
	} {
		if _, err := f.Validator.ValidateActionProposal(f.Ctx, agentrun.ActionProposalValidationInput{
			Run: run, Bound: f.Bound(t, "finn", skillbinding.SkillActionUpsertBinding), Arguments: skillbindingtest.ReaderUpsertArguments(target),
		}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("target %q = %v, want %q", target, err, want)
		}
	}
	// Only a reply in the builder's own chat may act for another Agent.
	teamRun := *run
	teamRun.Owner = models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "ops-team"}
	if _, err := skillbinding.SkillActionTarget(f.Ctx, f.Catalog, f.Agents, &teamRun, "finn", skillbindingtest.ReaderUpsertArguments("researcher")); err == nil {
		t.Fatal("a Team chat Run acted for another Agent")
	}
	taskRun := *run
	taskRun.Context = map[string]interface{}{models.ConversationTaskContextKey: "task-1"}
	if _, err := skillbinding.SkillActionTarget(f.Ctx, f.Catalog, f.Agents, &taskRun, "finn", skillbindingtest.ReaderUpsertArguments("researcher")); err == nil {
		t.Fatal("a conversation task Run acted for another Agent")
	}
	// A host without the Agent registry refuses every target.
	bare, _ := skillbinding.NewSkillBindingActionValidator(f.Catalog, f.Store)
	if _, err := bare.ValidateActionProposal(f.Ctx, agentrun.ActionProposalValidationInput{
		Run: run, Bound: f.Bound(t, "finn", skillbinding.SkillActionUpsertBinding), Arguments: skillbindingtest.ReaderUpsertArguments("researcher"),
	}); err == nil {
		t.Fatal("a validator without the Agent registry accepted a target")
	}
}

func TestSkillBindingTargetIgnoresBlankTarget(t *testing.T) {
	f := skillbindingtest.NewTargetFixture(t)
	run := f.ChatRun(t, "writer")
	for _, blank := range []string{"", " ", "\t\n"} {
		target, err := skillbinding.SkillActionTarget(f.Ctx, f.Catalog, f.Agents, run, "writer", map[string]interface{}{skillbinding.SkillActionTargetAgentArgument: blank})
		if err != nil || target != nil {
			t.Fatalf("blank target %q = %#v, %v; want none", blank, target, err)
		}
	}
}
