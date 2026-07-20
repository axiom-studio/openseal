package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

type ParticipationDisposition string

const (
	ParticipationSpeak    ParticipationDisposition = "speak"
	ParticipationSilent   ParticipationDisposition = "silent"
	ParticipationDeferred ParticipationDisposition = "deferred"
)

type ParticipationReason string

const (
	ParticipationReasonDirectMention        ParticipationReason = "direct_mention"
	ParticipationReasonAnswersQuestion      ParticipationReason = "answers_open_question"
	ParticipationReasonNewInformation       ParticipationReason = "new_information"
	ParticipationReasonRoleRelevant         ParticipationReason = "role_relevant"
	ParticipationReasonEvidenceBacked       ParticipationReason = "evidence_backed"
	ParticipationReasonResolvesWork         ParticipationReason = "resolves_open_work"
	ParticipationReasonCoordinatesWork      ParticipationReason = "coordinates_work"
	ParticipationReasonSubstantiveObjection ParticipationReason = "substantive_objection"
	ParticipationReasonRequestedSilence     ParticipationReason = "requested_silence"
	ParticipationReasonNoNewInformation     ParticipationReason = "no_new_information"
	ParticipationReasonLowRelevance         ParticipationReason = "low_relevance"
	ParticipationReasonDuplicate            ParticipationReason = "duplicate"
	ParticipationReasonBackpressure         ParticipationReason = "backpressure"
	ParticipationReasonAcknowledgmentOnly   ParticipationReason = "acknowledgment_only"
	ParticipationReasonNotAddressed         ParticipationReason = "not_addressed"
)

type ParticipationSignals struct {
	DirectlyMentioned              bool `json:"directlyMentioned,omitempty"`
	TriggerTargetsOtherParticipant bool `json:"triggerTargetsOtherParticipant,omitempty"`
	AnswersOpenQuestion            bool `json:"answersOpenQuestion,omitempty"`
	HasNewInformation              bool `json:"hasNewInformation,omitempty"`
	RoleRelevant                   bool `json:"roleRelevant,omitempty"`
	HasEvidence                    bool `json:"hasEvidence,omitempty"`
	ResolvesOpenWork               bool `json:"resolvesOpenWork,omitempty"`
	CoordinatesWork                bool `json:"coordinatesWork,omitempty"`
	SubstantiveObjection           bool `json:"substantiveObjection,omitempty"`
}

// ParticipationProposal is the bounded, visible output of a participant's
// relevance check. It contains no chain-of-thought: only the proposed message,
// structured intent, audience, and independently auditable signals.
type ParticipationProposal struct {
	ID                string                    `json:"id"`
	RoundID           string                    `json:"roundId"`
	Participant       ConversationParticipant   `json:"participant"`
	SemanticRoles     []string                  `json:"semanticRoles,omitempty"`
	WantsToSpeak      bool                      `json:"wantsToSpeak"`
	Intent            ConversationMessageIntent `json:"intent,omitempty"`
	Content           string                    `json:"content,omitempty"`
	ContributionKey   string                    `json:"contributionKey,omitempty"`
	Audience          ConversationAudience      `json:"audience,omitempty"`
	Mentions          []ConversationParticipant `json:"mentions,omitempty"`
	References        []ConversationReference   `json:"references,omitempty"`
	ReplyToMessageID  string                    `json:"replyToMessageId,omitempty"`
	RequiresResponse  bool                      `json:"requiresResponse,omitempty"`
	ResolvesMessageID string                    `json:"resolvesMessageId,omitempty"`
	Signals           ParticipationSignals      `json:"signals"`
	Priority          int                       `json:"priority,omitempty"`
	// ProposedAction is an optional, already-authorized capability request made
	// by this participant. It remains part of the visible participation record;
	// arbitration selects at most one speaker action and the Conversation Run
	// materializes it through the ordinary ActionCoordinator.
	ProposedAction *TurnAction            `json:"proposedAction,omitempty"`
	ActionInputs   map[string]interface{} `json:"actionInputs,omitempty"`
}

