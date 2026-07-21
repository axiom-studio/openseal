package runtime

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func migrateActionCredentialLeaseRedemptionsSQLite(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS action_credential_lease_redemptions (
			issuer TEXT NOT NULL,
			audience TEXT NOT NULL,
			nonce_digest TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			action_call_id TEXT NOT NULL,
			action_lease_id TEXT NOT NULL,
			deployment_id TEXT NOT NULL,
			binding_id TEXT NOT NULL,
			binding_revision INTEGER NOT NULL,
			transport TEXT NOT NULL,
			credential_fields_digest TEXT NOT NULL,
			expires_at DATETIME NOT NULL,
			consumed_at DATETIME NOT NULL,
			PRIMARY KEY (issuer, audience, nonce_digest)
		);
		CREATE INDEX IF NOT EXISTS idx_action_credential_lease_redemptions_expiry
			ON action_credential_lease_redemptions(expires_at);
	`)
	return err
}

// RedeemActionCredentialLease provides the standalone SQLite implementation
// of the same atomic authority boundary as Postgres. BEGIN IMMEDIATE prevents
// ActionCall, Run, or SkillBinding writers from committing between recheck and
// nonce insertion.
func (s *SQLiteStore) RedeemActionCredentialLease(ctx context.Context, request ActionCredentialLeaseRedemptionRequest) error {
	if s == nil || s.db == nil {
		return errors.New("SQLite store is not configured")
	}
	request.Lease.Credentials = cloneActionCredentialFieldReferences(request.Lease.Credentials)
	request.CredentialFields = cloneActionCredentialFields(request.CredentialFields)
	if err := request.Lease.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(request.Transport) == "" {
		return ErrActionCredentialLeaseMismatch
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	call, err := getActionCallFrom(ctx, conn, request.Lease.Scope, `id = ?`, request.Lease.ActionCallID)
	if errors.Is(err, ErrActionNotFound) {
		return ErrActionCredentialLeaseMismatch
	}
	if err != nil {
		return err
	}
	var runPayload string
	err = conn.QueryRowContext(ctx, `SELECT payload FROM agent_runs
		WHERE scope_kind = ? AND scope_id = ? AND id = ?`, call.Scope.Kind, call.Scope.ID, call.RunID).Scan(&runPayload)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrActionCredentialLeaseMismatch
	}
	if err != nil {
		return err
	}
	run, err := decodeAgentRun(runPayload)
	if err != nil {
		return err
	}
	var bindingPayload string
	err = conn.QueryRowContext(ctx, `SELECT payload FROM skill_bindings
		WHERE scope_kind = ? AND scope_id = ? AND deployment_id = ? AND id = ?`,
		call.Scope.Kind, call.Scope.ID, call.DeploymentID, call.BindingID).Scan(&bindingPayload)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrActionCredentialLeaseMismatch
	}
	if err != nil {
		return err
	}
	var binding skill.Binding
	if err := json.Unmarshal([]byte(bindingPayload), &binding); err != nil {
		return err
	}

	var unixMicros int64
	if err := conn.QueryRowContext(ctx, `SELECT CAST((julianday('now') - 2440587.5) * 86400000000 AS INTEGER)`).Scan(&unixMicros); err != nil {
		return err
	}
	now := canonicalLeaseTime(time.UnixMicro(unixMicros))
	if now.Before(request.Lease.IssuedAt) || !now.Before(request.Lease.ExpiresAt) || !now.Before(request.Lease.ActionLease.ExpiresAt) {
		return ErrActionCredentialLeaseExpired
	}
	if call.LeaseExpiresAt == nil || !now.Before(canonicalLeaseTime(*call.LeaseExpiresAt)) {
		return ErrActionCredentialLeaseExpired
	}
	if err := MatchActionCredentialLease(request.Lease, call, run, request.Transport, request.CredentialFields); err != nil {
		return err
	}
	if err := matchCurrentActionCredentialBinding(request.Lease, call, &binding); err != nil {
		return err
	}
	credentialFieldsDigest, err := digestActionCredentialFields(request.Lease.Credentials)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM action_credential_lease_redemptions WHERE rowid IN (
		SELECT rowid FROM action_credential_lease_redemptions WHERE expires_at <= ? ORDER BY expires_at LIMIT ?
	)`, now, actionCredentialLeaseCleanupBatch); err != nil {
		return err
	}
	digest := actionCredentialLeaseNonceDigest(request.Lease)
	result, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO action_credential_lease_redemptions
		(issuer,audience,nonce_digest,scope_kind,scope_id,run_id,action_call_id,action_lease_id,
		 deployment_id,binding_id,binding_revision,transport,credential_fields_digest,expires_at,consumed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, request.Lease.Issuer, request.Lease.Audience, hex.EncodeToString(digest[:]),
		request.Lease.Scope.Kind, request.Lease.Scope.ID, request.Lease.RunID, request.Lease.ActionCallID,
		request.Lease.ActionLease.ID, request.Lease.BindingOwnerID, request.Lease.BindingID, request.Lease.BindingRevision,
		request.Transport, credentialFieldsDigest, request.Lease.ExpiresAt, now)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrActionCredentialLeaseReplay
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}
