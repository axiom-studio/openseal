package models

import (
	"testing"
)

func TestFailedConversationReportCandidateNeedsASpeaker(t *testing.T) {
	context := map[string]interface{}{ConversationRunConversationIDContextKey: "c1", ConversationRunTriggerIDContextKey: "m1"}
	team := ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-gtm"}
	routing := &AgentRun{ID: "r1", Status: AgentRunStatusFailed, Kind: RunKindConversation, Owner: team, Context: context}
	member := &AgentRun{ID: "r2", Status: AgentRunStatusFailed, Kind: RunKindConversation, Owner: team, AssignedAgentID: "ada", Context: context}
	if FailedConversationReportCandidate(routing) {
		t.Fatal("a Team routing Run has no Agent to speak a failure reply")
	}
	if !FailedConversationReportCandidate(member) {
		t.Fatal("a Team member's failed Run answers as that member")
	}
}
