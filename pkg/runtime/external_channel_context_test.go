package runtime

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	runtimeportfolio "github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop/runlooptest"
)

// bindChatAgent resolves the chat seams for a Run of agentID's own chat, as
// chatLoopFixture.bind does for agent-1.
func bindChatAgent(f *runlooptest.ChatFixture, run *models.AgentRun, producer agentrun.StepProducer, agentID string) *agentrun.TurnRunnerBinding {
	f.T.Helper()
	return f.BindAgent(run, agentID, producer)
}

// chatStepText is everything one model step was given: the system sections
// of the binding and the text of each rendered entry.
func chatStepText(binding *agentrun.TurnRunnerBinding, path []*models.SessionEntry) string {
	var parts []string
	for _, section := range binding.Prompt.Sections {
		parts = append(parts, section.Text)
	}
	for _, entry := range path {
		parts = append(parts, runlooptest.ChatEntryText(entry))
		if entry.User != nil {
			for _, part := range entry.User.Content[1:] {
				parts = append(parts, part.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// An external provider channel's chat step sees that channel only: not the
// Agent's DMs, objectives or runbooks. The Agent's own application chat in the
// same tenant still sees its objectives and runbooks.
func TestExternalChannelModelContextExcludesAgentPrivateWork(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		scope, owner := f.Conversation.Scope, f.Conversation.Owner
		external := func(key, title string) *models.Conversation {
			conversation, _, err := f.Service.CreateConversation(f.Ctx, conversation.CreateConversationRequest{
				Scope: scope, Owner: owner, Title: title, IdempotencyKey: key,
				Origin: &models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: "same-slack-app", Version: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			return conversation
		}
		personal := f.Conversation
		f.Conversation = external("private-dm", "Private DM")
		f.Post("private-message", "user-1", "PRIVATE_DM_SECRET")
		public := external("public-channel", "Public channel")
		portfolio, ok := f.Store.(interface {
			models.RunbookActivationStore
			models.PortfolioStore
		})
		if !ok {
			t.Fatalf("%T has no runbook activations", f.Store)
		}
		objective, err := runtimeportfolio.NewPortfolioService(portfolio).CreateObjective(f.Ctx, runtimeportfolio.CreateObjectiveRequest{Scope: scope, Owner: owner, Title: "PRIVATE_OBJECTIVE", Goal: "PRIVATE_OBJECTIVE_SECRET", Status: models.ObjectiveStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := routine.NewRunbookActivationService(portfolio).Create(f.Ctx, routine.CreateRunbookActivationRequest{
			Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID, DefinitionID: "private-workflow", DefinitionVersion: "1",
			TriggerID: "scheduled", Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "private-workflow", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}},
			Status: models.RunbookActivationActive, IdempotencyKey: "private-workflow",
		}); err != nil {
			t.Fatal(err)
		}

		ask := func(conversation *models.Conversation, key, question string) string {
			t.Helper()
			f.Conversation = conversation
			run := f.StartRun(f.Post(key, "user-1", question))
			producer := &runlooptest.ChatProducer{Answers: []string{"Answered."}}
			binding := bindChatAgent(f, run, producer, owner.ID)
			if exit := f.Drive(run, binding); exit.Run.Status != models.AgentRunStatusCompleted || len(producer.Paths) != 1 {
				t.Fatalf("%s: exit %s, run %s, steps %d", key, exit.Reason, exit.Run.Status, len(producer.Paths))
			}
			return chatStepText(binding, producer.Paths[0])
		}
		channel := ask(public, "public-message", "PUBLIC_CHANNEL_QUESTION")
		if !strings.Contains(channel, "PUBLIC_CHANNEL_QUESTION") || !strings.Contains(channel, conversation.ConversationExternalSourceRules) {
			t.Fatalf("current channel context missing:\n%s", channel)
		}
		for _, secret := range []string{"PRIVATE_DM_SECRET", "PRIVATE_OBJECTIVE_SECRET", "private-workflow"} {
			if strings.Contains(channel, secret) {
				t.Fatalf("external channel exposed %s:\n%s", secret, channel)
			}
		}
		control := ask(personal, "personal-message", "What am I working on?")
		for _, want := range []string{"PRIVATE_OBJECTIVE_SECRET", "private-workflow"} {
			if !strings.Contains(control, want) {
				t.Fatalf("personal chat lost its private portfolio context %s:\n%s", want, control)
			}
		}
		if strings.Contains(control, conversation.ConversationExternalSourceRules) || strings.Contains(control, "PRIVATE_DM_SECRET") {
			t.Fatalf("personal chat got external channel context:\n%s", control)
		}
	})
}
