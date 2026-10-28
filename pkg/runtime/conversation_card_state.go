package runtime

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"time"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

const (
	// maximumCardStateCards bounds the newest card references inspected for
	// a turn's card state.
	maximumCardStateCards = 8
	// maximumCardStateLines bounds the card state note.
	maximumCardStateLines = 6
)

// conversationCardState summarises interactions that do not wake the Agent
// (checklist, step and recipe ticks, flashcard marks, poll votes) on cards in
// messages,
// for the Agent's next turn: one line per card component whose state changed
// since the Agent's latest message in messages, e.g. "Packing list: 9/14
// ticked (unticked: passport, charger)". Ticks are the latest write per item
// across everyone the viewer can see; flashcard marks are each person's own,
// one line per person who marked since then, e.g. `Flashcards "French food
// words": 3 got, 1 again, 1 unseen`, named when more than one did. A poll
// with a vote since then gets its tally from everyone's latest vote, e.g.
// `Poll "Dinner": Thai 3, Pizza 1 (4 of 5 voted)`. Cards are
// read newest first and the note is at most maximumCardStateLines lines. It
// is best effort: a card that cannot be read is left out rather than failing
// the turn.
func (r *ConversationRunTurnRunner) conversationCardState(ctx context.Context, conversation *Conversation, viewer ConversationViewer, participantID string, messages []*ChannelMessage) []string {
	if r == nil || r.conversations == nil || r.artifacts == nil || r.config.AttachmentContent == nil || conversation == nil {
		return nil
	}
	ordered := make([]*ChannelMessage, 0, len(messages))
	for _, message := range messages {
		if message != nil {
			ordered = append(ordered, message)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence > ordered[j].Sequence })
	var since time.Time
	for _, message := range ordered {
		if message.Sender.Type == ConversationParticipantAgent && message.Sender.ID == participantID {
			since = message.CreatedAt
			break
		}
	}
	names := map[ConversationParticipant]string{}
	for _, message := range ordered {
		if _, known := names[message.Sender]; !known && message.SenderDisplayName != "" {
			names[message.Sender] = message.SenderDisplayName
		}
	}
	var lines []string
	seen := map[string]bool{}
	for _, message := range ordered {
		for _, reference := range message.References {
			if reference.Kind != ConversationReferenceArtifact || reference.Version < 1 || seen[reference.ID] {
				continue
			}
			if len(seen) == maximumCardStateCards || len(lines) == maximumCardStateLines {
				return lines
			}
			seen[reference.ID] = true
			for _, line := range r.cardStateLines(ctx, conversation, viewer, reference, since, names) {
				if len(lines) == maximumCardStateLines {
					return lines
				}
				lines = append(lines, line)
			}
		}
	}
	return lines
}

// cardStateLines returns the state lines of one card whose non-waking
// interactions changed at or after since. names maps participants to display
// names.
func (r *ConversationRunTurnRunner) cardStateLines(ctx context.Context, conversation *Conversation, viewer ConversationViewer, reference ConversationReference, since time.Time, names map[ConversationParticipant]string) []string {
	latest, err := r.conversations.ListLatestSurfaceInteractions(ctx, LatestSurfaceInteractionFilter{
		Scope: conversation.Scope, ConversationID: conversation.ID, SurfaceID: reference.ID, AcrossActors: true, Viewer: &viewer,
	})
	if err != nil {
		return nil
	}
	changed := map[string]bool{}
	for _, interaction := range latest {
		quiet := interaction.ItemKey != "" || interaction.ActionID == presentation.PollVoteAction
		if interaction.Repeatable && quiet && !interaction.CreatedAt.Before(since) {
			changed[interaction.ComponentID] = true
		}
	}
	if len(changed) == 0 {
		return nil
	}
	surface := r.loadCardSurface(ctx, conversation.Scope, reference)
	if surface == nil {
		return nil
	}
	var lines []string
	var perActor []*SurfaceInteraction
	perActorRead := false
	// readPerActor reads each person's latest interaction per item, once.
	readPerActor := func() []*SurfaceInteraction {
		if !perActorRead {
			perActorRead = true
			perActor, _ = r.conversations.ListLatestSurfaceInteractions(ctx, LatestSurfaceInteractionFilter{
				Scope: conversation.Scope, ConversationID: conversation.ID, SurfaceID: reference.ID, Viewer: &viewer,
			})
		}
		return perActor
	}
	eachCardComponent(surface.Components, func(component presentation.Component) {
		if !changed[component.ID] {
			return
		}
		switch component.Type {
		case "flashcards":
			lines = append(lines, flashcardStateLines(component, readPerActor(), since, names)...)
			return
		case "poll":
			if line, ok := r.pollStateLine(ctx, conversation, component, readPerActor()); ok {
				lines = append(lines, line)
			}
			return
		}
		rule, err := presentation.Interaction(component, "")
		if err != nil || !rule.ItemKeyed || rule.NotifyAgent {
			return
		}
		checked := map[string]bool{}
		for _, interaction := range latest {
			if interaction.ComponentID != component.ID || interaction.ItemKey == "" {
				continue
			}
			var values struct {
				Checked bool `json:"checked"`
			}
			if json.Unmarshal(interaction.Values, &values) == nil {
				checked[interaction.ItemKey] = values.Checked
			}
		}
		if line, ok := presentation.CardStateLine(component, checked); ok {
			lines = append(lines, line)
		}
	})
	return lines
}

