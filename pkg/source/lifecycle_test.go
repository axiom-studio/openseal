package source

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestLifecycleVersionsActivationRevocationAndCAS(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryLifecycleStore()
	service, err := NewLifecycleService(store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 2, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ids := []string{"activate-1", "activate-2", "revoke-3"}
	service.newID = func() string { id := ids[0]; ids = ids[1:]; return id }
	scope := capability.ScopeReference{Kind: "tenant", ID: "7"}

	v1 := testLifecyclePolicy("1", 5)
	v2 := testLifecyclePolicy("2", 2)
	for _, policy := range []Policy{v1, v2} {
		registered, registerErr := service.RegisterVersion(ctx, RegisterVersionRequest{Scope: scope, Policy: policy, ActorType: "user", ActorID: "operator", Reason: "reviewed immutable policy"})
		if registerErr != nil || registered.Policy.Version != policy.Version || registered.ActorID != "operator" || registered.RegisteredAt != now {
			t.Fatalf("register %s = %#v, %v", policy.Version, registered, registerErr)
		}
	}
	if _, err := service.RegisterVersion(ctx, RegisterVersionRequest{Scope: scope, Policy: v1, ActorType: "user", ActorID: "operator", Reason: "duplicate"}); !errors.Is(err, ErrPolicyVersionExists) {
		t.Fatalf("immutable duplicate error = %v", err)
	}

	first, err := service.Activate(ctx, ActivateRequest{Scope: scope, PolicyID: v1.ID, Version: "1", ExpectedRevision: 0, ActorType: "user", ActorID: "operator", Reason: "initial approval"})
	if err != nil || first.APIVersion != LifecycleAPIVersion || first.Lifecycle.Revision != 1 || first.Event.ID != "activate-1" || first.Policy.MaximumItems != 5 {
		t.Fatalf("first activation = %#v, %v", first, err)
	}
	if _, err := service.Activate(ctx, ActivateRequest{Scope: scope, PolicyID: v1.ID, Version: "2", ExpectedRevision: 0, ActorType: "user", ActorID: "operator", Reason: "stale"}); !errors.Is(err, ErrPolicyRevision) {
		t.Fatalf("stale activation error = %v", err)
	}
	second, err := service.Activate(ctx, ActivateRequest{Scope: scope, PolicyID: v1.ID, Version: "2", ExpectedRevision: 1, ActorType: "user", ActorID: "operator", Reason: "narrow item budget"})
	if err != nil || second.Lifecycle.ActiveVersion != "2" || second.Lifecycle.PreviousVersion != "1" || second.Lifecycle.Revision != 2 || second.Event.FromVersion != "1" {
		t.Fatalf("second activation = %#v, %v", second, err)
	}

	revoked, err := service.Revoke(ctx, RevokeRequest{Scope: scope, PolicyID: v1.ID, ExpectedRevision: 2, ActorType: "user", ActorID: "operator", Reason: "source no longer approved"})
	if err != nil || revoked.Lifecycle.State != LifecycleRevoked || revoked.Lifecycle.Revision != 3 || revoked.Event.ID != "revoke-3" {
		t.Fatalf("revocation = %#v, %v", revoked, err)
	}
	if _, _, err := service.ResolveActive(ctx, scope, v1.ID); !errors.Is(err, ErrPolicyRevoked) {
		t.Fatalf("revoked resolve error = %v", err)
	}
	if _, err := service.Revoke(ctx, RevokeRequest{Scope: scope, PolicyID: v1.ID, ExpectedRevision: 2, ActorType: "user", ActorID: "operator", Reason: "stale"}); !errors.Is(err, ErrPolicyRevision) {
		t.Fatalf("stale revocation error = %v", err)
	}
	events, err := service.ListEvents(ctx, scope, v1.ID)
	if err != nil || len(events) != 3 || events[2].ToState != LifecycleRevoked {
		t.Fatalf("events = %#v, %v", events, err)
	}
	versions, _ := service.ListVersions(ctx, scope, v1.ID)
	if len(versions) != 2 || versions[0].Policy.Version != "1" || versions[1].Policy.Version != "2" {
		t.Fatalf("versions = %#v", versions)
	}
}

func TestLifecycleValidationFailsClosedAtExactBounds(t *testing.T) {
	service, _ := NewLifecycleService(NewMemoryLifecycleStore())
	scope := capability.ScopeReference{Kind: "tenant", ID: "7"}
	base := RegisterVersionRequest{Scope: scope, Policy: testLifecyclePolicy("1", MaximumItems), ActorType: "user", ActorID: "operator", Reason: "reviewed"}
	if _, err := service.RegisterVersion(context.Background(), base); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*RegisterVersionRequest)
	}{
		{"items", func(r *RegisterVersionRequest) { r.Policy.Version = "items"; r.Policy.MaximumItems = MaximumItems + 1 }},
		{"retention", func(r *RegisterVersionRequest) { r.Policy.Version = "retention"; r.Policy.RetentionDays = 3651 }},
		{"method", func(r *RegisterVersionRequest) {
			r.Policy.Version = "method"
			r.Policy.Sources[0].Methods = []string{http.MethodPost}
		}},
		{"implicit-method", func(r *RegisterVersionRequest) {
			r.Policy.Version = "implicit-method"
			r.Policy.Sources[0].Methods = nil
		}},
		{"implicit-path", func(r *RegisterVersionRequest) {
			r.Policy.Version = "implicit-path"
			r.Policy.Sources[0].PathPrefixes = nil
		}},
		{"path", func(r *RegisterVersionRequest) {
			r.Policy.Version = "path"
			r.Policy.Sources[0].PathPrefixes = []string{"/v1/../admin"}
		}},
		{"outreach", func(r *RegisterVersionRequest) {
			r.Policy.Version = "outreach"
			r.Policy.Outreach = &OutreachPolicy{Enabled: true, ApprovalPolicy: "review", MaximumBytes: 20001}
		}},
		{"disabled", func(r *RegisterVersionRequest) { r.Policy.Version = "disabled"; r.Policy.Enabled = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.Policy = *clonePolicy(&base.Policy)
			test.mutate(&request)
			if _, err := service.RegisterVersion(context.Background(), request); err == nil {
				t.Fatal("invalid policy version was registered")
			}
		})
	}
}

func testLifecyclePolicy(version string, maximumItems int) Policy {
	return Policy{ID: "community-research", Version: version, Enabled: true, MaximumItems: maximumItems, RetentionDays: 30,
		Sources:  []PolicySource{{Host: "www.reddit.com", PathPrefixes: []string{"/r/kubernetes"}, Methods: []string{http.MethodGet}}},
		Outreach: &OutreachPolicy{Enabled: true, ApprovalPolicy: "human-review", MaximumBytes: 2000}}
}