func (p ParticipationProposal) Validate() error {
	if !validOpaqueIdentifier(p.ID, 128) || !validOpaqueIdentifier(p.RoundID, 128) {
		return errors.New("participation proposal and round ids are required")
	}
	if err := p.Participant.Validate(); err != nil {
		return err
	}
	if p.Priority < -100 || p.Priority > 100 {
		return errors.New("participation proposal priority must be between -100 and 100")
	}
	if !p.WantsToSpeak {
		if strings.TrimSpace(p.Content) != "" || p.ContributionKey != "" || p.Intent != "" || len(p.Mentions) > 0 || len(p.References) > 0 ||
			p.ReplyToMessageID != "" || p.RequiresResponse || p.ResolvesMessageID != "" || p.ProposedAction != nil || len(p.ActionInputs) > 0 {
			return errors.New("silent participation proposal cannot include message output")
		}
		return nil
	}
	if !validConversationMessageIntent(p.Intent) || p.Intent == MessageIntentSystem || strings.TrimSpace(p.Content) == "" || len(p.Content) > 65536 {
		return errors.New("speaking participation proposal requires a valid non-system intent and content")
	}
	if !validConversationContributionKey(p.ContributionKey) {
		return errors.New("participation proposal contribution key must be normalized lowercase metadata")
	}
	if err := p.Audience.Validate(); err != nil {
		return err
	}
	if p.ReplyToMessageID != "" && !validOpaqueIdentifier(p.ReplyToMessageID, 128) {
		return errors.New("participation reply id must be portable")
	}
	if p.ResolvesMessageID != "" && !validOpaqueIdentifier(p.ResolvesMessageID, 128) {
		return errors.New("resolved message id must be portable")
	}
	for _, mention := range p.Mentions {
		if err := mention.Validate(); err != nil {
			return err
		}
	}
	for _, reference := range p.References {
		if err := reference.Validate(); err != nil {
			return err
		}
	}
	if p.ProposedAction == nil {
		if len(p.ActionInputs) > 0 {
			return errors.New("participation action inputs require a proposed action")
		}
		return nil
	}
	if p.ProposedAction.PreparedRuntime != nil || strings.TrimSpace(p.ProposedAction.Capability) == "" || strings.TrimSpace(p.ProposedAction.Summary) == "" ||
		strings.TrimSpace(p.ProposedAction.InputRef) == "" || len(p.ActionInputs) == 0 || p.ResolvesMessageID != "" {
		return errors.New("participation action proposal is invalid")
	}
	if _, err := resolveTurnActionInput(p.ActionInputs, p.ProposedAction.InputRef); err != nil {
		return fmt.Errorf("participation action input: %w", err)
	}
	return nil
}

type ParticipationRoundStatus string

const (
	ParticipationRoundCommitted ParticipationRoundStatus = "committed"
)

// ParticipationRound preserves every proposal and deterministic decision,
// including silence and deferral, without exposing hidden model reasoning.
type ParticipationRound struct {
	ID                   string                        `json:"id"`
	Scope                Scope                         `json:"scope"`
	ConversationID       string                        `json:"conversationId"`
	ConversationRevision int64                         `json:"conversationRevision,omitempty"`
	TriggerMessageID     string                        `json:"triggerMessageId,omitempty"`
	Status               ParticipationRoundStatus      `json:"status"`
	Policy               ConversationArbitrationPolicy `json:"policy"`
	Proposals            []ParticipationProposal       `json:"proposals"`
	Arbitration          ConversationArbitration       `json:"arbitration"`
	IdempotencyKey       string                        `json:"idempotencyKey"`
	Revision             int64                         `json:"revision"`
	CreatedAt            time.Time                     `json:"createdAt"`
	CommittedAt          time.Time                     `json:"committedAt"`
}

