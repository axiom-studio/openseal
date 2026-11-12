package runtime

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

func conversationSectionIDs(sections []ModelStepSystemSection) []string {
	ids := make([]string, 0, len(sections))
	for _, section := range sections {
		ids = append(ids, section.ID)
	}
	return ids
}

func TestConversationSystemSectionsStableAcrossRuns(t *testing.T) {
	conversation := &Conversation{ID: "conversation-1", Title: "Launch"}
	first := &AgentRun{ID: "run-1", Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}, Goal: "first", SessionID: "session-1"}
	second := &AgentRun{ID: "run-2", Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}, Goal: "second message", SessionID: "session-1", Revision: 9}
	a, err := json.Marshal(conversationSystemSections(conversation, first, false))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(conversationSystemSections(conversation, second, false))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("sections differ across Runs of one session:\n%s\n%s", a, b)
	}
	got := conversationSectionIDs(conversationSystemSections(conversation, first, false))
	if strings.Join(got, ",") != "conversation_channel,conversation_attachments" {
		t.Fatalf("ids = %v", got)
	}
}

func TestExternalAndTeamSectionsIncludedByConversationKind(t *testing.T) {
	external := &Conversation{ID: "conversation-1", Origin: &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "source-1"}}
	teamRun := &AgentRun{ID: "run-1", Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-1"}, AssignedAgentID: "agent-1"}
	agentRun := &AgentRun{ID: "run-2", Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}}
	cases := []struct {
		name         string
		conversation *Conversation
		run          *AgentRun
		spoken       bool
		want         string
	}{
		{"plain", &Conversation{ID: "c"}, agentRun, false, "conversation_channel,conversation_attachments"},
		{"external", external, agentRun, false, "conversation_channel,conversation_attachments,conversation_external_source"},
		{"team", &Conversation{ID: "c"}, teamRun, false, "conversation_channel,conversation_attachments,team_channel"},
		{"external team spoken", external, teamRun, true, "conversation_channel,conversation_attachments,conversation_external_source,team_channel,spoken_rules"},
		{"non-external origin", &Conversation{ID: "c", Origin: &ConversationReference{Kind: ConversationReferenceAgentControl, ID: "agent-1"}}, agentRun, false, "conversation_channel,conversation_attachments"},
	}
	for _, tc := range cases {
		sections := conversationSystemSections(tc.conversation, tc.run, tc.spoken)
		if got := strings.Join(conversationSectionIDs(sections), ","); got != tc.want {
			t.Errorf("%s: ids = %s, want %s", tc.name, got, tc.want)
		}
		for _, section := range sections {
			if section.Title == "" || section.Text == "" {
				t.Errorf("%s: section %q has empty title or text", tc.name, section.ID)
			}
		}
	}
	sections := conversationSystemSections(external, teamRun, false)
	if sections[2].Text != ConversationExternalSourceRules || sections[3].Text != teamChannelRules {
		t.Fatalf("external/team texts not used: %+v", sections)
	}
}

func TestSystemSectionsHaveNoFormWords(t *testing.T) {
	external := &Conversation{ID: "c", Origin: &ConversationReference{Kind: ConversationReferenceExternalSource, ID: "s"}}
	teamRun := &AgentRun{ID: "run-1", Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-1"}}
	idPattern := regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
	seen := map[string]bool{}
	for _, section := range conversationSystemSections(external, teamRun, true) {
		if !idPattern.MatchString(section.ID) || seen[section.ID] {
			t.Errorf("section id %q is invalid or repeated", section.ID)
		}
		seen[section.ID] = true
		for _, kernelID := range modelstep.LoopKernelSectionOrder {
			if section.ID == kernelID {
				t.Errorf("section id %q collides with a kernel section", section.ID)
			}
		}
		for _, word := range []string{"output.summary", "runOutput", "proposed", "continuationCheckpoint", "Turn", "historyCompaction", "this turn"} {
			if strings.Contains(section.Text, word) {
				t.Errorf("section %q contains form word %q", section.ID, word)
			}
		}
	}
}

func TestSpokenRulesSectionKeepsAuthorityCaveat(t *testing.T) {
	var spoken *ModelStepSystemSection
	sections := conversationSystemSections(&Conversation{ID: "c"}, &AgentRun{ID: "run-1"}, true)
	for i := range sections {
		if sections[i].ID == conversationSectionSpoken {
			spoken = &sections[i]
		}
	}
	if spoken == nil {
		t.Fatalf("spoken_rules missing: %v", conversationSectionIDs(sections))
	}
	if !strings.HasPrefix(spoken.Text, spokenResponseStyleInstruction) {
		t.Fatalf("spoken_rules does not start with the spoken style: %q", spoken.Text)
	}
	if !strings.HasSuffix(spoken.Text, spokenResponseAuthorityInstruction) {
		t.Fatalf("spoken_rules lost the authority caveat: %q", spoken.Text)
	}
	if !strings.Contains(spoken.Text, "not authenticated approval") {
		t.Fatalf("spoken_rules must say spoken speech is not approval: %q", spoken.Text)
	}
	if !strings.Contains(spoken.Text, "call turn_final_answer with silent=true") {
		t.Fatalf("spoken_rules must name the silent final answer: %q", spoken.Text)
	}
	if strings.Contains(spoken.Text, "runOutput") {
		t.Fatalf("spoken_rules kept the old form wording: %q", spoken.Text)
	}
	for _, section := range conversationSystemSections(&Conversation{ID: "c"}, &AgentRun{ID: "run-1"}, false) {
		if section.ID == conversationSectionSpoken {
			t.Fatal("spoken_rules included for a written reply")
		}
	}
}
