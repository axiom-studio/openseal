package runtime

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/skill"
	"testing"
)

func TestCredentialBlockedSkillCanRequestSetupWithoutGrantingAccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checks  []skill.DiscoveryCompatibility
		allowed bool
	}{
		{"credentials", []skill.DiscoveryCompatibility{{Requirement: "credential:oauth", Compatible: false, Evidence: "Setup compatibility evidence"}}, true},
		{"host blocked", []skill.DiscoveryCompatibility{{Requirement: "credential:oauth", Compatible: false, Evidence: "Setup compatibility evidence"}, {Requirement: "execution-host:missing", Compatible: false, Evidence: "Setup compatibility evidence"}}, false},
		{"unknown unavailable", nil, false},
		{"empty kind", []skill.DiscoveryCompatibility{{Requirement: "credential:", Compatible: false, Evidence: "Setup compatibility evidence"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := NewMemoryStore()
			scope := Scope{Kind: "tenant", ID: "a"}
			catalog := skillActionCatalog(t, ctx, scope, "agent")
			provider := skill.DiscoveryProviderFunc(func(_ context.Context, r skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
				return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{{ID: "reddit.reader", Version: "1.0.0", Name: "Reddit", Readiness: skill.DiscoveryReadinessUnavailable, Compatibility: tc.checks}}}, nil
			})
			dispatcher, err := NewSkillBindingActionDispatcher(store, catalog, nil, provider)
			if err != nil {
				t.Fatal(err)
			}
			run := createClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
			run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
			before, _ := catalog.ListBindings(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "agent")
			result, err := dispatcher.requestSkillSetup(ctx, ActionDispatchInput{Call: &ActionCall{ID: "call", Scope: scope, RunID: run.ID}, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "1.0.0", "reason": "Connect the requested integration"}}, run, "agent")
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v result=%v err=%v", tc.allowed, result, err)
			}
			requests, _ := store.ListSkillSetupRequests(ctx, scope, "agent", "chat")
			expected := 0
			if tc.allowed {
				expected = 1
			}
			if len(requests) != expected {
				t.Fatalf("requests=%d", len(requests))
			}
			after, _ := catalog.ListBindings(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "agent")
			if len(after) != len(before) {
				t.Fatal("setup granted a binding")
			}
		})
	}
}
