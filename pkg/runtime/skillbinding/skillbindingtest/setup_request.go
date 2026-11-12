package skillbindingtest

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// SkillSetupRequest is a pending configure request for the Reddit reader.
func SkillSetupRequest() *models.SkillSetupRequest {
	now := time.Now().UTC()
	return &models.SkillSetupRequest{ID: "request-1", Scope: models.Scope{Kind: "tenant", ID: "a"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, DeploymentID: "agent", AgentID: "agent", ConversationID: "chat", TriggerMessageID: "message", RunID: "run", ActionCallID: "call", Kind: "configure", SkillID: "reddit.reader", SkillVersion: "1.0.0", SkillName: "Reddit", Reason: "Read the requested community", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
}
