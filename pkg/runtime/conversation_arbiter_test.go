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

func TestConversationArbiterAppliesExplicitParticipationPolicy(t *testing.T) {
	t.Parallel()
	roundID := "round-explicit-participation"
	channel := ConversationAudience{Kind: ConversationAudienceChannel}
	proposals := []ParticipationProposal{
		{
			ID: "acknowledgment", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "observer"},
			WantsToSpeak: true, Intent: MessageIntentAcknowledgment, Content: "Thanks, I have seen the update.", Audience: channel,
		},
		{
			ID: "first", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "first"},
			WantsToSpeak: true, Intent: MessageIntentUpdate, Content: "The release is ready for review.", Audience: channel,
			Signals: ParticipationSignals{HasNewInformation: true},
		},
		{
			ID: "second", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "second"},
			WantsToSpeak: true, Intent: MessageIntentUpdate, Content: "The release is ready for review.", Audience: channel,
			Signals: ParticipationSignals{HasNewInformation: true},
		},
	}
	policy := DefaultConversationArbitrationPolicy()
	policy.Participation = &ConversationParticipationPolicy{
		QuietByDefault:           false,
		RequireRoleRelevance:     false,
		SuppressDuplicateContent: false,
	}
	result, err := ArbitrateParticipation(roundID, proposals, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Speakers, []string{"first", "second", "acknowledgment"}) {
		t.Fatalf("explicit open participation speakers = %#v", result.Speakers)
	}
}

func TestConversationArbiterRequiresRoleRelevanceUnlessPolicyOptsOut(t *testing.T) {
	t.Parallel()
	roundID := "round-role-policy"
	proposal := ParticipationProposal{
		ID: "specialist", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "specialist"},
		WantsToSpeak: true, Intent: MessageIntentUpdate, Content: "I found a new affected workload.", Priority: 25,
		Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		Signals:  ParticipationSignals{HasNewInformation: true},
	}
	required, err := ArbitrateParticipation(roundID, []ParticipationProposal{proposal}, nil, DefaultConversationArbitrationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	requiredDecision := decisionsByProposal(required.Decisions)["specialist"]
	if requiredDecision.Disposition != ParticipationSilent ||
		!containsParticipationReason(requiredDecision.Reasons, ParticipationReasonRoleNotRelevant) {
		t.Fatalf("role-irrelevant proposal was not suppressed: %#v", requiredDecision)
	}

	preferredPolicy := DefaultConversationArbitrationPolicy()
	preferred := *preferredPolicy.Participation
	preferred.RequireRoleRelevance = false
	preferredPolicy.Participation = &preferred
	allowed, err := ArbitrateParticipation(roundID, []ParticipationProposal{proposal}, nil, preferredPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(allowed.Speakers, []string{"specialist"}) {
		t.Fatalf("role-relevance opt-out speakers = %#v", allowed.Speakers)
	}
}

func TestConversationArbiterDefaultsOmittedParticipationPolicyToSafeBehavior(t *testing.T) {
	t.Parallel()
	roundID := "round-omitted-participation"
	channel := ConversationAudience{Kind: ConversationAudienceChannel}
	recent := []*ChannelMessage{{
		ID: "existing", Scope: Scope{Kind: "tenant", ID: "one"}, ConversationID: "conversation-1", Sequence: 1,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "first"}, Intent: MessageIntentUpdate,
		Content: "The production rollout completed successfully.", Audience: channel, CreatedAt: time.Now(),
	}}
	proposals := []ParticipationProposal{
		{
			ID: "duplicate", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "second"},
			WantsToSpeak: true, Intent: MessageIntentUpdate, Content: "The production rollout completed successfully.", Audience: channel,
			Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true},
		},
		{
			ID: "acknowledgment", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "third"},
			WantsToSpeak: true, Intent: MessageIntentAcknowledgment, Content: "Thanks, everyone.", Audience: channel,
			Signals: ParticipationSignals{RoleRelevant: true},
		},
	}
	policy := ConversationArbitrationPolicy{
		MinimumScore: 30, MaximumSpeakers: 3, DuplicateThreshold: 0.72, MinimumAvailableParticipants: 1,
	}
	result, err := ArbitrateParticipation(roundID, proposals, recent, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Speakers) != 0 {
		t.Fatalf("omitted participation policy did not use safe defaults: %#v", result.Speakers)
	}
	decisions := decisionsByProposal(result.Decisions)
	if decisions["duplicate"].DuplicateOfID != "existing" ||
		!containsParticipationReason(decisions["acknowledgment"].Reasons, ParticipationReasonQuietByDefault) {
		t.Fatalf("safe default decisions = %#v", decisions)
	}
}

