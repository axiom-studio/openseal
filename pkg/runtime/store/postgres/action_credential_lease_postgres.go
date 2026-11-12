package postgres

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/skill"
)

var _ models.ActionCredentialLeaseRedeemer = (*PostgresStore)(nil)

func (s *PostgresStore) migrateActionCredentialLeaseRedemptions(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("action_credential_lease_redemptions")+` (
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
			binding_revision BIGINT NOT NULL,
			transport TEXT NOT NULL,
			credential_fields_digest TEXT NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			consumed_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (issuer, audience, nonce_digest)
		)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS action_credential_lease_redemptions_expiry_idx ON `+s.table("action_credential_lease_redemptions")+` (expires_at)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (21, 'atomic action credential lease redemption') ON CONFLICT (version) DO NOTHING`)
	return err
}

// RedeemActionCredentialLease atomically checks current durable action, Run,
// and Skill binding authority and consumes the signed nonce. The caller must
// authenticate the signature, tenant, issuer, and audience before invoking
// this method. Secret resolution must happen only after this transaction
// commits successfully.
func (s *PostgresStore) RedeemActionCredentialLease(ctx context.Context, request models.ActionCredentialLeaseRedemptionRequest) error {
	if s == nil || s.db == nil {
		return errors.New("PostgreSQL store is not configured")
	}
	request.Lease.Credentials = models.CloneActionCredentialFieldReferences(request.Lease.Credentials)
	request.CredentialFields = models.CloneActionCredentialFields(request.CredentialFields)
	if err := request.Lease.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(request.Transport) == "" {
		return models.ErrActionCredentialLeaseMismatch
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Submission and upgrade also lock the binding first. Read authority under
	// a shared lock so unrelated calls using the same grant can proceed while
	// binding changes still wait for every authority check to commit.
	var bindingPayload string
	err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("skill_bindings")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND deployment_id=$3 AND id=$4 FOR SHARE`,
		request.Lease.Scope.Kind, request.Lease.Scope.ID, request.Lease.BindingOwnerID, request.Lease.BindingID).Scan(&bindingPayload)
	// A host-supplied binding has no row; the host re-resolved it for this
	// exact run just before redemption.
	var binding skill.Binding
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if request.HostBinding == nil {
			return models.ErrActionCredentialLeaseMismatch
		}
		binding = *request.HostBinding
	case err != nil:
		return err
	case request.HostBinding != nil:
		return models.ErrActionCredentialLeaseMismatch
	default:
		if err := json.Unmarshal([]byte(bindingPayload), &binding); err != nil {
			return err
		}
	}
	// The remaining stable order is ActionCall, then its owning AgentRun.
	call, err := s.getActionByID(ctx, tx, request.Lease.Scope, request.Lease.ActionCallID, true)
	if errors.Is(err, models.ErrActionNotFound) {
		return models.ErrActionCredentialLeaseMismatch
	}
	if err != nil {
		return err
	}
	var runPayload string
	err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_runs")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`, call.Scope.Kind, call.Scope.ID, call.RunID).Scan(&runPayload)
	if errors.Is(err, sql.ErrNoRows) {
		return models.ErrActionCredentialLeaseMismatch
	}
	if err != nil {
		return err
	}
	run, err := sqlstore.DecodeAgentRun(runPayload)
	if err != nil {
		return err
	}
	// Read the database clock only after acquiring every authority lock so time
	// spent waiting for another transaction can never extend a signed lease.
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	now = models.CanonicalLeaseTime(now)
	if now.Before(request.Lease.IssuedAt) || !now.Before(request.Lease.ExpiresAt) || !now.Before(request.Lease.ActionLease.ExpiresAt) {
		return models.ErrActionCredentialLeaseExpired
	}
	if call.LeaseExpiresAt == nil || !now.Before(models.CanonicalLeaseTime(*call.LeaseExpiresAt)) {
		return models.ErrActionCredentialLeaseExpired
	}
	if err := models.MatchActionCredentialLease(request.Lease, call, run, request.Transport, request.CredentialFields); err != nil {
		return err
	}
	if err := models.CheckCurrentActionCredentialBinding(request.Lease, call, &binding); err != nil {
		return err
	}
	if err := models.CheckActionCredentialLeaseUmbrella(request.Lease, run, &binding); err != nil {
		return err
	}
	credentialFieldsDigest, err := models.ActionCredentialFieldSelectionDigest(request.CredentialFields)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM `+s.table("action_credential_lease_redemptions")+`
		WHERE (issuer, audience, nonce_digest) IN (
			SELECT issuer, audience, nonce_digest FROM `+s.table("action_credential_lease_redemptions")+`
			WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2
		)`, now, sqlstore.ActionCredentialLeaseCleanupBatch); err != nil {
		return err
	}
	digest := models.ActionCredentialLeaseNonceDigest(request.Lease)
	result, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("action_credential_lease_redemptions")+`
		(issuer, audience, nonce_digest, scope_kind, scope_id, run_id, action_call_id, action_lease_id,
		 deployment_id, binding_id, binding_revision, transport, credential_fields_digest, expires_at, consumed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT DO NOTHING`,
		request.Lease.Issuer, request.Lease.Audience, hex.EncodeToString(digest[:]), request.Lease.Scope.Kind,
		request.Lease.Scope.ID, request.Lease.RunID, request.Lease.ActionCallID, request.Lease.ActionLease.ID,
		request.Lease.BindingOwnerID, request.Lease.BindingID, request.Lease.BindingRevision, request.Transport,
		credentialFieldsDigest, request.Lease.ExpiresAt, now)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return models.ErrActionCredentialLeaseReplay
	}
	return tx.Commit()
}
