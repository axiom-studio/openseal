package runtime

import (
	"reflect"
	"testing"
	"time"
)

func TestConversationArbiterAllowsRelevantPeersWithoutLeaderMonopoly(t *testing.T) {
	t.Parallel()
	roundID := "round-1"
	channel := ConversationAudience{Kind: ConversationAudienceChannel}
	proposals := []ParticipationProposal{
		{
			ID: "leader", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "lead"},
			SemanticRoles: []string{"leader"}, WantsToSpeak: true, Intent: MessageIntentUpdate,
			Content: "I will coordinate the release handoff and keep the decision log current.", Audience: channel,
			Signals: ParticipationSignals{RoleRelevant: true, CoordinatesWork: true},
		},
		{
			ID: "developer", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "developer"},
			SemanticRoles: []string{"developer"}, WantsToSpeak: true, Intent: MessageIntentAnswer,
			Content: "The deployment completed; run 42 passed smoke tests and is ready for launch.", Audience: channel,
			Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true, HasEvidence: true},
		},
		{
			ID: "marketing", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "marketing"},
			SemanticRoles: []string{"marketing"}, WantsToSpeak: true, Intent: MessageIntentUpdate,
			Content: "Run 42 passed smoke tests, the deployment completed, and it is ready for launch.", Audience: channel,
			Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true},
		},
	}
	result, err := ArbitrateParticipation(roundID, proposals, nil, DefaultConversationArbitrationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Speakers, []string{"developer", "leader"}) {
		t.Fatalf("speakers = %#v", result.Speakers)
	}
	decisions := decisionsByProposal(result.Decisions)
	if decisions["developer"].Rank != 1 || decisions["leader"].Rank != 2 || decisions["marketing"].Disposition != ParticipationSilent ||
		decisions["marketing"].DuplicateOfID != "developer" {
		t.Fatalf("decisions = %#v", decisions)
	}
}

func TestConversationArbiterHonorsMentionsObjectionsSilenceAndBackpressure(t *testing.T) {
	t.Parallel()
	roundID := "round-2"
	channel := ConversationAudience{Kind: ConversationAudienceChannel}
	proposals := []ParticipationProposal{
		{ID: "silent", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "silent"}, WantsToSpeak: false},
		{
			ID: "mentioned", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "mentioned"},
			WantsToSpeak: true, Intent: MessageIntentAcknowledgment, Content: "I have it and will report back here.", Audience: channel,
			Signals: ParticipationSignals{DirectlyMentioned: true},
		},
		{
			ID: "objector", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "objector"},
			WantsToSpeak: true, Intent: MessageIntentObjection, Content: "The proposed rollout omits the required data migration rollback check.", Audience: channel,
			Signals: ParticipationSignals{SubstantiveObjection: true, HasEvidence: true, RoleRelevant: true},
		},
		{
			ID: "third", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "third"},
			WantsToSpeak: true, Intent: MessageIntentUpdate, Content: "I found a second affected tenant in the audit data.", Audience: channel,
			Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true, HasEvidence: true},
		},
	}
	policy := DefaultConversationArbitrationPolicy()
	policy.MaximumSpeakers = 2
	result, err := ArbitrateParticipation(roundID, proposals, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Speakers, []string{"mentioned", "objector"}) {
		t.Fatalf("speakers = %#v", result.Speakers)
	}
	decisions := decisionsByProposal(result.Decisions)
	if decisions["silent"].Disposition != ParticipationSilent || decisions["third"].Disposition != ParticipationDeferred ||
		!containsParticipationReason(decisions["third"].Reasons, ParticipationReasonBackpressure) {
		t.Fatalf("decisions = %#v", decisions)
	}
}

