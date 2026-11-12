package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

// TestAgentBuilderConversationNeedsNoParticipationOptIn: a host that leaves
// unconfigured chats inert (the OSS daemon) still answers a builder chat,
// and only a builder chat.
func TestAgentBuilderConversationNeedsNoParticipationOptIn(t *testing.T) {
	builder := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: AgentBuilderDeploymentID}
	origin := &models.ConversationReference{Kind: models.ConversationReferenceAgentBuilder, ID: "request-1"}
	message := &models.ChannelMessage{Sequence: 1}
	for _, tc := range []struct {
		name         string
		conversation *models.Conversation
		want         bool
	}{
		{"builder chat", &models.Conversation{Owner: builder, Origin: origin, Status: models.ConversationStatusActive}, true},
		{"archived builder chat", &models.Conversation{Owner: builder, Origin: origin, Status: models.ConversationStatusArchived}, false},
		{"builder's other chat", &models.Conversation{Owner: builder, Status: models.ConversationStatusActive}, false},
		{"another agent's builder-origin chat", &models.Conversation{Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "helper"}, Origin: origin, Status: models.ConversationStatusActive}, false},
		{"opted-in chat", &models.Conversation{Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}, Status: models.ConversationStatusActive, Participation: &models.ConversationParticipation{Enabled: true}}, true},
	} {
		if got := conversationParticipationAllows(tc.conversation, message); got != tc.want {
			t.Errorf("%s: allows = %v, want %v", tc.name, got, tc.want)
		}
	}
}
