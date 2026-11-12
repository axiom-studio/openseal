package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	defaultSurfaceInteractionLimit = 100
	// MaximumSurfaceInteractionLimit leaves room for the change stream's
	// one-row look-ahead over its own maximum page.
	MaximumSurfaceInteractionLimit = 1000
	maximumSurfaceInteractionBytes = 64 << 10
)

var (
	ErrInvalidSurfaceInteraction  = errors.New("invalid surface interaction")
	ErrSurfaceInteractionConflict = errors.New("surface interaction conflict")
)

// SurfaceInteraction is one durable answer to an interactive card component:
// a quiz pick, a checklist tick, a poll vote, a flashcard mark. An app in a
// chat reports results the same way, on the app artifact itself
// (SurfaceArtifactID and SurfaceID are its id; see presentation.AppInteraction).
// Interactions are version-scoped (SurfaceVersion) and per actor.
//
// Uniqueness is chosen by Repeatable when the interaction is created:
//
//   - Single-use (Repeatable false): one interaction per
//     (conversation, surfaceId, surfaceVersion, componentId, actor). A second
//     one under a different idempotency key fails with a
//     *SurfaceInteractionConflictError carrying the stored interaction.
//   - Repeatable (Repeatable true): any number per component; each is
//     deduplicated by IdempotencyKey only.
//
// Every interaction, single-use or repeatable, replays on its own
// IdempotencyKey: creating it again with the same key and target returns the
// stored record (first write wins for that key; Values are not compared).
//
// ItemKey names a sub-item of a repeatable component, such as a checklist
// item or a poll option. State that is "last write wins per item" is read
// with ListLatestSurfaceInteractions, which keeps the latest interaction per
// (surfaceId, componentId, itemKey) and, unless AcrossActors, per actor.
// Removing a tick is a new interaction whose Values say so.
//
// MessageID decides who reads an interaction: every viewer who can see
// MessageID (the message whose references carry the surface artifact) reads
// all of that card's interactions.
type SurfaceInteraction struct {
	ID             string `json:"id"`
	Scope          Scope  `json:"scope"`
	ConversationID string `json:"conversationId"`
	// Sequence is assigned by the store, increasing per conversation. The
	// conversation change stream pages interactions by it.
	Sequence          int64                       `json:"sequence"`
	SurfaceArtifactID string                      `json:"surfaceArtifactId"`
	SurfaceID         string                      `json:"surfaceId"`
	SurfaceVersion    int64                       `json:"surfaceVersion"`
	MessageID         string                      `json:"messageId"`
	ComponentID       string                      `json:"componentId"`
	ItemKey           string                      `json:"itemKey,omitempty"`
	ActionID          string                      `json:"actionId,omitempty"`
	ActorType         ConversationParticipantType `json:"actorType"`
	ActorID           string                      `json:"actorId"`
	Repeatable        bool                        `json:"repeatable"`
	Values            json.RawMessage             `json:"values"`
	// Outcome is server-computed, for example a quiz grade.
	Outcome        json.RawMessage `json:"outcome,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey"`
	CreatedAt      time.Time       `json:"createdAt"`
}

// SurfaceInteractionConflictError reports a second single-use interaction, or
// an idempotency key reused for a different target. Existing is the stored
// interaction. It matches ErrSurfaceInteractionConflict with errors.Is.
type SurfaceInteractionConflictError struct {
	Existing *SurfaceInteraction
}

func (e *SurfaceInteractionConflictError) Error() string {
	if e == nil || e.Existing == nil {
		return ErrSurfaceInteractionConflict.Error()
	}
	return fmt.Sprintf("%s: component %q of surface %q version %d already has interaction %q",
		ErrSurfaceInteractionConflict, e.Existing.ComponentID, e.Existing.SurfaceID, e.Existing.SurfaceVersion, e.Existing.ID)
}

func (e *SurfaceInteractionConflictError) Is(target error) bool {
	return target == ErrSurfaceInteractionConflict
}

// SurfaceInteractionFilter lists interactions in ascending Sequence order.
// SurfaceID narrows the list to one surface across all its versions.
// Viewer, honoured by conversation.Service only (stores reject it), keeps the
// interactions that viewer may read; see SurfaceInteraction.
type SurfaceInteractionFilter struct {
	Scope          Scope
	ConversationID string
	SurfaceID      string
	AfterSequence  int64
	Limit          int
	Viewer         *ConversationViewer
}

// LatestSurfaceInteractionFilter selects the last-write-wins state of one
// surface: the latest interaction per (componentId, itemKey, actor), or per
// (componentId, itemKey) when AcrossActors is set (shared checklists and
// polls). Versions are not part of the key, so state carries forward to a
// new version that keeps the component.
//
// Viewer, honoured by conversation.Service only (stores reject it), reduces
// only the interactions that viewer may read: those on a card message the
// viewer can see.
type LatestSurfaceInteractionFilter struct {
	Scope          Scope
	ConversationID string
	SurfaceID      string
	AcrossActors   bool
	Viewer         *ConversationViewer
}

