package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestSQLiteActionCredentialLeaseRedemptionIsAtomicDurableAndBounded(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "credential-redemption.db")
	primary, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	replica, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	db := openTestSQLiteDB(t, path)
	request, _ := storetest.ActionCredentialLeaseFixture(t, ctx, primary, "nonce-sqlite-concurrent-0001")
	cutoff := time.Now().UTC().Add(-time.Minute)
	for index := 0; index < 130; index++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO action_credential_lease_redemptions
			(issuer,audience,nonce_digest,scope_kind,scope_id,run_id,action_call_id,action_lease_id,deployment_id,binding_id,binding_revision,transport,credential_fields_digest,expires_at,consumed_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "expired", "execution-host", fmt.Sprintf("%064x", index), "tenant", "expired", "run", "call", "lease", "agent", "binding", 1, "tool", fmt.Sprintf("%064x", index), cutoff, cutoff); err != nil {
			t.Fatal(err)
		}
	}

	stores := []*sqlite.Store{primary, replica}
	results := make(chan error, len(stores))
	var wait sync.WaitGroup
	for _, store := range stores {
		wait.Add(1)
		go func(store *sqlite.Store) {
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
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM action_credential_lease_redemptions WHERE expires_at <= ?`, time.Now().UTC()).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 2 {
		t.Fatalf("bounded cleanup left %d expired rows, want 2", expired)
	}
	var nonceDigest, fieldsDigest string
	if err := db.QueryRowContext(ctx, `SELECT nonce_digest, credential_fields_digest FROM action_credential_lease_redemptions
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
	restarted, err := sqlite.NewSQLiteStore(path)
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
		drift func(*testing.T, context.Context, *sqlite.Store, *sql.DB, *skill.Binding, ActionCredentialLeaseRedemptionRequest)
	}{
		{"action lease owner", func(t *testing.T, ctx context.Context, _ *sqlite.Store, db *sql.DB, _ *skill.Binding, request ActionCredentialLeaseRedemptionRequest) {
			var payload string
			if err := db.QueryRowContext(ctx, `SELECT payload FROM action_calls WHERE scope_kind=? AND scope_id=? AND id=?`, request.Lease.Scope.Kind, request.Lease.Scope.ID, request.Lease.ActionCallID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			call, err := sqlstore.DecodeActionCall(payload)
			if err != nil {
				t.Fatal(err)
			}
			call.LeaseOwner = "other-worker"
			updated, _ := json.Marshal(call)
			if _, err := db.ExecContext(ctx, `UPDATE action_calls SET lease_owner=?, payload=? WHERE scope_kind=? AND scope_id=? AND id=?`, call.LeaseOwner, string(updated), call.Scope.Kind, call.Scope.ID, call.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"assigned agent", func(t *testing.T, ctx context.Context, _ *sqlite.Store, db *sql.DB, _ *skill.Binding, request ActionCredentialLeaseRedemptionRequest) {
			var payload string
			if err := db.QueryRowContext(ctx, `SELECT payload FROM agent_runs WHERE scope_kind=? AND scope_id=? AND id=?`, request.Lease.Scope.Kind, request.Lease.Scope.ID, request.Lease.RunID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			run, err := sqlstore.DecodeAgentRun(payload)
			if err != nil {
				t.Fatal(err)
			}
			run.AssignedAgentID = "other-agent"
			updated, _ := json.Marshal(run)
			if _, err := db.ExecContext(ctx, `UPDATE agent_runs SET assigned_agent_id=?, payload=? WHERE scope_kind=? AND scope_id=? AND id=?`, run.AssignedAgentID, string(updated), run.Scope.Kind, run.Scope.ID, run.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"binding revision", func(t *testing.T, ctx context.Context, store *sqlite.Store, _ *sql.DB, binding *skill.Binding, _ ActionCredentialLeaseRedemptionRequest) {
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
			path := filepath.Join(t.TempDir(), "authority.db")
			store, err := sqlite.NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			db := openTestSQLiteDB(t, path)
			request, binding := storetest.ActionCredentialLeaseFixture(t, ctx, store, "nonce-sqlite-drift-"+uuid.NewString())
			test.drift(t, ctx, store, db, binding, request)
			if err := store.RedeemActionCredentialLease(ctx, request); !errors.Is(err, ErrActionCredentialLeaseMismatch) {
				t.Fatalf("authority drift error = %v, want %v", err, ErrActionCredentialLeaseMismatch)
			}
			var consumed int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM action_credential_lease_redemptions WHERE issuer=? AND audience=?`, request.Lease.Issuer, request.Lease.Audience).Scan(&consumed); err != nil {
				t.Fatal(err)
			}
			if consumed != 0 {
				t.Fatalf("authority failure consumed %d nonces", consumed)
			}
		})
	}
}

var _ ActionCredentialLeaseRedeemer = (*sqlite.Store)(nil)
