package runtime

import (
	"strings"
	"testing"

	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// startSpokenChat posts text as the fixture user with the given response mode
// and schedules the reply Run through the production scheduler.
func startSpokenChat(f *loopFixture, text, mode string) (*Conversation, *AgentRun) {
	f.t.Helper()
	conversation, _, err := f.convs.CreateConversation(f.ctx, runtimeconversation.CreateConversationRequest{
		Scope: f.scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: loopFixtureAgent},
		Title: "Spoken chat", IdempotencyKey: "spoken-chat",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	posted, err := f.convs.PostChannelMessage(f.ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: f.scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: loopFixtureUser},
		Intent: MessageIntentQuestion, Content: text, ResponseMode: mode,
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "spoken-trigger",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return conversation, f.chatRun(conversation, posted.Message)
}

// A spoken chat may end silently: turn_final_answer{silent:true} completes the
// Run and posts nothing. A text chat is never offered silent, so the same call
// is refused and the model has to write the answer, which is then posted.
func TestSilentConversationCompletionRequiresSpokenChannel(t *testing.T) {
	for _, mode := range []string{"spoken", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			eachStore(t, "spoken_silent", func(t *testing.T, store KernelStore) {
				f := newLoopFixture(t, store, withRules(
					&modelRule{name: "silent", times: 1, reply: func(v stepView) ModelStepResult {
						return finalAnswer(map[string]any{"answer": "", "silent": true})
					}},
					&modelRule{name: "answer", reply: func(stepView) ModelStepResult {
						return finalAnswer(map[string]any{"answer": "I heard you."})
					}},
				))
				conversation, run := startSpokenChat(f, "Ambient speech", mode)
				if mode == "spoken" && run.Context["responseMode"] != mode {
					t.Fatal("spoken presentation was lost when scheduling the reply")
				}
				done := f.driveToRest(run.ID, 3)
				f.mustStatus(done, AgentRunStatusCompleted)

				steps := f.model.calls(ModelStepPurposeStep)
				replies := f.agentReplies(conversation.ID)
				if mode == "spoken" {
					if len(steps) != 1 || len(replies) != 0 || done.Output["silent"] != true {
						t.Fatalf("silent spoken completion: steps=%d replies=%q output=%#v", len(steps), replies, done.Output)
					}
					return
				}
				if len(steps) != 2 || len(replies) != 1 || !strings.Contains(replies[0], "I heard you.") {
					t.Fatalf("text reply was not written after silent: steps=%d replies=%q", len(steps), replies)
				}
				var refused bool
				for _, entry := range f.toolEntries(run.ID) {
					if entry.State == ToolEntryError && strings.Contains(toolEntryText(entry), "silent") {
						refused = true
					}
				}
				if !refused {
					t.Fatal("silent final in a text chat was not refused with a tool error")
				}
			})
		})
	}
}
