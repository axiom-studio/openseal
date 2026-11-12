package runtime

import (
	"encoding/json"
	"time"
)

// nullableRoutineEventIndex turns the index columns into SQL values (NULL
// when the activation is not an event Task).
func nullableRoutineEventIndex(activation *RunbookActivation) (interface{}, interface{}) {
	kind, sourceRef := routineEventIndexOf(activation)
	if kind == "" {
		return nil, nil
	}
	return string(kind), sourceRef
}

// routineEventRecordPayload is the stored payload of a record: the full
// event for a pending record, nothing for a dropped one.
func routineEventRecordPayload(record *RoutineEventRecord) ([]byte, error) {
	if record.State == RoutineEventDropped {
		return nil, nil
	}
	return json.Marshal(record.Event)
}

// decodeRoutineEventRecordEvent rebuilds the event of a stored row. A row
// without payload (dropped) keeps only its ID, test flag and time.
func decodeRoutineEventRecordEvent(record *RoutineEventRecord, payload []byte, eventID string, test bool, occurredAt time.Time) error {
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

var (
	_ RoutineEventStore = (*SQLiteStore)(nil)
	_ RoutineEventStore = (*PostgresStore)(nil)
)
