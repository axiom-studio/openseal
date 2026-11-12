package runtime

import (
	"strings"
	"testing"

	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
)

// startSpokenChat posts text as the fixture user with the given response mode
// and schedules the reply Run through the production scheduler.
func startSpokenChat(f *looptest.Fixture, text, mode string) (*Conversation, *AgentRun) {
	f.T.Helper()
	conversation, _, err := f.Convs.CreateConversation(f.Ctx, runtimeconversation.CreateConversationRequest{
		Scope: f.Scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: looptest.FixtureAgent},
		Title: "Spoken chat", IdempotencyKey: "spoken-chat",
	})
	if err != nil {
		f.T.Fatal(err)
	}
	posted, err := f.Convs.PostChannelMessage(f.Ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: f.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: looptest.FixtureUser},
		Intent: MessageIntentQuestion, Content: text, ResponseMode: mode,
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "spoken-trigger",
	})
	if err != nil {
		f.T.Fatal(err)
	}
	return conversation, f.ChatRun(conversation, posted.Message)
}

// A spoken chat may end silently: turn_final_answer{silent:true} completes the
// Run and posts nothing. A text chat is never offered silent, so the same call
// is refused and the model has to write the answer, which is then posted.
func TestSilentConversationCompletionRequiresSpokenChannel(t *testing.T) {
	for _, mode := range []string{"spoken", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			looptest.EachStore(t, "spoken_silent", func(t *testing.T, store KernelStore) {
				f := looptest.NewFixture(t, store, looptest.WithRules(
					&looptest.ModelRule{Name: "silent", Times: 1, Reply: func(v looptest.StepView) ModelStepResult {
						return looptest.FinalAnswer(map[string]any{"answer": "", "silent": true})
					}},
					&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) ModelStepResult {
						return looptest.FinalAnswer(map[string]any{"answer": "I heard you."})
					}},
				))
				conversation, run := startSpokenChat(f, "Ambient speech", mode)
				if mode == "spoken" && run.Context["responseMode"] != mode {
					t.Fatal("spoken presentation was lost when scheduling the reply")
				}
				done := f.DriveToRest(run.ID, 3)
				f.MustStatus(done, AgentRunStatusCompleted)

				steps := f.Model.Calls(ModelStepPurposeStep)
				replies := f.AgentReplies(conversation.ID)
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
				for _, entry := range f.ToolEntries(run.ID) {
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
