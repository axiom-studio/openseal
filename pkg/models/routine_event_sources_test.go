package models

import (
	"reflect"
	"testing"
)

func TestRoutineEventNamedChannels(t *testing.T) {
	trigger := &RoutineEventTrigger{Kind: RoutineEventSlackMessage, Filter: []RoutineEventCondition{
		{Field: "channel", Op: RoutineFilterEquals, Value: "C1"},
		{Field: "channel", Op: RoutineFilterOneOf, Values: []string{"C1", " C2 "}},
		{Field: "channel", Op: RoutineFilterEquals, Value: "C3", Not: true},
		{Field: "channel", Op: RoutineFilterContains, Value: "C4"},
		{Field: "text", Op: RoutineFilterEquals, Value: "C5"},
	}}
	if got := routineEventNamedChannels(trigger); !reflect.DeepEqual(got, []string{"C1", "C2"}) {
		t.Fatalf("channels=%v", got)
	}
	if got := routineEventNamedChannels(&RoutineEventTrigger{Kind: RoutineEventGitHub, Filter: trigger.Filter}); got != nil {
		t.Fatalf("github channels=%v", got)
	}
}
