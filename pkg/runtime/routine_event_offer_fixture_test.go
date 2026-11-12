// Copies of runtime's routine event fixtures for the external routine
// offer test, which moves with the external conversation code.

package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

// slackRoutineMessage is a Slack channel message event from participant.
func slackRoutineMessage(id, participant, name, text string, bot bool) models.NormalizedExternalConversationEvent {
	return models.NormalizedExternalConversationEvent{
		ID: "slack:message:T123:C123:" + id, Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "C123", ExternalMessageID: id, ExternalThreadID: id,
		ExternalParticipantID: participant, ParticipantDisplayName: name, ParticipantIsBot: bot,
		Text: text, OrderingKey: "C123:" + id, OccurredAt: time.Now().UTC(),
	}
}

// newUnboundRoutineEventMatcher is a routine event matcher whose scope has a
// routine on another connection only.
func newUnboundRoutineEventMatcher(tb testing.TB) (*runtime.RoutineEventMatcher, *countingRoutineEventStore) {
	tb.Helper()
	store := &countingRoutineEventStore{}
	matcher := runtime.NewRoutineEventMatcher(store)
	tb.Cleanup(matcher.Close)
	// The scope has a routine, but on another connection.
	if err := matcher.RefreshRoutineIndex(context.Background(), models.Scope{Kind: "tenant", ID: "busy"}); err != nil {
		tb.Fatal(err)
	}
	return matcher, store
}

// countingRoutineEventStore fails the test on any store call: an offer for
// a source no routine listens to must not reach the store.
type countingRoutineEventStore struct {
	models.RoutineEventStore
	calls int
}

func (s *countingRoutineEventStore) GetRunbookActivation(context.Context, models.Scope, string) (*models.RunbookActivation, error) {
	s.calls++
	return nil, nil
}

func (s *countingRoutineEventStore) ListRoutineEventActivations(context.Context, models.Scope, models.RoutineEventKind, string) ([]*models.RunbookActivation, error) {
	s.calls++
	return nil, nil
}

func (s *countingRoutineEventStore) InsertRoutineEvent(context.Context, *models.RoutineEventRecord) (bool, error) {
	s.calls++
	return false, nil
}

func (s *countingRoutineEventStore) CountPendingRoutineEvents(context.Context, models.Scope, string) (int, error) {
	s.calls++
	return 0, nil
}

func (s *countingRoutineEventStore) CountPendingRoutineEventsInScope(context.Context, models.Scope) (int, error) {
	s.calls++
	return 0, nil
}

func (s *countingRoutineEventStore) ListRoutineEventSourceKeysInScope(_ context.Context, scope models.Scope) ([]models.RoutineEventSourceKey, error) {
	return []models.RoutineEventSourceKey{{Scope: scope, Kind: models.RoutineEventSlackMessage, SourceRef: models.RoutineConversationSourceRef("deployment", "bound", "slack", "listening")}}, nil
}
