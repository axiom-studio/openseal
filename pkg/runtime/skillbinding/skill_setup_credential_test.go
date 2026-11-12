package skillbinding_test

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestCredentialBlockedSkillCanRequestSetupWithoutGrantingAccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checks  []skill.DiscoveryCompatibility
		allowed bool
	}{
		{"credentials", []skill.DiscoveryCompatibility{{Requirement: "credential:oauth", Compatible: false, Evidence: "Setup compatibility evidence"}}, true},
		{"credentials and config", []skill.DiscoveryCompatibility{{Requirement: "credential:oauth", Compatible: false, Evidence: "Missing credential"}, {Requirement: "binding_configuration", Compatible: false, Evidence: "Configuration required"}}, true},
		{"host blocked", []skill.DiscoveryCompatibility{{Requirement: "credential:oauth", Compatible: false, Evidence: "Setup compatibility evidence"}, {Requirement: "execution-host:missing", Compatible: false, Evidence: "Setup compatibility evidence"}}, false},
		{"unknown unavailable", nil, false},
		{"empty kind", []skill.DiscoveryCompatibility{{Requirement: "credential:", Compatible: false, Evidence: "Setup compatibility evidence"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := skillbindingtest.NewStore(t)
			scope := models.Scope{Kind: "tenant", ID: "a"}
			catalog := skillbindingtest.SkillActionCatalog(t, ctx, scope, "agent")
			provider := skill.DiscoveryProviderFunc(func(_ context.Context, r skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
				return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{{ID: "reddit.reader", Version: "1.0.0", Name: "Reddit", Readiness: skill.DiscoveryReadinessUnavailable, Compatibility: tc.checks}}}, nil
			})
			dispatcher, err := skillbinding.NewSkillBindingActionDispatcher(store, catalog, nil, provider)
			if err != nil {
				t.Fatal(err)
			}
			run := skillbindingtest.ClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
			run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
			before, _ := catalog.ListBindings(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "agent")
			if tc.allowed {
				_, invalidErr := dispatcher.RequestSkillSetup(ctx, agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "invalid-action", Scope: scope, RunID: run.ID}, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "1.0.0", "reason": "Connect", "requiredActions": []interface{}{"binding_configuration"}}}, run, "agent", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"})
				if invalidErr == nil {
					t.Fatal("configuration requirement accepted as an action")
				}
				pending, _ := store.ListSkillSetupRequests(ctx, scope, "agent", "chat")
				if len(pending) != 0 {
					t.Fatal("invalid permission scope created setup request")
				}
			}

			result, err := dispatcher.RequestSkillSetup(ctx, agentrun.ActionDispatchInput{Call: &models.ActionCall{ID: "call", Scope: scope, RunID: run.ID}, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "1.0.0", "reason": "Connect the requested integration"}}, run, "agent", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"})
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