func (r *ParticipationRound) Validate() error {
	if r == nil || !validOpaqueIdentifier(r.ID, 128) || !validOpaqueIdentifier(r.ConversationID, 128) || r.ConversationRevision < 0 ||
		r.Status != ParticipationRoundCommitted || r.Revision <= 0 || r.CreatedAt.IsZero() || r.CommittedAt.IsZero() ||
		strings.TrimSpace(r.IdempotencyKey) == "" || len(r.IdempotencyKey) > 256 {
		return errors.New("invalid participation round")
	}
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if r.TriggerMessageID != "" && !validOpaqueIdentifier(r.TriggerMessageID, 128) {
		return errors.New("participation trigger message id must be portable")
	}
	if r.Arbitration.RoundID != r.ID || len(r.Proposals) == 0 {
		return errors.New("participation round arbitration and proposals are required")
	}
	for _, proposal := range r.Proposals {
		if err := proposal.Validate(); err != nil || proposal.RoundID != r.ID {
			if err != nil {
				return err
			}
			return errors.New("participation proposal belongs to another round")
		}
	}
	return nil
}

type ConversationArbitrationPolicy struct {
	MinimumScore       int     `json:"minimumScore"`
	MaximumSpeakers    int     `json:"maximumSpeakers"`
	DuplicateThreshold float64 `json:"duplicateThreshold"`
}

func DefaultConversationArbitrationPolicy() ConversationArbitrationPolicy {
	return ConversationArbitrationPolicy{MinimumScore: 30, MaximumSpeakers: 3, DuplicateThreshold: 0.72}
}

func (p ConversationArbitrationPolicy) normalize() (ConversationArbitrationPolicy, error) {
	if p.MinimumScore == 0 && p.MaximumSpeakers == 0 && p.DuplicateThreshold == 0 {
		return DefaultConversationArbitrationPolicy(), nil
	}
	if p.MinimumScore < 1 || p.MinimumScore > 100 || p.MaximumSpeakers < 1 || p.MaximumSpeakers > 20 ||
		p.DuplicateThreshold < 0.5 || p.DuplicateThreshold > 1 {
		return ConversationArbitrationPolicy{}, errors.New("invalid conversation arbitration policy")
	}
	return p, nil
}

type ParticipationDecision struct {
	ProposalID    string                   `json:"proposalId"`
	Participant   ConversationParticipant  `json:"participant"`
	Disposition   ParticipationDisposition `json:"disposition"`
	Score         int                      `json:"score"`
	Rank          int                      `json:"rank,omitempty"`
	Reasons       []ParticipationReason    `json:"reasons"`
	DuplicateOfID string                   `json:"duplicateOfId,omitempty"`
	Fingerprint   string                   `json:"fingerprint,omitempty"`
}

type ConversationArbitration struct {
	RoundID   string                  `json:"roundId"`
	Decisions []ParticipationDecision `json:"decisions"`
	Speakers  []string                `json:"speakerProposalIds,omitempty"`
}

