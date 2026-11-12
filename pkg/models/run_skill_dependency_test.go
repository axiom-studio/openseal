package models

import (
	"testing"
)

func TestRunSkillActorUmbrellaIsTheRunOwner(t *testing.T) {
	for name, test := range map[string]struct {
		run   *AgentRun
		valid bool
	}{
		"1:1":       {&AgentRun{ID: "r", Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "a"}, AssignedAgentID: "a", ActingUser: "7"}, true},
		"Team":      {&AgentRun{ID: "r", Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "a", ActingUser: "7"}, true},
		"delegated": {&AgentRun{ID: "r", Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "a"}, AssignedAgentID: "b", ActingUser: "7"}, false},
	} {
		actor := RunSkillActor(test.run)
		if actor.Umbrella != SkillUmbrella(test.run.Owner) || actor.AgentID != test.run.AssignedAgentID || actor.ActingUser != "7" || actor.Valid() != test.valid {
			t.Errorf("%s: actor = %#v valid=%v", name, actor, actor.Valid())
		}
	}
}
