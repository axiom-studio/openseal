package runtime

import (
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// withChatSkill binds a chat skill whose post takes a destination channel,
// like Slack's slack-send-message.
func (f *routineGrantFixtureState) withChatSkill(t *testing.T) {
	t.Helper()
	text := map[string]interface{}{"type": "string"}
	chat := &skill.Definition{
		ID: "chat", Version: "1.0.0", Name: "Chat", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://chat.invalid"},
		Actions: map[string]skill.Action{
			"post": {Name: "post", Description: "Send a chat message.", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationOptional,
				InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"channel": text, "message": text, "threadTs": text}}},
			"read": {Name: "read", Description: "Read a channel.", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
				InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"channel": text}}},
		},
	}
	if err := f.catalog.Register(t.Context(), chat); err != nil {
		t.Fatal(err)
	}
	if err := f.catalog.Bind(t.Context(), &skill.Binding{ID: "chat-binding", Revision: 1, Scope: f.bound.Binding.Scope, DeploymentID: f.run.AssignedAgentID, SkillID: "chat", SkillVersion: "1.0.0", AllowedActions: []string{"post", "read"}, MaximumRisk: skill.RiskLevelExternal}); err != nil {
		t.Fatal(err)
	}
}

func TestRoutineBriefPinsPostingGrantsToDestinations(t *testing.T) {
	f := routineGrantFixture(t)
	f.withChatSkill(t)
	for name, args := range map[string]map[string]interface{}{
		"unpinned post":           f.withGrants(map[string]interface{}{"action": "chat.post", "label": "Post the digest"}),
		"empty destinations":      f.withGrants(map[string]interface{}{"action": "chat.post", "toDestinations": []interface{}{}}),
		"blank destination":       f.withGrants(map[string]interface{}{"action": "chat.post", "toDestinations": []interface{}{" # "}}),
		"spaced destination":      f.withGrants(map[string]interface{}{"action": "chat.post", "toDestinations": []interface{}{"ai news"}}),
		"no channel to pin":       f.withGrants(map[string]interface{}{"action": "mail.note", "toDestinations": []interface{}{"C1"}}),
		"owner and destinations":  f.withGrants(map[string]interface{}{"action": "mail.send", "toOwnerOnly": true, "toDestinations": []interface{}{"C1"}}),
		"label too long to name":  f.withGrants(map[string]interface{}{"action": "chat.post", "label": strings.Repeat("a", 115), "toDestinations": []interface{}{"C08AINEWS"}}),
		"too many destinations":   f.withGrants(map[string]interface{}{"action": "chat.post", "toDestinations": []interface{}{"a", "b", "c", "d", "e", "f", "g", "h", "i"}}),
		"destination not strings": f.withGrants(map[string]interface{}{"action": "chat.post", "toDestinations": []interface{}{7}}),
	} {
		if _, err := f.resolve(t, args); err == nil {
			t.Fatalf("%s: brief was accepted", name)
		}
	}
	resolved, err := f.resolve(t, f.withGrants(
		map[string]interface{}{"action": "chat.post", "label": "Post to #ai-news", "toDestinations": []interface{}{" #ai-news ", "ai-news"}},
		map[string]interface{}{"action": "chat.read", "label": "Read the digest", "toDestinations": []interface{}{"C08AINEWS"}},
		map[string]interface{}{"action": "chat.read"},
	))
	if err == nil {
		t.Fatal("chat.read was granted twice")
	}
	resolved, err = f.resolve(t, f.withGrants(
		map[string]interface{}{"action": "chat.post", "label": "Post the digest.", "toDestinations": []interface{}{"C08AINEWS", "#ai-news"}},
		map[string]interface{}{"action": "chat.read"},
	))
	if err != nil {
		t.Fatal(err)
	}
	grants := resolved["brief"].(map[string]interface{})["grants"].([]interface{})
	post, read := grants[0].(map[string]interface{}), grants[1].(map[string]interface{})
	if post["label"] != "Post the digest (C08AINEWS, ai-news)" || !reflect.DeepEqual(post["toDestinations"], []interface{}{"C08AINEWS", "ai-news"}) {
		t.Fatalf("post grant = %#v", post)
	}
	// A read takes a channel but posts nothing, so it needs no pin.
	if read["label"] != "Read a channel" || read["toDestinations"] != nil {
		t.Fatalf("read grant = %#v", read)
	}
	named, err := f.resolve(t, f.withGrants(map[string]interface{}{"action": "chat.post", "label": "Post to #AI-News", "toDestinations": []interface{}{"#ai-news"}}))
	if err != nil || named["brief"].(map[string]interface{})["grants"].([]interface{})[0].(map[string]interface{})["label"] != "Post to #AI-News" {
		t.Fatalf("label naming its destination was changed: %#v %v", named, err)
	}
}