// ArbitrateParticipation deterministically chooses who speaks before model
// output is committed. Agents with new role-relevant information may all
// participate; quiet, repetitive, low-relevance, and pile-on messages do not.
func ArbitrateParticipation(roundID string, proposals []ParticipationProposal, recent []*ChannelMessage, policy ConversationArbitrationPolicy) (*ConversationArbitration, error) {
	if !validOpaqueIdentifier(roundID, 128) || len(proposals) == 0 {
		return nil, errors.New("participation round and proposals are required")
	}
	normalizedPolicy, err := policy.normalize()
	if err != nil {
		return nil, err
	}
	seenProposals := make(map[string]struct{}, len(proposals))
	seenParticipants := make(map[ConversationParticipant]struct{}, len(proposals))
	decisions := make([]ParticipationDecision, 0, len(proposals))
	proposalByID := make(map[string]ParticipationProposal, len(proposals))
	for _, proposal := range proposals {
		if err := proposal.Validate(); err != nil {
			return nil, err
		}
		if proposal.RoundID != roundID {
			return nil, errors.New("participation proposal belongs to another round")
		}
		if _, exists := seenProposals[proposal.ID]; exists {
			return nil, errors.New("duplicate participation proposal id")
		}
		if _, exists := seenParticipants[proposal.Participant]; exists {
			return nil, errors.New("participant submitted more than one proposal in a round")
		}
		seenProposals[proposal.ID] = struct{}{}
		seenParticipants[proposal.Participant] = struct{}{}
		proposalByID[proposal.ID] = proposal
		decision := scoreParticipationProposal(proposal)
		decisions = append(decisions, decision)
	}

	sort.SliceStable(decisions, func(i, j int) bool {
		if decisions[i].Disposition != decisions[j].Disposition {
			return decisions[i].Disposition != ParticipationSilent
		}
		if decisions[i].Score != decisions[j].Score {
			return decisions[i].Score > decisions[j].Score
		}
		left, right := proposalByID[decisions[i].ProposalID], proposalByID[decisions[j].ProposalID]
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		return string(decisions[i].Participant.Type)+":"+decisions[i].Participant.ID < string(decisions[j].Participant.Type)+":"+decisions[j].Participant.ID
	})

	recentComparable := make([]comparableMessage, 0, len(recent)+normalizedPolicy.MaximumSpeakers)
	for _, message := range recent {
		if message == nil || strings.TrimSpace(message.Content) == "" {
			continue
		}
		recentComparable = append(recentComparable, comparableMessage{id: message.ID, content: message.Content, key: message.ContributionKey})
	}
	speakers := make([]string, 0, normalizedPolicy.MaximumSpeakers)
	for index := range decisions {
		decision := &decisions[index]
		proposal := proposalByID[decision.ProposalID]
		if decision.Disposition == ParticipationSilent {
			continue
		}
		if decision.Score < normalizedPolicy.MinimumScore {
			decision.Disposition = ParticipationSilent
			decision.Reasons = appendReason(decision.Reasons, ParticipationReasonLowRelevance)
			continue
		}
		if duplicateID := findDuplicateConversationMessage(proposal.Content, proposal.ContributionKey, recentComparable, normalizedPolicy.DuplicateThreshold); duplicateID != "" {
			decision.Disposition = ParticipationSilent
			decision.DuplicateOfID = duplicateID
			decision.Reasons = appendReason(decision.Reasons, ParticipationReasonDuplicate)
			continue
		}
		if len(speakers) >= normalizedPolicy.MaximumSpeakers {
			decision.Disposition = ParticipationDeferred
			decision.Reasons = appendReason(decision.Reasons, ParticipationReasonBackpressure)
			continue
		}
		decision.Disposition = ParticipationSpeak
		decision.Rank = len(speakers) + 1
		decision.Fingerprint = ConversationMessageFingerprint(proposal.Content)
		speakers = append(speakers, proposal.ID)
		recentComparable = append(recentComparable, comparableMessage{id: proposal.ID, content: proposal.Content, key: proposal.ContributionKey})
	}
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].ProposalID < decisions[j].ProposalID })
	return &ConversationArbitration{RoundID: roundID, Decisions: decisions, Speakers: speakers}, nil
}

