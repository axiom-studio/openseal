package cardinteraction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

// outcome returns the server outcome of an interaction: a quiz grade, the
// tally of a finished flashcards deck, the poll tally of the vote that
// completes a poll, an explorer share's recomputed outputs, or nil.
func (s *Service) outcome(ctx context.Context, req Request, component presentation.Component) (json.RawMessage, error) {
	switch {
	case component.Type == "poll":
		return s.pollVoteOutcome(ctx, req, component)
	case component.Type == "explorer":
		encoded, err := json.Marshal(req.Values)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidValues, err)
		}
		inputs, err := presentation.ExplorerShareInputs(encoded)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidValues, err)
		}
		// Outputs are always worked out here; the client's numbers are never
		// stored.
		shared, err := presentation.ExplorerShare(component, inputs)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidValues, err)
		}
		return json.Marshal(shared)
	case component.Type == "quiz":
		// Graded here, against the answer key only the server sees.
		selected, err := presentation.QuizSelection(req.Values)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidValues, err)
		}
		outcome, err := presentation.GradeQuiz(component, selected)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidValues, err)
		}
		return json.Marshal(outcome)
	case component.Type == "flashcards" && req.ActionID == presentation.FlashcardsDone:
		latest, err := s.conversations.ListLatestSurfaceInteractions(ctx, models.LatestSurfaceInteractionFilter{
			Scope: req.Conversation.Scope, ConversationID: req.Conversation.ID, SurfaceID: req.SurfaceArtifactID,
		})
		if err != nil {
			return nil, err
		}
		marks := map[string]string{}
		for _, interaction := range latest {
			if interaction == nil || interaction.ActorType != req.Actor.Type || interaction.ActorID != req.Actor.ID ||
				interaction.ComponentID != component.ID || interaction.ActionID != presentation.FlashcardsMark {
				continue
			}
			if result, ok := presentation.FlashcardMark(interaction.Values); ok {
				marks[interaction.ItemKey] = result
			}
		}
		tally, err := presentation.FlashcardsOutcome(component, marks)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidValues, err)
		}
		return json.Marshal(tally)
	}
	return nil, nil
}

// pollVoteOutcome is the tally when this vote is the one that has every
// person in the chat voted on this card version for the first time, else nil.
// Votes carry across versions (each person's latest counts); people who
// voted count as people in the chat.
func (s *Service) pollVoteOutcome(ctx context.Context, req Request, component presentation.Component) (json.RawMessage, error) {
	encoded, err := json.Marshal(req.Values)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidValues, err)
	}
	ids, ok := presentation.PollVote(encoded)
	if !ok {
		return nil, fmt.Errorf("%w: optionIds needs at least one option id", ErrInvalidValues)
	}
	woken, err := s.firstCompletingVote(ctx, req.Conversation, req.SurfaceArtifactID, component.ID, req.SurfaceVersion)
	if err != nil || woken != nil {
		return nil, err
	}
	latest, err := s.conversations.ListLatestSurfaceInteractions(ctx, models.LatestSurfaceInteractionFilter{
		Scope: req.Conversation.Scope, ConversationID: req.Conversation.ID, SurfaceID: req.SurfaceArtifactID,
	})
	if err != nil {
		return nil, err
	}
	votes := conversationrun.PollVotes(component.ID, latest)
	votes[conversationrun.PollVoterKey(req.Actor)] = ids
	if s.people == nil {
		return nil, errors.New("conversation people cannot be read")
	}
	people, err := s.people(ctx, req.Conversation)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	participants := make([]string, 0, len(people)+len(votes))
	for _, person := range people {
		if key := conversationrun.PollVoterKey(person); !keys[key] {
			keys[key] = true
			participants = append(participants, key)
		}
	}
	for voter := range votes {
		if !keys[voter] {
			keys[voter] = true
			participants = append(participants, voter)
		}
	}
	tally, err := presentation.PollOutcome(component, votes, participants)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidValues, err)
	}
	if !tally.EveryoneVoted {
		return nil, nil
	}
	return json.Marshal(tally)
}

// PollCompleted reports whether a stored vote outcome is the tally of the
// vote that had everyone voted.
func PollCompleted(outcome json.RawMessage) bool {
	var tally presentation.PollTally
	return len(outcome) > 0 && json.Unmarshal(outcome, &tally) == nil && tally.EveryoneVoted
}

// firstCompletingVote returns the earliest vote on a poll component of one
// card version whose outcome says everyone voted, or nil while none has.
func (s *Service) firstCompletingVote(ctx context.Context, conversation *models.Conversation, surfaceID, componentID string, version int64) (*models.SurfaceInteraction, error) {
	const page = 1000
	after := int64(0)
	for {
		interactions, err := s.conversations.ListSurfaceInteractions(ctx, models.SurfaceInteractionFilter{
			Scope: conversation.Scope, ConversationID: conversation.ID, SurfaceID: surfaceID, AfterSequence: after, Limit: page,
		})
		if err != nil {
			return nil, err
		}
		for _, interaction := range interactions {
			if interaction == nil {
				continue
			}
			if interaction.Sequence > after {
				after = interaction.Sequence
			}
			if interaction.ComponentID == componentID && interaction.ActionID == presentation.PollVoteAction &&
				interaction.SurfaceVersion == version && PollCompleted(interaction.Outcome) {
				return interaction, nil
			}
		}
		if len(interactions) < page {
			return nil, nil
		}
	}
}
