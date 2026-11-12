package runtime

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// A Team member's chat step gets the message's attachments exactly as a 1:1
// chat step does: the same content parts on the trigger's user entry and the
// same "Attached:" line. Only the member also gets the Team context.
func TestTeamMemberStepGetsTheSameAttachmentsAsOneToOne(t *testing.T) {
	forConversationThreadStartStores(t, func(t *testing.T, base conversationThreadStartTestStore, _ func(*testing.T) conversationThreadStartTestStore) {
		content := "company,total\nAcme,42"
		artifacts, ok := base.(interface {
			CreateArtifactVersion(context.Context, *Artifact, int64) error
		})
		if !ok {
			t.Fatalf("%T stores no artifacts", base)
		}
		ask := func(team bool) (*SessionEntry, *SessionEntry, *agentrun.TurnRunnerBinding, *chatLoopFixture) {
			t.Helper()
			f := newChatLoopFixture(t, base, chatLoopOptions{team: team})
			artifact := CSVArtifact()
			artifact.Scope = f.conversation.Scope
			artifact.MediaType, artifact.SizeBytes, artifact.Digest = "text/csv", int64(len(content)), Digest(content)
			if err := artifacts.CreateArtifactVersion(f.ctx, artifact, 0); err != nil {
				t.Fatal(err)
			}
			f.runner = f.newRunner(ConversationRunTurnRunnerConfig{AttachmentContent: &TextContent{text: content}})
			posted, err := f.service.PostChannelMessage(f.ctx, PostChannelMessageRequest{
				Scope: f.conversation.Scope, ConversationID: f.conversation.ID, ExpectedRevision: f.conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user-1"}, Intent: MessageIntentQuestion,
				Content: "What does the attached file total?", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				References:     []ConversationReference{{Kind: ConversationReferenceArtifact, ID: artifact.ID, Version: 1}},
				IdempotencyKey: "question", RequiresResponse: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			f.conversation = posted.Conversation
			run := f.startRun(posted.Message)
			producer := &chatLoopProducer{answers: []string{"Acme totals 42."}}
			binding := f.bind(run, producer)
			if exit := f.drive(run, binding); exit.Run.Status != AgentRunStatusCompleted || len(producer.paths) != 1 {
				t.Fatalf("team=%v: exit %s, run %s, steps %d", team, exit.Reason, exit.Run.Status, len(producer.paths))
			}
			var trigger, note *SessionEntry
			for _, entry := range producer.paths[0] {
				switch {
				case entry.User != nil && entry.User.MessageID == posted.Message.ID:
					trigger = entry
				case entry.Note != nil && entry.Note.Kind == NoteRunContext:
					note = entry
				}
			}
			if trigger == nil || note == nil {
				t.Fatalf("team=%v: step path = %v", team, chatLoopShape(producer.paths[0]))
			}
			return trigger, note, binding, f
		}
		direct, directNote, directBinding, _ := ask(false)
		member, memberNote, memberBinding, team := ask(true)

		want := ContentPart{Type: ContentPartText, Text: "<attachment id=\"file\" version=\"1\" name=\"file.csv\" status=\"supplied\">\n" + content + "\n</attachment>"}
		if len(direct.User.Content) != 2 || !reflect.DeepEqual(direct.User.Content[1], want) {
			t.Fatalf("1:1 attachment parts = %#v", direct.User.Content)
		}
		if !reflect.DeepEqual(member.User.Content[1:], direct.User.Content[1:]) {
			t.Fatalf("Team member attachment parts = %#v, 1:1 = %#v", member.User.Content[1:], direct.User.Content[1:])
		}
		for name, entry := range map[string]*SessionEntry{"1:1": direct, "member": member} {
			if !strings.HasSuffix(chatLoopEntryText(entry), "\nAttached: file.csv (artifact file v1)") {
				t.Fatalf("%s trigger = %q", name, chatLoopEntryText(entry))
			}
		}
		if strings.Contains(chatStepText(directBinding, []*SessionEntry{directNote}), teamMemberRules) ||
			!strings.Contains(chatStepText(memberBinding, []*SessionEntry{memberNote}), teamMemberRules) {
			t.Fatal("Team rules reached the 1:1 step or missed the member step")
		}
		answers := team.agentMessages()
		if len(answers) != 1 || answers[0].Sender != AgentParticipant("agent-1") || answers[0].Content != "Acme totals 42." {
			t.Fatalf("member answer = %#v", answers)
		}
	})
}

// teamMemberRules is the Team channel section a member's step gets with its
// roster. The test spells it out to pin the text the model sees.
const teamMemberRules = "You are one member of this Team channel and answer under your own name. " +
	"Answer the current message yourself; do not speak for teammates. " +
	"To bring a teammate in, end your answer with a one-line @Name mention and the exact ask; that teammate is then asked to answer. " +
	"Never @mention teammates just to agree, thank or acknowledge. " +
	"Prefer a reaction (👀 picked up, ✅ done, 👍 agree) over a filler message."
