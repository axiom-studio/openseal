package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestSQLiteActionCredentialLeaseRedemptionIsAtomicDurableAndBounded(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "credential-redemption.db")
	primary, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	replica, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := createSQLiteActionCredentialLeaseFixture(t, ctx, primary, "nonce-sqlite-concurrent-0001")
	cutoff := time.Now().UTC().Add(-time.Minute)
	for index := 0; index < 130; index++ {
		if _, err := primary.db.ExecContext(ctx, `INSERT INTO action_credential_lease_redemptions
			(issuer,audience,nonce_digest,scope_kind,scope_id,run_id,action_call_id,action_lease_id,deployment_id,binding_id,binding_revision,transport,credential_fields_digest,expires_at,consumed_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "expired", "execution-host", fmt.Sprintf("%064x", index), "tenant", "expired", "run", "call", "lease", "agent", "binding", 1, "tool", fmt.Sprintf("%064x", index), cutoff, cutoff); err != nil {
			t.Fatal(err)
		}
	}

	stores := []*SQLiteStore{primary, replica}
	results := make(chan error, len(stores))
	var wait sync.WaitGroup
	for _, store := range stores {
		wait.Add(1)
		go func(store *SQLiteStore) {
			defer wait.Done()
			results <- store.RedeemActionCredentialLease(ctx, request)
		}(store)
	}
	wait.Wait()
	close(results)
	succeeded, replayed := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrActionCredentialLeaseReplay):
			replayed++
		default:
			t.Fatalf("redemption error = %v", err)
		}
	}
	if succeeded != 1 || replayed != 1 {
		t.Fatalf("redemptions succeeded=%d replayed=%d, want 1/1", succeeded, replayed)
	}
	var expired int
	if err := primary.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM action_credential_lease_redemptions WHERE expires_at <= ?`, time.Now().UTC()).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 2 {
		t.Fatalf("bounded cleanup left %d expired rows, want 2", expired)
	}
	var nonceDigest, fieldsDigest string
	if err := primary.db.QueryRowContext(ctx, `SELECT nonce_digest, credential_fields_digest FROM action_credential_lease_redemptions
		WHERE issuer=? AND audience=?`, request.Lease.Issuer, request.Lease.Audience).Scan(&nonceDigest, &fieldsDigest); err != nil {
		t.Fatal(err)
	}
	if len(nonceDigest) != 64 || nonceDigest == request.Lease.Nonce || len(fieldsDigest) != 64 || fieldsDigest == "oauth.access_token" {
		t.Fatalf("unsafe durable projection nonce=%q fields=%q", nonceDigest, fieldsDigest)
	}

	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.RedeemActionCredentialLease(ctx, request); !errors.Is(err, ErrActionCredentialLeaseReplay) {
		t.Fatalf("restart replay error = %v, want %v", err, ErrActionCredentialLeaseReplay)
	}
}

