package skill_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// hostProvider supplies one connected-app binding per allowed run, as a host
// does for a user's or the workspace's connected apps.
type hostProvider struct {
	calls    atomic.Int64
	allowed  map[string]bool
	bindings func() []*skill.Binding
	revision string
}

func (p *hostProvider) provide(_ context.Context, scope skill.ScopeReference, deploymentID string, actor skill.RunActor) (*skill.HostBindingSet, error) {
	p.calls.Add(1)
	if !p.allowed[actor.RunID] {
		return nil, nil
	}
	set := &skill.HostBindingSet{Fingerprint: p.revision, Services: map[string][]string{}}
	for _, binding := range p.bindings() {
		binding.Scope, binding.DeploymentID = scope, deploymentID
		set.Bindings = append(set.Bindings, binding)
		set.Services[binding.ID] = []string{binding.Slug}
	}
	return set, nil
}

func hostCatalog(t *testing.T) *skill.Catalog {
	t.Helper()
	catalog := skill.NewCatalog()
	if err := catalog.Register(context.Background(), mcp.SkillDefinition()); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func hostBindingToolNames(snapshot *skill.ActivationSnapshot) map[string]string {
	names := map[string]string{}
	for _, activated := range snapshot.Skills {
		for _, action := range activated.Actions {
			names[action.Name] = action.BindingID
		}
	}
	return names
}

// A connected app reaches an Agent's run only through the run's actor: it is
// named mcp.<slug>.<tool>, resolvable at action time for that run, and
// invisible to activations and resolutions without the actor or for another
// deployment.
func TestHostBindingsActivateAndResolveOnlyForTheirRun(t *testing.T) {
	ctx := context.Background()
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	catalog := hostCatalog(t)
	search := mcpSnapshot("search", true)
	provider := &hostProvider{allowed: map[string]bool{"run-mine": true}, revision: "r1", bindings: func() []*skill.Binding {
		binding := mcpBinding(t, "mcp:mcpc-linear", "linear-mcp", search)
		binding.AllowedActions = []string{mcp.Read}
		binding.MaximumRisk = skill.RiskLevelRead
		return []*skill.Binding{binding}
	}}
	catalog.SetHostBindingProvider(provider.provide)

	mine := skill.WithRunActor(ctx, skill.RunActor{RunID: "run-mine", AgentID: "agent"})
	snapshot, err := catalog.Activate(mine, scope, "agent", skill.HostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := hostBindingToolNames(snapshot)["mcp.linear-mcp.search"]; got != "mcp:mcpc-linear" {
		t.Fatalf("connected tool missing from activation: %v", hostBindingToolNames(snapshot))
	}
	bound, err := catalog.Resolve(mine, scope, "agent", mcp.SkillID, mcp.SkillVersion, mcp.Read, skill.BindingReference{ID: "mcp:mcpc-linear", Revision: 1})
	if err != nil || !bound.Host || bound.Binding.DeploymentID != "agent" {
		t.Fatalf("resolve for the run = %#v, %v", bound, err)
	}
	if binding, err := catalog.GetHostBinding(mine, scope, "agent", "mcp:mcpc-linear"); err != nil || binding == nil || binding.Revision != 1 {
		t.Fatalf("host binding lookup = %#v, %v", binding, err)
	}

	for name, other := range map[string]context.Context{
		"no actor":         ctx,
		"another user run": skill.WithRunActor(ctx, skill.RunActor{RunID: "run-theirs", AgentID: "agent"}),
		"another Agent":    skill.WithRunActor(ctx, skill.RunActor{RunID: "run-mine", AgentID: "team"}),
	} {
		snapshot, err := catalog.Activate(other, scope, "agent", skill.HostCapabilityState{})
		if err != nil {
			t.Fatal(err)
		}
		if _, leaked := hostBindingToolNames(snapshot)["mcp.linear-mcp.search"]; leaked {
			t.Fatalf("%s: connected tool leaked into activation", name)
		}
		if _, err := catalog.Resolve(other, scope, "agent", mcp.SkillID, mcp.SkillVersion, mcp.Read, skill.BindingReference{ID: "mcp:mcpc-linear", Revision: 1}); !errors.Is(err, skill.ErrBindingUnavailable) {
			t.Fatalf("%s: resolve = %v, want unavailable", name, err)
		}
	}
	// A stale revision is refused like a changed stored binding.
	if _, err := catalog.Resolve(mine, scope, "agent", mcp.SkillID, mcp.SkillVersion, mcp.Read, skill.BindingReference{ID: "mcp:mcpc-linear", Revision: 2}); !errors.Is(err, skill.ErrBindingUnavailable) {
		t.Fatalf("stale revision resolved: %v", err)
	}
}

// Activations are cached per host binding set: an unchanged set reuses the
// snapshot, a changed revision or a different user's set computes its own.
func TestHostBindingsKeyTheActivationCache(t *testing.T) {
	ctx := context.Background()
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	catalog := hostCatalog(t)
	revision := int64(1)
	tools := []mcp.ToolSnapshot{mcpSnapshot("search", true)}
	provider := &hostProvider{allowed: map[string]bool{"run-a": true, "run-b": true}, revision: "r1", bindings: func() []*skill.Binding {
		binding := mcpBinding(t, "mcp:mcpc-linear", "linear-mcp", tools...)
		binding.Revision, binding.AllowedActions = revision, []string{mcp.Read, mcp.Call}
		return []*skill.Binding{binding}
	}}
	catalog.SetHostBindingProvider(provider.provide)
	actor := skill.WithRunActor(ctx, skill.RunActor{RunID: "run-a", AgentID: "agent"})
	first, err := catalog.Activate(actor, scope, "agent", skill.HostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.Activate(skill.WithRunActor(ctx, skill.RunActor{RunID: "run-b", AgentID: "agent"}), scope, "agent", skill.HostCapabilityState{})
	if err != nil || second.SnapshotID != first.SnapshotID {
		t.Fatalf("an identical set must share the snapshot: %v", err)
	}
	none, err := catalog.Activate(ctx, scope, "agent", skill.HostCapabilityState{})
	if err != nil || none.SnapshotID == first.SnapshotID {
		t.Fatalf("a run without the set reused its snapshot: %v", err)
	}
	revision, provider.revision = 2, "r2"
	tools = append(tools, mcpSnapshot("create_issue", false))
	changed, err := catalog.Activate(actor, scope, "agent", skill.HostCapabilityState{})
	if err != nil || changed.SnapshotID == first.SnapshotID {
		t.Fatalf("a changed connection reused the old snapshot: %v", err)
	}
	if _, ok := hostBindingToolNames(changed)["mcp.linear-mcp.create_issue"]; !ok {
		t.Fatalf("new tool missing: %v", hostBindingToolNames(changed))
	}
	// Every activation with an actor asks the provider; one without never does.
	if provider.calls.Load() != 3 {
		t.Fatalf("provider calls = %d", provider.calls.Load())
	}
}

// A host binding is validated like a stored one and may not collide with a
// stored binding's ID or model names.
func TestHostBindingsAreValidatedAndNeverShadowStoredBindings(t *testing.T) {
	ctx := context.Background()
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	search := mcpSnapshot("search", true)
	for name, test := range map[string]struct {
		stored *skill.Binding
		host   func() *skill.Binding
		want   string
	}{
		"same id as a stored binding": {
			stored: mcpBinding(t, "mcp:mcpc-linear", "linear-stored", search),
			host:   func() *skill.Binding { return mcpBinding(t, "mcp:mcpc-linear", "linear-mcp", search) },
			want:   "collides with a stored binding",
		},
		"same slug as a stored binding": {
			stored: mcpBinding(t, "linear-binding", "linear-mcp", search),
			host:   func() *skill.Binding { return mcpBinding(t, "mcp:mcpc-linear", "linear-mcp", search) },
			want:   "already used by binding",
		},
		"action outside the definition": {
			host: func() *skill.Binding {
				binding := mcpBinding(t, "mcp:mcpc-linear", "linear-mcp", search)
				binding.AllowedActions = append(binding.AllowedActions, "delete_everything")
				return binding
			},
			want: "host skill binding mcp:mcpc-linear is invalid",
		},
	} {
		t.Run(name, func(t *testing.T) {
			catalog := hostCatalog(t)
			if test.stored != nil {
				if err := catalog.Bind(ctx, test.stored); err != nil {
					t.Fatal(err)
				}
			}
			catalog.SetHostBindingProvider((&hostProvider{allowed: map[string]bool{"run": true}, bindings: func() []*skill.Binding { return []*skill.Binding{test.host()} }}).provide)
			actor := skill.WithRunActor(ctx, skill.RunActor{RunID: "run", AgentID: "agent"})
			if _, err := catalog.Activate(actor, scope, "agent", skill.HostCapabilityState{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("activation error = %v, want %q", err, test.want)
			}
		})
	}
	t.Run("another deployment", func(t *testing.T) {
		catalog := hostCatalog(t)
		catalog.SetHostBindingProvider(func(_ context.Context, scope skill.ScopeReference, _ string, _ skill.RunActor) (*skill.HostBindingSet, error) {
			binding := mcpBinding(t, "mcp:mcpc-linear", "linear-mcp", search)
			binding.Scope, binding.DeploymentID = scope, "other-agent"
			return &skill.HostBindingSet{Bindings: []*skill.Binding{binding}}, nil
		})
		actor := skill.WithRunActor(ctx, skill.RunActor{RunID: "run", AgentID: "agent"})
		if _, err := catalog.Activate(actor, scope, "agent", skill.HostCapabilityState{}); err == nil || !strings.Contains(err.Error(), "must be an enabled binding of this deployment") {
			t.Fatalf("activation error = %v", err)
		}
	})
}
