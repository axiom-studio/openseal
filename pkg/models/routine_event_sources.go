package models

import (
	"slices"
	"strings"
)

// routineEventNamedChannels lists the channels or chats a Slack or Telegram
// trigger names in its equals/one_of place conditions, the ones the
// runtime's create-time channel pinning checked.
func routineEventNamedChannels(trigger *RoutineEventTrigger) []string {
	if trigger == nil {
		return nil
	}
	field := RoutineEventPlaceField(trigger.Kind)
	if field == "" {
		return nil
	}
	var channels []string
	for _, condition := range trigger.Filter {
		if condition.Field != field || condition.Not || (condition.Op != RoutineFilterEquals && condition.Op != RoutineFilterOneOf) {
			continue
		}
		values := condition.Values
		if condition.Op == RoutineFilterEquals {
			values = []string{condition.Value}
		}
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" && !slices.Contains(channels, value) {
				channels = append(channels, value)
			}
		}
	}
	return channels
}
