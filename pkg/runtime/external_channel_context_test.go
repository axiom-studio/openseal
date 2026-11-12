package runtime

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
)

// bindChatAgent resolves the chat seams for a Run of agentID's own chat, as
// chatLoopFixture.bind does for agent-1.
func bindChatAgent(f *chatLoopFixture, run *AgentRun, producer agentrun.StepProducer, agentID string) *agentrun.TurnRunnerBinding {
	f.t.Helper()
	return f.bindAgent(run, agentID, producer)
}

// chatStepText is everything one model step was given: the system sections
// of the binding and the text of each rendered entry.
func chatStepText(binding *agentrun.TurnRunnerBinding, path []*SessionEntry) string {
	var parts []string
	for _, section := range binding.Prompt.Sections {
		parts = append(parts, section.Text)
	}
	for _, entry := range path {
		parts = append(parts, chatLoopEntryText(entry))
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
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		scope, owner := f.conversation.Scope, f.conversation.Owner
		external := func(key, title string) *Conversation {
			conversation, _, err := f.service.CreateConversation(f.ctx, conversation.CreateConversationRequest{
				Scope: scope, Owner: owner, Title: title, IdempotencyKey: key,
				Origin: &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "same-slack-app", Version: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			return conversation
		}
		personal := f.conversation
		f.conversation = external("private-dm", "Private DM")
		f.post("private-message", "user-1", "PRIVATE_DM_SECRET")
		public := external("public-channel", "Public channel")
		portfolio, ok := f.store.(interface {
			RunbookActivationStore
			PortfolioStore
		})
		if !ok {
			t.Fatalf("%T has no runbook activations", f.store)
		}
		objective, err := NewPortfolioService(portfolio).CreateObjective(f.ctx, CreateObjectiveRequest{Scope: scope, Owner: owner, Title: "PRIVATE_OBJECTIVE", Goal: "PRIVATE_OBJECTIVE_SECRET", Status: ObjectiveStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := routine.NewRunbookActivationService(portfolio).Create(f.ctx, routine.CreateRunbookActivationRequest{
			Scope: scope, Owner: owner, ObjectiveID: objective.ID, AssignedAgentID: owner.ID, DefinitionID: "private-workflow", DefinitionVersion: "1",
			TriggerID: "scheduled", Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "private-workflow", Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}},
			Status: RunbookActivationActive, IdempotencyKey: "private-workflow",
		}); err != nil {
			t.Fatal(err)
		}

		ask := func(conversation *Conversation, key, question string) string {
			t.Helper()
			f.conversation = conversation
			run := f.startRun(f.post(key, "user-1", question))
			producer := &chatLoopProducer{answers: []string{"Answered."}}
			binding := bindChatAgent(f, run, producer, owner.ID)
			if exit := f.drive(run, binding); exit.Run.Status != AgentRunStatusCompleted || len(producer.paths) != 1 {
				t.Fatalf("%s: exit %s, run %s, steps %d", key, exit.Reason, exit.Run.Status, len(producer.paths))
			}
			return chatStepText(binding, producer.paths[0])
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