func scoreParticipationProposal(proposal ParticipationProposal) ParticipationDecision {
	decision := ParticipationDecision{ProposalID: proposal.ID, Participant: proposal.Participant, Disposition: ParticipationSpeak}
	if !proposal.WantsToSpeak {
		decision.Disposition = ParticipationSilent
		decision.Reasons = []ParticipationReason{ParticipationReasonRequestedSilence}
		return decision
	}
	if proposal.Signals.TriggerTargetsOtherParticipant && !proposal.Signals.DirectlyMentioned &&
		!proposalIsSubstantiveObjection(proposal) && !proposalHasVerifiedEvidence(proposal) && !proposalCoordinatesWork(proposal) {
		decision.Disposition = ParticipationSilent
		decision.Reasons = []ParticipationReason{ParticipationReasonNotAddressed}
		return decision
	}
	add := func(enabled bool, points int, reason ParticipationReason) {
		if enabled {
			decision.Score += points
			decision.Reasons = append(decision.Reasons, reason)
		}
	}
	add(proposal.Signals.DirectlyMentioned, 70, ParticipationReasonDirectMention)
	add(proposal.Signals.AnswersOpenQuestion, 35, ParticipationReasonAnswersQuestion)
	add(proposal.Signals.HasNewInformation, 25, ParticipationReasonNewInformation)
	add(proposal.Signals.RoleRelevant, 20, ParticipationReasonRoleRelevant)
	add(proposalHasVerifiedEvidence(proposal), 15, ParticipationReasonEvidenceBacked)
	add(proposal.Signals.ResolvesOpenWork, 20, ParticipationReasonResolvesWork)
	add(proposalCoordinatesWork(proposal), 10, ParticipationReasonCoordinatesWork)
	add(proposalIsSubstantiveObjection(proposal), 30, ParticipationReasonSubstantiveObjection)
	decision.Score += proposal.Priority / 5
	if proposal.Intent == MessageIntentAcknowledgment && !proposal.Signals.DirectlyMentioned {
		decision.Score -= 45
		decision.Reasons = append(decision.Reasons, ParticipationReasonAcknowledgmentOnly)
	}
	if !proposal.Signals.HasNewInformation && !proposal.Signals.AnswersOpenQuestion && !proposal.Signals.ResolvesOpenWork &&
		!proposalIsSubstantiveObjection(proposal) && !proposal.Signals.DirectlyMentioned && !proposalCoordinatesWork(proposal) {
		decision.Score -= 25
		decision.Reasons = append(decision.Reasons, ParticipationReasonNoNewInformation)
	}
	if decision.Score < 0 {
		decision.Score = 0
	}
	if decision.Score > 100 {
		decision.Score = 100
	}
	return decision
}

func proposalHasVerifiedEvidence(proposal ParticipationProposal) bool {
	return proposal.Signals.HasEvidence && len(proposal.References) > 0
}

func proposalIsSubstantiveObjection(proposal ParticipationProposal) bool {
	return proposal.Signals.SubstantiveObjection && proposal.Intent == MessageIntentObjection
}

func proposalCoordinatesWork(proposal ParticipationProposal) bool {
	if !proposal.Signals.CoordinatesWork {
		return false
	}
	switch proposal.Intent {
	case MessageIntentUpdate, MessageIntentProposal, MessageIntentDecision, MessageIntentHandoff, MessageIntentApprovalRequest:
		return true
	default:
		return false
	}
}

type comparableMessage struct {
	id      string
	content string
	key     string
}

func findDuplicateConversationMessage(content, key string, messages []comparableMessage, threshold float64) string {
	for _, message := range messages {
		if key != "" && message.key != "" && key == message.key {
			return message.id
		}
		if conversationMessageSimilarity(content, message.content) >= threshold {
			return message.id
		}
	}
	return ""
}

func ConversationMessageFingerprint(content string) string {
	normalized := strings.Join(conversationTokens(content), " ")
	digest := sha256.Sum256([]byte(normalized))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func conversationMessageSimilarity(left, right string) float64 {
	leftTokens := conversationTokenSet(left)
	rightTokens := conversationTokenSet(right)
	if len(leftTokens) == 0 || len(rightTokens) == 0 {
		if len(leftTokens) == len(rightTokens) {
			return 1
		}
		return 0
	}
	intersection := 0
	for token := range leftTokens {
		if _, exists := rightTokens[token]; exists {
			intersection++
		}
	}
	union := len(leftTokens) + len(rightTokens) - intersection
	return float64(intersection) / float64(union)
}

func conversationTokenSet(content string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, token := range conversationTokens(content) {
		result[token] = struct{}{}
	}
	return result
}

func conversationTokens(content string) []string {
	return strings.FieldsFunc(strings.ToLower(content), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func appendReason(reasons []ParticipationReason, reason ParticipationReason) []ParticipationReason {
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}
