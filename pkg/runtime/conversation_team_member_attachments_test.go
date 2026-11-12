package runtime

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop/runlooptest"
)

// A Team member's chat step gets the message's attachments exactly as a 1:1
// chat step does: the same content parts on the trigger's user entry and the
// same "Attached:" line. Only the member also gets the Team context.
func TestTeamMemberStepGetsTheSameAttachmentsAsOneToOne(t *testing.T) {
	conversationtest.ForThreadStartStores(t, func(t *testing.T, base conversationtest.ThreadStartStore, _ func(*testing.T) conversationtest.ThreadStartStore) {
		content := "company,total\nAcme,42"
		artifacts, ok := base.(interface {
			CreateArtifactVersion(context.Context, *models.Artifact, int64) error
		})
		if !ok {
			t.Fatalf("%T stores no artifacts", base)
		}
		ask := func(team bool) (*models.SessionEntry, *models.SessionEntry, *agentrun.TurnRunnerBinding, *runlooptest.ChatFixture) {
			t.Helper()
			f := runlooptest.NewChatFixture(t, base, runlooptest.ChatOptions{Team: team})
			artifact := conversationtest.CSVArtifact()
			artifact.Scope = f.Conversation.Scope
			artifact.MediaType, artifact.SizeBytes, artifact.Digest = "text/csv", int64(len(content)), conversationtest.Digest(content)
			if err := artifacts.CreateArtifactVersion(f.Ctx, artifact, 0); err != nil {
				t.Fatal(err)
			}
			f.Runner = f.NewRunner(conversationrun.TurnRunnerConfig{AttachmentContent: &conversationtest.TextContent{Text: content}})
			posted, err := f.Service.PostChannelMessage(f.Ctx, conversation.PostChannelMessageRequest{
				Scope: f.Conversation.Scope, ConversationID: f.Conversation.ID, ExpectedRevision: f.Conversation.Revision,
				Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user-1"}, Intent: models.MessageIntentQuestion,
				Content: "What does the attached file total?", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
				References:     []models.ConversationReference{{Kind: models.ConversationReferenceArtifact, ID: artifact.ID, Version: 1}},
				IdempotencyKey: "question", RequiresResponse: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			f.Conversation = posted.Conversation
			run := f.StartRun(posted.Message)
			producer := &runlooptest.ChatProducer{Answers: []string{"Acme totals 42."}}
			binding := f.Bind(run, producer)
			if exit := f.Drive(run, binding); exit.Run.Status != models.AgentRunStatusCompleted || len(producer.Paths) != 1 {
				t.Fatalf("team=%v: exit %s, run %s, steps %d", team, exit.Reason, exit.Run.Status, len(producer.Paths))
			}
			var trigger, note *models.SessionEntry
			for _, entry := range producer.Paths[0] {
				switch {
				case entry.User != nil && entry.User.MessageID == posted.Message.ID:
					trigger = entry
				case entry.Note != nil && entry.Note.Kind == models.NoteRunContext:
					note = entry
				}
			}
			if trigger == nil || note == nil {
				t.Fatalf("team=%v: step path = %v", team, runlooptest.ChatShape(producer.Paths[0]))
			}
			return trigger, note, binding, f
		}
		direct, directNote, directBinding, _ := ask(false)
		member, memberNote, memberBinding, team := ask(true)

		want := models.ContentPart{Type: models.ContentPartText, Text: "<attachment id=\"file\" version=\"1\" name=\"file.csv\" status=\"supplied\">\n" + content + "\n</attachment>"}
		if len(direct.User.Content) != 2 || !reflect.DeepEqual(direct.User.Content[1], want) {
			t.Fatalf("1:1 attachment parts = %#v", direct.User.Content)
		}
		if !reflect.DeepEqual(member.User.Content[1:], direct.User.Content[1:]) {
			t.Fatalf("Team member attachment parts = %#v, 1:1 = %#v", member.User.Content[1:], direct.User.Content[1:])
		}
		for name, entry := range map[string]*models.SessionEntry{"1:1": direct, "member": member} {
			if !strings.HasSuffix(runlooptest.ChatEntryText(entry), "\nAttached: file.csv (artifact file v1)") {
				t.Fatalf("%s trigger = %q", name, runlooptest.ChatEntryText(entry))
			}
		}
		if strings.Contains(chatStepText(directBinding, []*models.SessionEntry{directNote}), teamMemberRules) ||
			!strings.Contains(chatStepText(memberBinding, []*models.SessionEntry{memberNote}), teamMemberRules) {
			t.Fatal("Team rules reached the 1:1 step or missed the member step")
		}
		answers := team.AgentMessages()
		if len(answers) != 1 || answers[0].Sender != conversationtest.AgentParticipant("agent-1") || answers[0].Content != "Acme totals 42." {
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
