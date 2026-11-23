package conversationrun

import (
	"context"
	"sort"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

// pagedMessages serves ListChannelMessages from a fixed history; every other
// store method is unused here.
type pagedMessages struct {
	models.ConversationStore
	messages []*models.ChannelMessage
	reads    int
}

func (p *pagedMessages) ListChannelMessages(_ context.Context, filter models.ChannelMessageFilter) ([]*models.ChannelMessage, error) {
	p.reads++
	var out []*models.ChannelMessage
	for _, message := range p.messages {
		if filter.BeforeSequence == 0 || message.Sequence < filter.BeforeSequence {
			out = append(out, message)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence > out[j].Sequence })
	if len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// The composer's model picks the model of the builder's own Runs: the newest
// one a person set at or before the trigger, even many pages back, and only
// in builder conversations.
func TestConversationModelIsTheComposerChoiceAtTheTrigger(t *testing.T) {
	person := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "4"}
	finn := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: AgentBuilderDeploymentID}
	history := []*models.ChannelMessage{
		{Sequence: 1, Sender: person, BuilderParameters: &models.AgentBuilderParameters{Kind: "agent", Model: "axiomcloud/automatic"}},
		{Sequence: 2, Sender: person, BuilderParameters: &models.AgentBuilderParameters{Kind: "agent", Model: "deepseek-v4-flash"}},
	}
	for sequence := int64(3); sequence <= 130; sequence++ {
		history = append(history, &models.ChannelMessage{Sequence: sequence, Sender: finn, Content: "reply"})
	}
	history = append(history, &models.ChannelMessage{Sequence: 131, Sender: person, BuilderParameters: &models.AgentBuilderParameters{Kind: "agent", Model: "gemini-4-hydrogen"}})
	builder := &models.Conversation{Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: AgentBuilderDeploymentID},
		Origin: &models.ConversationReference{Kind: models.ConversationReferenceAgentBuilder, ID: "request-1"}}

	for _, tc := range []struct {
		name    string
		trigger int64
		want    string
	}{
		{"newest choice", 131, "gemini-4-hydrogen"},
		{"choice three pages back", 130, "deepseek-v4-flash"},
		{"first message", 1, "axiomcloud/automatic"},
	} {
		store := &pagedMessages{messages: history}
		scheduler := &Scheduler{conversationStore: store}
		got, err := scheduler.conversationModel(context.Background(), builder, &models.ChannelMessage{Sequence: tc.trigger})
		if err != nil || got != tc.want {
			t.Errorf("%s: model = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}

	store := &pagedMessages{messages: history}
	plain := &models.Conversation{Owner: builder.Owner}
	if got, err := (&Scheduler{conversationStore: store}).conversationModel(context.Background(), plain, &models.ChannelMessage{Sequence: 131}); err != nil || got != "" || store.reads != 0 {
		t.Fatalf("plain chat: model = %q, %v after %d reads", got, err, store.reads)
	}
}
