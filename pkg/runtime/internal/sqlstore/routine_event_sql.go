package sqlstore

import (
	"encoding/json"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// nullableRoutineEventIndex turns the index columns into SQL values (NULL
// when the activation is not an event Task).
func NullableRoutineEventIndex(activation *models.RunbookActivation) (interface{}, interface{}) {
	kind, sourceRef := models.RoutineEventIndexOf(activation)
	if kind == "" {
		return nil, nil
	}
	return string(kind), sourceRef
}

// routineEventRecordPayload is the stored payload of a record: the full
// event for a pending record, nothing for a dropped one.
func RoutineEventRecordPayload(record *models.RoutineEventRecord) ([]byte, error) {
	if record.State == models.RoutineEventDropped {
		return nil, nil
	}
	return json.Marshal(record.Event)
}

// decodeRoutineEventRecordEvent rebuilds the event of a stored row. A row
// without payload (dropped) keeps only its ID, test flag and time.
func DecodeRoutineEventRecordEvent(record *models.RoutineEventRecord, payload []byte, eventID string, test bool, occurredAt time.Time) error {
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &record.Event); err != nil {
			return err
		}
	}
	record.Event.ID, record.Event.Test = eventID, test
	if record.Event.OccurredAt.IsZero() {
		record.Event.OccurredAt = occurredAt.UTC()
	}
	return nil
}
