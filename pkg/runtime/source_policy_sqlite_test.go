package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/source"
)

func TestSQLiteSourcePolicyLifecycleIsScopedAuditedAndRestartDurable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "openseal.db")
	primary, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	otherScope := capability.ScopeReference{Kind: "tenant", ID: "two"}
	service, err := source.NewLifecycleService(primary)
	if err != nil {
		t.Fatal(err)
	}

	v1 := registerSourcePolicyVersion(t, ctx, service, scope, "1")
	if _, err := service.RegisterVersion(ctx, source.RegisterVersionRequest{
		Scope: scope, Policy: *v1.Policy, ActorType: "user", ActorID: "operator", Reason: "duplicate",
	}); !errors.Is(err, source.ErrPolicyVersionExists) {
		t.Fatalf("duplicate registration error = %v, want %v", err, source.ErrPolicyVersionExists)
	}
	activated, err := service.Activate(ctx, source.ActivateRequest{
		Scope: scope, PolicyID: v1.Policy.ID, Version: "1", ExpectedRevision: 0,
		ActorType: "user", ActorID: "operator", Reason: "reviewed initial source authority",
	})
	if err != nil {
		t.Fatal(err)
	}
	if activated.Lifecycle.Revision != 1 || activated.Lifecycle.State != source.LifecycleActive {
		t.Fatalf("initial lifecycle = %#v", activated.Lifecycle)
	}
	if _, err := service.GetLifecycle(ctx, otherScope, v1.Policy.ID); !errors.Is(err, source.ErrPolicyNotFound) {
		t.Fatalf("cross-scope lookup error = %v, want %v", err, source.ErrPolicyNotFound)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}

	primary, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	service, _ = source.NewLifecycleService(primary)
	resolved, lifecycle, err := service.ResolveActive(ctx, scope, v1.Policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Version != "1" || lifecycle.Revision != 1 {
		t.Fatalf("resolved after restart = policy %q lifecycle %#v", resolved.Version, lifecycle)
	}

	registerSourcePolicyVersion(t, ctx, service, scope, "2")
	registerSourcePolicyVersion(t, ctx, service, scope, "3")
	replica, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	replicaService, _ := source.NewLifecycleService(replica)
	services := []*source.LifecycleService{service, replicaService}
	versions := []string{"2", "3"}
	var winners atomic.Int32
	var wait sync.WaitGroup
	for index := range services {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, activateErr := services[index].Activate(ctx, source.ActivateRequest{
				Scope: scope, PolicyID: v1.Policy.ID, Version: versions[index], ExpectedRevision: 1,
				ActorType: "user", ActorID: "operator", Reason: "concurrent reviewed replacement",
			})
			if activateErr == nil {
				winners.Add(1)
				return
			}
			if !errors.Is(activateErr, source.ErrPolicyRevision) {
				t.Errorf("concurrent activation error = %v", activateErr)
			}
		}(index)
	}
	wait.Wait()
	if winners.Load() != 1 {
		t.Fatalf("concurrent activation winners = %d, want 1", winners.Load())
	}

	current, err := service.GetLifecycle(ctx, scope, v1.Policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 2 || current.State != source.LifecycleActive || current.PreviousVersion != "1" {
		t.Fatalf("concurrent lifecycle = %#v", current)
	}
	if _, err := service.Revoke(ctx, source.RevokeRequest{
		Scope: scope, PolicyID: v1.Policy.ID, ExpectedRevision: 1,
		ActorType: "user", ActorID: "operator", Reason: "stale revocation",
	}); !errors.Is(err, source.ErrPolicyRevision) {
		t.Fatalf("stale revoke error = %v, want %v", err, source.ErrPolicyRevision)
	}
	revoked, err := service.Revoke(ctx, source.RevokeRequest{
		Scope: scope, PolicyID: v1.Policy.ID, ExpectedRevision: 2,
		ActorType: "user", ActorID: "operator", Reason: "source authority withdrawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Lifecycle.Revision != 3 || revoked.Lifecycle.State != source.LifecycleRevoked {
		t.Fatalf("revoked lifecycle = %#v", revoked.Lifecycle)
	}
	if _, _, err := replicaService.ResolveActive(ctx, scope, v1.Policy.ID); !errors.Is(err, source.ErrPolicyRevoked) {
		t.Fatalf("replica resolution after revoke = %v, want %v", err, source.ErrPolicyRevoked)
	}
	events, err := service.ListEvents(ctx, scope, v1.Policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("event count = %d, want 3", len(events))
	}
	for index, event := range events {
		if event.PolicyRevision != int64(index+1) || event.ActorID != "operator" || event.Reason == "" {
			t.Fatalf("event %d = %#v", index, event)
		}
	}
	versionsAfterRestart, err := service.ListVersions(ctx, scope, v1.Policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versionsAfterRestart) != 3 {
		t.Fatalf("version count = %d, want 3", len(versionsAfterRestart))
	}
}

func registerSourcePolicyVersion(t *testing.T, ctx context.Context, service *source.LifecycleService, scope capability.ScopeReference, version string) *source.PolicyVersion {
	t.Helper()
	registered, err := service.RegisterVersion(ctx, source.RegisterVersionRequest{
		Scope: scope,
		Policy: source.Policy{
			ID: "community-research", Version: version, Enabled: true,
			Sources:      []source.PolicySource{{Host: "community.example", PathPrefixes: []string{"/forum"}, Methods: []string{"GET", "HEAD"}}},
			MaximumItems: 25, RetentionDays: 30, ApprovalPolicy: "read",
		},
		ActorType: "user", ActorID: "operator", Reason: "reviewed source policy version " + version,
	})
	if err != nil {
		t.Fatal(err)
	}
	return registered
}
