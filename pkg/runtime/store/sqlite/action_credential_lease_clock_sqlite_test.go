package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// Store test: the SQLite store's lease clock resolution
// (canonicalSQLiteLeaseTime) and its expiry check.

func TestSQLiteActionCredentialLeaseClockResolutionAndExpiry(t *testing.T) {
	t.Run("canonicalizes sub-millisecond issuance", func(t *testing.T) {
		millisecond := time.Date(2026, time.July, 22, 15, 0, 0, 123_000_000, time.UTC)
		if got := canonicalSQLiteLeaseTime(millisecond.Add(999 * time.Microsecond)); !got.Equal(millisecond) {
			t.Fatalf("canonical SQLite time = %s, want %s", got, millisecond)
		}
	})

	t.Run("expired lease fails closed without consuming nonce", func(t *testing.T) {
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "expired.db")
		store, err := New(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		db := openTestSQLiteDB(t, path)
		request, _ := storetest.ActionCredentialLeaseFixture(t, ctx, store, "nonce-sqlite-expired-0001")
		request.Lease.IssuedAt = time.Now().UTC().Add(-2 * time.Minute)
		request.Lease.ExpiresAt = time.Now().UTC().Add(-time.Minute)

		if err := store.RedeemActionCredentialLease(ctx, request); !errors.Is(err, models.ErrActionCredentialLeaseExpired) {
			t.Fatalf("expired lease error = %v, want %v", err, models.ErrActionCredentialLeaseExpired)
		}
		var consumed int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM action_credential_lease_redemptions`).Scan(&consumed); err != nil {
			t.Fatal(err)
		}
		if consumed != 0 {
			t.Fatalf("expired lease consumed %d nonces", consumed)
		}
	})
}