func TestRoutineRunsPostOnlyToPinnedDestinationsWithoutAsking(t *testing.T) {
	f := routineGrantFixture(t)
	f.withChatSkill(t)
	activation, _ := f.createRoutine(t, map[string]interface{}{"action": "chat.post", "label": "Post to #ai-news", "toDestinations": []interface{}{"#ai-news", "C08AINEWS"}})
	stored, err := f.store.GetRunbookActivation(t.Context(), f.run.Scope, activation.ID)
	if err != nil || len(stored.Task.Grants) != 1 || !reflect.DeepEqual(stored.Task.Grants[0], RoutineGrant{Action: "chat.post", Label: "Post to #ai-news (C08AINEWS)", ToDestinations: []string{"ai-news", "C08AINEWS"}}) {
		t.Fatalf("persisted grants = %#v, %v", stored.Task, err)
	}
	f.reviewEverything(t, activation)
	coordinator := NewActionCoordinator(f.store, f.store, f.catalog, NewDefaultActionPolicy())
	for index, tc := range []struct {
		name      string
		arguments map[string]interface{}
		asks      bool
	}{
		{"pinned name", map[string]interface{}{"channel": "#ai-news", "message": "Digest"}, false},
		{"pinned id in a thread", map[string]interface{}{"channel": "C08AINEWS", "message": "Digest", "threadTs": "1.2"}, false},
		{"other channel", map[string]interface{}{"channel": "#general", "message": "Digest"}, true},
		{"lookalike", map[string]interface{}{"channel": "ai-news-2", "message": "Digest"}, true},
		{"case changed id", map[string]interface{}{"channel": "c08ainews", "message": "Digest"}, true},
		{"default destination", map[string]interface{}{"message": "Digest"}, true},
	} {
		run := f.routineOccurrence(t, activation, "post-"+tc.name)
		result, err := coordinator.Propose(t.Context(), ProposeActionRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "worker", DeploymentID: run.AssignedAgentID, SkillID: "chat", SkillVersion: "1.0.0", Action: "post", Arguments: tc.arguments, IdempotencyKey: "post-" + string(rune('a'+index))})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if asks := result.Approval != nil; asks != tc.asks {
			t.Fatalf("%s: asks=%v want %v", tc.name, asks, tc.asks)
		}
	}

	// A posting grant stored without destinations never covers a post.
	unpinned := cloneRunbookActivation(stored)
	unpinned.Task.Grants = []RoutineGrant{{Action: "chat.post", Label: "Post the digest"}}
	unpinned.Revision++
	if err := f.store.UpdateRunbookActivation(t.Context(), unpinned, stored.Revision); err != nil {
		t.Fatal(err)
	}
	post := &skill.BoundAction{Definition: &skill.Definition{ID: "chat"}, Action: skill.Action{Name: "post", SideEffect: skill.SideEffectExternal,
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"channel": map[string]interface{}{"type": "string"}}}}}
	if grant, err := resolveRoutineGrant(t.Context(), f.store, f.store, nil, f.routineOccurrence(t, unpinned, "unpinned"), post, map[string]interface{}{"channel": "ai-news"}); err != nil || grant != nil {
		t.Fatalf("unpinned posting grant applied: %#v %v", grant, err)
	}
}

func TestRoutineDestinationsPinned(t *testing.T) {
	keys := []string{"channel", "channels"}
	pinned := []string{"ai-news", "C1"}
	for name, tc := range map[string]struct {
		arguments map[string]interface{}
		want      bool
	}{
		"name":          {map[string]interface{}{"channel": "#ai-news"}, true},
		"id":            {map[string]interface{}{"channel": " C1 "}, true},
		"list":          {map[string]interface{}{"channels": []interface{}{"C1", "ai-news"}}, true},
		"list with one": {map[string]interface{}{"channels": []interface{}{"C1", "C2"}}, false},
		"both keys":     {map[string]interface{}{"channel": "C1", "channels": []interface{}{"C9"}}, false},
		"non string":    {map[string]interface{}{"channel": 7}, false},
		"object":        {map[string]interface{}{"channel": map[string]interface{}{"id": "C1"}}, false},
		"none":          {map[string]interface{}{"message": "hi"}, false},
		"blank":         {map[string]interface{}{"channel": " "}, false},
	} {
		if got := routineDestinationsPinned(tc.arguments, keys, pinned); got != tc.want {
			t.Fatalf("%s: got %v", name, got)
		}
	}
}