// flashcardStateLines returns one line per person whose flashcard marks on
// component changed at or after since, from each person's latest mark per
// card. Lines name the person when more than one marked.
func flashcardStateLines(component presentation.Component, latest []*SurfaceInteraction, since time.Time, names map[ConversationParticipant]string) []string {
	marks := map[ConversationParticipant]map[string]string{}
	var changed []ConversationParticipant
	for _, interaction := range latest {
		if interaction.ComponentID != component.ID || interaction.ActionID != presentation.FlashcardsMark || interaction.ItemKey == "" {
			continue
		}
		actor := ConversationParticipant{Type: interaction.ActorType, ID: interaction.ActorID}
		if marks[actor] == nil {
			marks[actor] = map[string]string{}
		}
		if result, ok := presentation.FlashcardMark(interaction.Values); ok {
			marks[actor][interaction.ItemKey] = result
		}
		if !interaction.CreatedAt.Before(since) && !containsParticipant(changed, actor) {
			changed = append(changed, actor)
		}
	}
	sort.Slice(changed, func(i, j int) bool {
		if changed[i].Type != changed[j].Type {
			return changed[i].Type < changed[j].Type
		}
		return changed[i].ID < changed[j].ID
	})
	var lines []string
	for _, actor := range changed {
		line, ok := presentation.FlashcardsStateLine(component, marks[actor])
		if !ok {
			continue
		}
		if len(changed) > 1 {
			name := names[actor]
			if name == "" {
				name = "Someone"
			}
			line = name + ": " + line
		}
		lines = append(lines, line)
	}
	return lines
}

// pollStateLine tallies everyone's latest vote on a poll component.
func (r *ConversationRunTurnRunner) pollStateLine(ctx context.Context, conversation *Conversation, component presentation.Component, latest []*SurfaceInteraction) (string, bool) {
	votes := PollVotes(component.ID, latest)
	var participants []string
	if r.config.HumanParticipants != nil {
		if people, err := r.config.HumanParticipants(ctx, conversation); err == nil {
			for _, person := range people {
				participants = append(participants, PollVoterKey(person))
			}
		}
	}
	tally, err := presentation.PollOutcome(component, votes, participants)
	if err != nil {
		return "", false
	}
	return presentation.PollStateLine(component, tally)
}

// PollVoterKey is the key PollVotes gives a person's vote.
func PollVoterKey(participant ConversationParticipant) string {
	return string(participant.Type) + ":" + participant.ID
}

// PollVotes collects each person's latest vote on one poll component, keyed
// by PollVoterKey, from ListLatestSurfaceInteractions without AcrossActors.
// Hosts pass it, with the human participants' keys, to
// presentation.PollOutcome and presentation.EveryoneVoted.
func PollVotes(componentID string, latest []*SurfaceInteraction) map[string][]string {
	votes := map[string][]string{}
	for _, interaction := range latest {
		if interaction == nil || interaction.ComponentID != componentID || interaction.ActionID != presentation.PollVoteAction {
			continue
		}
		if ids, ok := presentation.PollVote(interaction.Values); ok {
			votes[PollVoterKey(ConversationParticipant{Type: interaction.ActorType, ID: interaction.ActorID})] = ids
		}
	}
	return votes
}

func containsParticipant(list []ConversationParticipant, participant ConversationParticipant) bool {
	for _, candidate := range list {
		if candidate == participant {
			return true
		}
	}
	return false
}

func (r *ConversationRunTurnRunner) loadCardSurface(ctx context.Context, scope Scope, reference ConversationReference) *presentation.Surface {
	artifact, err := r.artifacts.GetArtifact(ctx, scope, reference.ID, reference.Version)
	if err != nil || artifact == nil || artifact.Type != presentation.ArtifactType {
		return nil
	}
	reader, err := r.config.AttachmentContent.Open(ctx, artifact.Scope, artifact.ContentRef)
	if err != nil {
		return nil
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, presentation.MaxSurfaceBytes+1))
	if err != nil {
		return nil
	}
	surface, err := presentation.Decode(raw)
	if err != nil {
		return nil
	}
	return surface
}

// eachCardComponent visits components depth first, including tab children.
func eachCardComponent(components []presentation.Component, visit func(presentation.Component)) {
	for _, component := range components {
		visit(component)
		eachCardComponent(component.Children, visit)
		for _, tab := range component.Tabs {
			eachCardComponent(tab.Children, visit)
		}
	}
}