func TestConversationArbiterSuppressesUnaddressedPileOnButKeepsMaterialIntervention(t *testing.T) {
	t.Parallel()
	roundID := "round-targeted"
	channel := ConversationAudience{Kind: ConversationAudienceChannel}
	proposals := []ParticipationProposal{
		{
			ID: "accountant", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "accountant"},
			WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "Reconcile the bank, receivables, payables, payroll, and tax accounts.", Audience: channel,
			Signals: ParticipationSignals{DirectlyMentioned: true, AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true},
		},
		{
			ID: "support", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "support"},
			WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "Review bank, receivable, payable, payroll, and tax balances.", Audience: channel,
			Signals: ParticipationSignals{TriggerTargetsOtherParticipant: true, AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true, CoordinatesWork: true, HasEvidence: true},
		},
		{
			ID: "reviewer", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "reviewer"},
			WantsToSpeak: true, Intent: MessageIntentObjection, Content: "The checklist omits a legally required segregation-of-duties review.", Audience: channel,
			Signals: ParticipationSignals{TriggerTargetsOtherParticipant: true, SubstantiveObjection: true, HasEvidence: true, RoleRelevant: true},
		},
	}
	result, err := ArbitrateParticipation(roundID, proposals, nil, DefaultConversationArbitrationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	decisions := decisionsByProposal(result.Decisions)
	if decisions["accountant"].Disposition != ParticipationSpeak || decisions["reviewer"].Disposition != ParticipationSpeak ||
		decisions["support"].Disposition != ParticipationSilent || !containsParticipationReason(decisions["support"].Reasons, ParticipationReasonNotAddressed) {
		t.Fatalf("decisions = %#v", decisions)
	}
}

func TestConversationArbiterSuppressesRecentAndAcknowledgmentPileOn(t *testing.T) {
	t.Parallel()
	roundID := "round-3"
	channel := ConversationAudience{Kind: ConversationAudienceChannel}
	recent := []*ChannelMessage{{
		ID: "message-1", Scope: Scope{Kind: "tenant", ID: "one"}, ConversationID: "conversation-1", Sequence: 1,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "developer"}, Intent: MessageIntentUpdate,
		Content: "Production deployment is complete and smoke tests passed.", Audience: channel, CreatedAt: time.Now(),
	}}
	proposals := []ParticipationProposal{
		{
			ID: "duplicate", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "analyst"},
			WantsToSpeak: true, Intent: MessageIntentUpdate, Content: "Smoke tests passed and the production deployment is complete.", Audience: channel,
			Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true},
		},
		{
			ID: "thanks", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "marketing"},
			WantsToSpeak: true, Intent: MessageIntentAcknowledgment, Content: "Thanks, great work everyone.", Audience: channel,
			Signals: ParticipationSignals{RoleRelevant: true},
		},
	}
	result, err := ArbitrateParticipation(roundID, proposals, recent, DefaultConversationArbitrationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Speakers) != 0 {
		t.Fatalf("speakers = %#v", result.Speakers)
	}
	decisions := decisionsByProposal(result.Decisions)
	if decisions["duplicate"].DuplicateOfID != "message-1" || !containsParticipationReason(decisions["duplicate"].Reasons, ParticipationReasonDuplicate) ||
		!containsParticipationReason(decisions["thanks"].Reasons, ParticipationReasonAcknowledgmentOnly) {
		t.Fatalf("decisions = %#v", decisions)
	}
}

func TestConversationArbitrationIsInputOrderIndependent(t *testing.T) {
	t.Parallel()
	roundID := "round-deterministic"
	channel := ConversationAudience{Kind: ConversationAudienceChannel}
	a := ParticipationProposal{ID: "a", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "a"}, WantsToSpeak: true, Intent: MessageIntentUpdate, Content: "New finding A", Audience: channel, Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true}}
	b := ParticipationProposal{ID: "b", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "b"}, WantsToSpeak: true, Intent: MessageIntentUpdate, Content: "Different finding B", Audience: channel, Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true}}
	first, err := ArbitrateParticipation(roundID, []ParticipationProposal{a, b}, nil, DefaultConversationArbitrationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	second, err := ArbitrateParticipation(roundID, []ParticipationProposal{b, a}, nil, DefaultConversationArbitrationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("arbitration differs by input order:\n%#v\n%#v", first, second)
	}
	if ConversationMessageFingerprint("Hello, WORLD!") != ConversationMessageFingerprint("hello world") {
		t.Fatal("normalized fingerprints differ")
	}
}

func decisionsByProposal(decisions []ParticipationDecision) map[string]ParticipationDecision {
	result := make(map[string]ParticipationDecision, len(decisions))
	for _, decision := range decisions {
		result[decision.ProposalID] = decision
	}
	return result
}

func containsParticipationReason(reasons []ParticipationReason, wanted ParticipationReason) bool {
	for _, reason := range reasons {
		if reason == wanted {
			return true
		}
	}
	return false
}