func TestConversationArbiterSuppressesSemanticClaimPileOn(t *testing.T) {
	t.Parallel()
	roundID := "round-semantic-duplicate"
	channel := ConversationAudience{Kind: ConversationAudienceChannel}
	proposals := []ParticipationProposal{
		{
			ID: "accountant", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "accountant"},
			WantsToSpeak: true, Intent: MessageIntentProposal, Content: "I propose Agent 37 as rollout owner because they coordinate the whole team.", ContributionKey: "rollout-owner:agent-37", Audience: channel,
			Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true, CoordinatesWork: true},
		},
		{
			ID: "sales", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "sales"},
			WantsToSpeak: true, Intent: MessageIntentProposal, Content: "The team leader should own release execution due to cross-functional visibility.", ContributionKey: "rollout-owner:agent-37", Audience: channel,
			Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true, CoordinatesWork: true},
		},
		{
			ID: "support", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "support"},
			WantsToSpeak: true, Intent: MessageIntentProposal, Content: "Customer support should own the rollout so incidents have one accountable responder.", ContributionKey: "rollout-owner:support", Audience: channel,
			Signals: ParticipationSignals{HasNewInformation: true, RoleRelevant: true, CoordinatesWork: true},
		},
	}
	result, err := ArbitrateParticipation(roundID, proposals, nil, DefaultConversationArbitrationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Speakers, []string{"accountant", "support"}) {
		t.Fatalf("speakers = %#v", result.Speakers)
	}
	decisions := decisionsByProposal(result.Decisions)
	if decisions["sales"].Disposition != ParticipationSilent || decisions["sales"].DuplicateOfID != "accountant" ||
		!containsParticipationReason(decisions["sales"].Reasons, ParticipationReasonDuplicate) {
		t.Fatalf("decisions = %#v", decisions)
	}
}

func TestConversationArbiterKeepsGovernedActionDistinctFromTriggerAndDeduplicatesExactActionPileOn(t *testing.T) {
	t.Parallel()
	roundID := "round-governed-action"
	channel := ConversationAudience{Kind: ConversationAudienceChannel}
	recent := []*ChannelMessage{{
		ID: "request-1", Scope: Scope{Kind: "tenant", ID: "one"}, ConversationID: "conversation-1", Sequence: 1,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion,
		Content: "Update the Coordinator role purpose to publish concise progress updates.", Audience: channel, CreatedAt: time.Now(),
	}}
	action := &TurnAction{
		Type: "skill_action", Capability: "openseal.teams.update_role",
		Summary: "Update the Coordinator role purpose", IdempotencyKey: "coordinator-role-revision-3", InputRef: "/actionInputs/call",
	}
	proposals := []ParticipationProposal{
		{
			ID: "coordinator", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "coordinator"},
			WantsToSpeak: true, Intent: MessageIntentProposal,
			Content: "Proposing to update the Coordinator role purpose to publish concise progress updates.", Audience: channel,
			Signals:        ParticipationSignals{DirectlyMentioned: true, AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true, CoordinatesWork: true},
			ProposedAction: action, ActionInputs: map[string]interface{}{"actionInputs": map[string]interface{}{"call": map[string]interface{}{"roleId": "coordinator"}}},
		},
		{
			ID: "reviewer", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "reviewer"},
			WantsToSpeak: true, Intent: MessageIntentProposal,
			Content: "I also propose updating the Coordinator purpose to publish concise progress updates.", Audience: channel,
			Signals:        ParticipationSignals{HasNewInformation: true, RoleRelevant: true, CoordinatesWork: true},
			ProposedAction: action, ActionInputs: map[string]interface{}{"actionInputs": map[string]interface{}{"call": map[string]interface{}{"roleId": "coordinator"}}},
		},
	}
	result, err := ArbitrateParticipation(roundID, proposals, recent, DefaultConversationArbitrationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Speakers, []string{"coordinator"}) {
		t.Fatalf("governed action speakers = %#v", result.Speakers)
	}
	decisions := decisionsByProposal(result.Decisions)
	if decisions["coordinator"].Disposition != ParticipationSpeak || decisions["coordinator"].DuplicateOfID != "" {
		t.Fatalf("action was suppressed as trigger narration: %#v", decisions["coordinator"])
	}
	if decisions["reviewer"].Disposition != ParticipationSilent || decisions["reviewer"].DuplicateOfID != "coordinator" ||
		!containsParticipationReason(decisions["reviewer"].Reasons, ParticipationReasonDuplicate) {
		t.Fatalf("exact action pile-on was not suppressed: %#v", decisions["reviewer"])
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
