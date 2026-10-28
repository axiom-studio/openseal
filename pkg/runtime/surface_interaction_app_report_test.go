package runtime

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

// An app reports results as repeatable interactions on the app artifact
// itself: surfaceArtifactId and surfaceId are the app's artifact id.
func TestAppReportsAreRepeatableInteractionsOnTheAppArtifact(t *testing.T) {
	for _, fixture := range surfaceInteractionStores(t) {
		t.Run(fixture.name, func(t *testing.T) {
			f := newSurfaceInteractionFixture(t, fixture.store(t))
			const app = "artifact-app-times-tables"
			conversation, err := f.conversations.GetConversation(f.ctx, f.scope, f.conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			posted, err := f.conversations.PostChannelMessage(f.ctx, PostChannelMessageRequest{
				Scope: f.scope, ConversationID: f.conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "finn"}, Intent: MessageIntentUpdate,
				Content: "Here's your times tables app.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "app-message",
				References: []ConversationReference{{Kind: ConversationReferenceArtifact, ID: app, Version: 3}},
			})
			if err != nil {
				t.Fatal(err)
			}
			rule, err := presentation.AppInteraction(presentation.AppReportComponentID, presentation.AppReportAction, "")
			if err != nil || rule.SingleUse || rule.Shared || rule.ItemKeyed || !rule.NotifyAgent {
				t.Fatalf("rule = %#v, %v", rule, err)
			}
			report := func(key, values string) RecordSurfaceInteractionRequest {
				if _, err := presentation.ValidateAppReport(json.RawMessage(values)); err != nil {
					t.Fatal(err)
				}
				return RecordSurfaceInteractionRequest{
					SurfaceArtifactID: app, SurfaceID: app, SurfaceVersion: 3, MessageID: posted.Message.ID,
					ComponentID: presentation.AppReportComponentID, ActionID: presentation.AppReportAction, Actor: surfaceInteractionUser("ana"),
					Repeatable: !rule.SingleUse, Values: json.RawMessage(values), IdempotencyKey: key,
				}
			}
			for _, req := range []RecordSurfaceInteractionRequest{
				report("r1", `{"values":{"score":7,"of":10},"summary":"Scored 7 out of 10"}`),
				report("r2", `{"values":{"score":8,"of":10},"summary":"Scored 8 out of 10"}`),
			} {
				if stored, replayed, err := f.record(t, req); err != nil || replayed || stored.SurfaceID != app || stored.SurfaceArtifactID != app {
					t.Fatalf("%s = %#v, %t, %v", req.IdempotencyKey, stored, replayed, err)
				}
			}
			if replay, replayed, err := f.record(t, report("r1", `{"values":{"score":7,"of":10},"summary":"Scored 7 out of 10"}`)); err != nil || !replayed || replay.Sequence != 1 {
				t.Fatalf("replay = %#v, %t, %v", replay, replayed, err)
			}
			all, err := f.conversations.ListSurfaceInteractions(f.ctx, SurfaceInteractionFilter{Scope: f.scope, ConversationID: f.conversation.ID, SurfaceID: app})
			if err != nil || !reflect.DeepEqual(surfaceInteractionKeys(all), []string{"r1", "r2"}) {
				t.Fatalf("reports = %v, %v", surfaceInteractionKeys(all), err)
			}
		})
	}
}