func TestSQLiteActionCredentialLeaseAuthorityDriftDoesNotConsumeNonce(t *testing.T) {
	for _, test := range []struct {
		name  string
		drift func(*testing.T, context.Context, *SQLiteStore, *skill.Binding, ActionCredentialLeaseRedemptionRequest)
	}{
		{"action lease owner", func(t *testing.T, ctx context.Context, store *SQLiteStore, _ *skill.Binding, request ActionCredentialLeaseRedemptionRequest) {
			var payload string
			if err := store.db.QueryRowContext(ctx, `SELECT payload FROM action_calls WHERE scope_kind=? AND scope_id=? AND id=?`, request.Lease.Scope.Kind, request.Lease.Scope.ID, request.Lease.ActionCallID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			call, err := decodeActionCall(payload)
			if err != nil {
				t.Fatal(err)
			}
			call.LeaseOwner = "other-worker"
			updated, _ := json.Marshal(call)
			if _, err := store.db.ExecContext(ctx, `UPDATE action_calls SET lease_owner=?, payload=? WHERE scope_kind=? AND scope_id=? AND id=?`, call.LeaseOwner, string(updated), call.Scope.Kind, call.Scope.ID, call.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"assigned agent", func(t *testing.T, ctx context.Context, store *SQLiteStore, _ *skill.Binding, request ActionCredentialLeaseRedemptionRequest) {
			var payload string
			if err := store.db.QueryRowContext(ctx, `SELECT payload FROM agent_runs WHERE scope_kind=? AND scope_id=? AND id=?`, request.Lease.Scope.Kind, request.Lease.Scope.ID, request.Lease.RunID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			run, err := decodeAgentRun(payload)
			if err != nil {
				t.Fatal(err)
			}
			run.AssignedAgentID = "other-agent"
			updated, _ := json.Marshal(run)
			if _, err := store.db.ExecContext(ctx, `UPDATE agent_runs SET assigned_agent_id=?, payload=? WHERE scope_kind=? AND scope_id=? AND id=?`, run.AssignedAgentID, string(updated), run.Scope.Kind, run.Scope.ID, run.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"binding revision", func(t *testing.T, ctx context.Context, store *SQLiteStore, binding *skill.Binding, _ ActionCredentialLeaseRedemptionRequest) {
			catalog := skill.NewCatalogWithStore(store)
			if _, err := catalog.DisableBinding(ctx, skill.DisableBindingRequest{
				Scope: binding.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ExpectedRevision: binding.Revision,
				Actor: skill.BindingActor{Type: "user", ID: "security"}, Reason: "test authority drift",
			}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "authority.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			request, binding := createSQLiteActionCredentialLeaseFixture(t, ctx, store, "nonce-sqlite-drift-"+uuid.NewString())
			test.drift(t, ctx, store, binding, request)
			if err := store.RedeemActionCredentialLease(ctx, request); !errors.Is(err, ErrActionCredentialLeaseMismatch) {
				t.Fatalf("authority drift error = %v, want %v", err, ErrActionCredentialLeaseMismatch)
			}
			var consumed int
			if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM action_credential_lease_redemptions WHERE issuer=? AND audience=?`, request.Lease.Issuer, request.Lease.Audience).Scan(&consumed); err != nil {
				t.Fatal(err)
			}
			if consumed != 0 {
				t.Fatalf("authority failure consumed %d nonces", consumed)
			}
		})
	}
}

func TestSQLiteActionCredentialLeaseClockResolutionAndExpiry(t *testing.T) {
	t.Run("canonicalizes sub-millisecond issuance", func(t *testing.T) {
		millisecond := time.Date(2026, time.July, 22, 15, 0, 0, 123_000_000, time.UTC)
		if got := canonicalSQLiteLeaseTime(millisecond.Add(999 * time.Microsecond)); !got.Equal(millisecond) {
			t.Fatalf("canonical SQLite time = %s, want %s", got, millisecond)
		}
	})

	t.Run("expired lease fails closed without consuming nonce", func(t *testing.T) {
		ctx := context.Background()
		store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "expired.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		request, _ := createSQLiteActionCredentialLeaseFixture(t, ctx, store, "nonce-sqlite-expired-0001")
		request.Lease.IssuedAt = time.Now().UTC().Add(-2 * time.Minute)
		request.Lease.ExpiresAt = time.Now().UTC().Add(-time.Minute)

		if err := store.RedeemActionCredentialLease(ctx, request); !errors.Is(err, ErrActionCredentialLeaseExpired) {
			t.Fatalf("expired lease error = %v, want %v", err, ErrActionCredentialLeaseExpired)
		}
		var consumed int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM action_credential_lease_redemptions`).Scan(&consumed); err != nil {
			t.Fatal(err)
		}
		if consumed != 0 {
			t.Fatalf("expired lease consumed %d nonces", consumed)
		}
	})
}

func createSQLiteActionCredentialLeaseFixture(t *testing.T, ctx context.Context, store *SQLiteStore, nonce string) (ActionCredentialLeaseRedemptionRequest, *skill.Binding) {
	t.Helper()
	now := time.Now().UTC().Round(0)
	scope := Scope{Kind: "tenant", ID: "tenant-credential-lease"}
	const deploymentID = "research-agent"
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: deploymentID}, AssignedAgentID: deploymentID,
		Goal: "Research safely", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := &skill.Definition{
		ID: "reddit.reader", Version: "1.0.0", Name: "Reddit Reader",
		Transport: skill.TransportReference{Kind: "tool", Endpoint: "reddit_http"},
		Actions: map[string]skill.Action{"read": {
			Name: "read", Description: "Read Reddit posts", InputSchema: map[string]interface{}{"type": "object"},
			Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, Credentials: []skill.CredentialRequirement{{Name: "reddit", Kind: "reddit-oauth"}},
		}},
	}
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{
		ID: "reddit-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "opaque://tenant/reddit"}}, Revision: 1,
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	expires := now.Add(2 * time.Minute)
	call := &ActionCall{
		ID: uuid.NewString(), Scope: scope, RunID: run.ID, DeploymentID: deploymentID, BindingID: binding.ID, BindingRevision: binding.Revision,
		SkillID: definition.ID, SkillVersion: definition.Version, Action: "read", Status: ActionCallStatusRunning,
		Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, CredentialRefs: binding.Credentials,
		Attempt: 1, MaxAttempts: 3, AvailableAt: now, LeaseOwner: "runtime-worker", LeaseExpiresAt: &expires,
		Revision: 2, CreatedAt: now, UpdatedAt: now,
	}
	payload, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO action_calls
		(id,scope_kind,scope_id,run_id,status,available_at,lease_owner,lease_expires_at,revision,created_at,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, call.ID, scope.Kind, scope.ID, run.ID, call.Status, call.AvailableAt,
		call.LeaseOwner, call.LeaseExpiresAt, call.Revision, call.CreatedAt, string(payload)); err != nil {
		t.Fatal(err)
	}
	fields := map[string][]string{"reddit": {"oauth.access_token"}}
	lease, err := NewActionCredentialLease(CreateActionCredentialLeaseRequest{
		TenantID: scope.ID, Call: call, Run: run, CredentialFields: fields, Transport: definition.Transport.Endpoint,
		Issuer: "credential-authority", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ActionCredentialLeaseRedemptionRequest{Lease: *lease, Transport: definition.Transport.Endpoint, CredentialFields: fields}, binding
}

var _ ActionCredentialLeaseRedeemer = (*SQLiteStore)(nil)
