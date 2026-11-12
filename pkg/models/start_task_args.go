package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaximumStartTaskGoalBytes           = 16000
	MaximumStartTaskAcknowledgmentBytes = 600
	startTaskKeyPrefix                  = "task-"
)

type StartTaskArgs struct {
	Goal           string `json:"goal"`
	Acknowledgment string `json:"acknowledgment"`
}

// ParseStartTaskArgs strictly decodes and checks the arguments of a
// conversation_start_task call: a trimmed goal and acknowledgment within
// their limits.
func ParseStartTaskArgs(raw json.RawMessage) (*StartTaskArgs, error) {
	args := &StartTaskArgs{}
	if err := DecodeControlArguments(raw, args); err != nil {
		return nil, err
	}
	args.Goal = strings.TrimSpace(args.Goal)
	args.Acknowledgment = strings.TrimSpace(args.Acknowledgment)
	if args.Goal == "" || len(args.Goal) > MaximumStartTaskGoalBytes || !utf8.ValidString(args.Goal) {
		return nil, fmt.Errorf("goal must be valid, non-empty text of at most %d bytes", MaximumStartTaskGoalBytes)
	}
	if args.Acknowledgment == "" || len(args.Acknowledgment) > MaximumStartTaskAcknowledgmentBytes || !utf8.ValidString(args.Acknowledgment) {
		return nil, fmt.Errorf("acknowledgment must be valid, non-empty text of at most %d bytes", MaximumStartTaskAcknowledgmentBytes)
	}
	return args, nil
}

// StartTaskEntryArgs decodes the arguments of a conversation_start_task
// entry exactly as the handler does (nil when it is not one or they are
// invalid). Admission and provenance proofs compare against these.
func StartTaskEntryArgs(entry *SessionEntry) *StartTaskArgs {
	if entry == nil || entry.Type != SessionEntryTool || entry.Tool == nil || entry.Tool.Name != string(ControlToolStartTask) ||
		entry.Tool.ArgumentsError != "" {
		return nil
	}
	args, err := ParseStartTaskArgs(entry.Tool.Arguments)
	if err != nil {
		return nil
	}
	return args
}

// StartTaskKey is the idempotency key of the task an entry starts (§3.4).
func StartTaskKey(entryID string) string { return startTaskKeyPrefix + entryID }
