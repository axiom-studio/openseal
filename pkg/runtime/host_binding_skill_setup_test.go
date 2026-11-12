package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Skill setup is refused for a service a connected app already covers for
// this run, and the refusal names the connected tools to use instead.
func TestSkillSetupDefersToAConnectedApp(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	scope := Scope{Kind: "tenant", ID: "a"}
	catalog := skillActionCatalog(t, ctx, scope, "agent")
	run := createClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	hosted := hostConnectorCatalog(t, scope, map[string]bool{run.ID: true})
	if err := catalog.Register(ctx, mustDefinition(t, hosted, "openseal.connector")); err != nil {
		t.Fatal(err)
	}
	catalog.SetHostBindingProvider(func(ctx context.Context, scope skill.ScopeReference, deploymentID string, actor skill.RunActor) (*skill.HostBindingSet, error) {
		return hosted.HostBindings(ctx, scope, deploymentID, actor)
	})
	provider := skill.DiscoveryProviderFunc(func(context.Context, skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{
			{ID: "linear", Version: "1.0.0", Name: "Linear", Readiness: skill.DiscoveryReadinessBindable},
			{ID: "reddit.reader", Version: "1.0.0", Name: "Reddit", Readiness: skill.DiscoveryReadinessBindable},
		}}, nil
	})
	dispatcher, err := NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	setup := func(callID, skillID string, run *AgentRun) (map[string]interface{}, error) {
		return dispatcher.requestSkillSetup(ctx, agentrun.ActionDispatchInput{Call: &ActionCall{ID: callID, Scope: scope, RunID: run.ID},
			Arguments: map[string]interface{}{"kind": "configure", "skillId": skillID, "skillVersion": "latest", "reason": "File a ticket"}}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	}
	_, err = setup("call-linear", "linear", run)
	var refusal *interactionRequestRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.message, "Linear is already connected") ||
		!strings.Contains(refusal.message, "mcp.linear.create_issue") || !strings.Contains(refusal.message, "mcp.linear.search") {
		t.Fatalf("setup of a connected service = %v", err)
	}
	if requests, _ := store.ListSkillSetupRequests(ctx, scope, "agent", "chat"); len(requests) != 0 {
		t.Fatalf("a setup form was created: %#v", requests)
	}
	// Other services, and the same service for a run without the connection,
	// still get a setup form.
	if result, err := setup("call-reddit", "reddit.reader", run); err != nil || result["setupRequest"] == nil {
		t.Fatalf("unrelated setup = %#v, %v", result, err)
	}
	other := cloneAgentRun(run)
	other.ID = "other-users-run"
	if result, err := setup("call-other", "linear", other); err != nil || result["setupRequest"] == nil {
		t.Fatalf("setup for a run without the connection = %#v, %v", result, err)
	}
}

func TestServiceKey(t *testing.T) {
	for value, want := range map[string]string{
		"Linear": "linear", "linear-mcp": "linear", "skill-linear": "linear", "neondatabase/mcp-server-neon": "neondatabase-neon",
		"Google Drive": "google-drive", "MCP Server": "",
	} {
		if got := serviceKey(value); got != want {
			t.Fatalf("serviceKey(%q) = %q, want %q", value, got, want)
		}
	}
}