func (i *SurfaceInteraction) Validate() error {
	if i == nil {
		return fmt.Errorf("%w: interaction is required", ErrInvalidSurfaceInteraction)
	}
	if err := i.Scope.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"id": i.ID, "conversation id": i.ConversationID, "surface artifact id": i.SurfaceArtifactID,
		"surface id": i.SurfaceID, "message id": i.MessageID, "component id": i.ComponentID, "idempotency key": i.IdempotencyKey,
	} {
		if !IsValidOpaqueIdentifier(value, 128) || strings.TrimSpace(value) != value {
			return fmt.Errorf("%w: %s must be a portable opaque identifier", ErrInvalidSurfaceInteraction, name)
		}
	}
	for name, value := range map[string]string{"item key": i.ItemKey, "action id": i.ActionID} {
		if value != "" && (!IsValidOpaqueIdentifier(value, 128) || strings.TrimSpace(value) != value) {
			return fmt.Errorf("%w: %s must be a portable opaque identifier", ErrInvalidSurfaceInteraction, name)
		}
	}
	if i.ItemKey != "" && !i.Repeatable {
		return fmt.Errorf("%w: only repeatable interactions have an item key", ErrInvalidSurfaceInteraction)
	}
	if i.SurfaceVersion < 1 {
		return fmt.Errorf("%w: surface version must be positive", ErrInvalidSurfaceInteraction)
	}
	if err := (ConversationParticipant{Type: i.ActorType, ID: i.ActorID}).Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSurfaceInteraction, err)
	}
	if len(i.Values) == 0 || !json.Valid(i.Values) {
		return fmt.Errorf("%w: values must be JSON", ErrInvalidSurfaceInteraction)
	}
	if len(i.Outcome) > 0 && !json.Valid(i.Outcome) {
		return fmt.Errorf("%w: outcome must be JSON", ErrInvalidSurfaceInteraction)
	}
	if len(i.Values)+len(i.Outcome) > maximumSurfaceInteractionBytes {
		return fmt.Errorf("%w: values and outcome exceed %d bytes", ErrInvalidSurfaceInteraction, maximumSurfaceInteractionBytes)
	}
	if i.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created time is required", ErrInvalidSurfaceInteraction)
	}
	return nil
}

func (f SurfaceInteractionFilter) Validate() error {
	if err := f.Scope.Validate(); err != nil {
		return err
	}
	if !IsValidOpaqueIdentifier(f.ConversationID, 128) {
		return fmt.Errorf("%w: conversation id is required", ErrInvalidSurfaceInteraction)
	}
	if f.SurfaceID != "" && !IsValidOpaqueIdentifier(f.SurfaceID, 128) {
		return fmt.Errorf("%w: invalid surface id", ErrInvalidSurfaceInteraction)
	}
	if f.AfterSequence < 0 || f.Limit < 0 || f.Limit > MaximumSurfaceInteractionLimit {
		return fmt.Errorf("%w: invalid page", ErrInvalidSurfaceInteraction)
	}
	if f.Viewer != nil {
		return errSurfaceInteractionStoreViewer
	}
	return nil
}

func (f SurfaceInteractionFilter) EffectiveLimit() int {
	if f.Limit == 0 {
		return defaultSurfaceInteractionLimit
	}
	return f.Limit
}

func (f LatestSurfaceInteractionFilter) Validate() error {
	if err := f.Scope.Validate(); err != nil {
		return err
	}
	if !IsValidOpaqueIdentifier(f.ConversationID, 128) || !IsValidOpaqueIdentifier(f.SurfaceID, 128) {
		return fmt.Errorf("%w: conversation and surface ids are required", ErrInvalidSurfaceInteraction)
	}
	if f.Viewer != nil {
		return errSurfaceInteractionStoreViewer
	}
	return nil
}

var errSurfaceInteractionStoreViewer = fmt.Errorf("%w: viewer filtering is applied by ConversationService, not the store", ErrInvalidSurfaceInteraction)

// SameSurfaceInteractionTarget reports whether a replay under one idempotency
// key addresses the same component, item and actor as the stored interaction.
func SameSurfaceInteractionTarget(left, right *SurfaceInteraction) bool {
	return left != nil && right != nil && left.Scope == right.Scope && left.ConversationID == right.ConversationID &&
		left.SurfaceArtifactID == right.SurfaceArtifactID && left.SurfaceID == right.SurfaceID &&
		left.SurfaceVersion == right.SurfaceVersion && left.ComponentID == right.ComponentID &&
		left.ItemKey == right.ItemKey && left.ActionID == right.ActionID && left.ActorType == right.ActorType &&
		left.ActorID == right.ActorID && left.Repeatable == right.Repeatable
}

// LatestSurfaceInteractions reduces interactions in ascending Sequence order
// to the last write per item, keeping that ascending order.
func LatestSurfaceInteractions(interactions []*SurfaceInteraction, acrossActors bool) []*SurfaceInteraction {
	latest := make(map[string]int, len(interactions))
	for index, interaction := range interactions {
		key := interaction.ComponentID + "\x00" + interaction.ItemKey
		if !acrossActors {
			key += "\x00" + string(interaction.ActorType) + "\x00" + interaction.ActorID
		}
		latest[key] = index
	}
	result := make([]*SurfaceInteraction, 0, len(latest))
	for index, interaction := range interactions {
		key := interaction.ComponentID + "\x00" + interaction.ItemKey
		if !acrossActors {
			key += "\x00" + string(interaction.ActorType) + "\x00" + interaction.ActorID
		}
		if latest[key] == index {
			result = append(result, interaction)
		}
	}
	return result
}

func CloneSurfaceInteraction(in *SurfaceInteraction) *SurfaceInteraction {
	if in == nil {
		return nil
	}
	out := *in
	out.Values = bytes.Clone(in.Values)
	out.Outcome = bytes.Clone(in.Outcome)
	return &out
}
