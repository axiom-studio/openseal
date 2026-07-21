package runtime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const actionCredentialLeaseCleanupBatch = 128

var _ ActionCredentialLeaseRedeemer = (*PostgresStore)(nil)

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
func (s *PostgresStore) RedeemActionCredentialLease(ctx context.Context, request ActionCredentialLeaseRedemptionRequest) error {
	if s == nil || s.db == nil {
		return errors.New("PostgreSQL store is not configured")
	}
	request.Lease.Credentials = cloneActionCredentialFieldReferences(request.Lease.Credentials)
	request.CredentialFields = cloneActionCredentialFields(request.CredentialFields)
	if err := request.Lease.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(request.Transport) == "" {
		return ErrActionCredentialLeaseMismatch
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Keep this lock order stable with action execution: ActionCall, AgentRun,
	// then the independently managed SkillBinding.
	call, err := s.getActionByID(ctx, tx, request.Lease.Scope, request.Lease.ActionCallID, true)
	if errors.Is(err, ErrActionNotFound) {
		return ErrActionCredentialLeaseMismatch
	}
	if err != nil {
		return err
	}
	var runPayload string
	err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_runs")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`, call.Scope.Kind, call.Scope.ID, call.RunID).Scan(&runPayload)
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
	err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("skill_bindings")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND deployment_id=$3 AND id=$4 FOR UPDATE`,
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

	// Read the database clock only after acquiring every authority lock so time
	// spent waiting for another transaction can never extend a signed lease.
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	now = canonicalLeaseTime(now)
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
	credentialFieldsDigest, err := digestActionCredentialFieldSelection(request.CredentialFields)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM `+s.table("action_credential_lease_redemptions")+`
		WHERE (issuer, audience, nonce_digest) IN (
			SELECT issuer, audience, nonce_digest FROM `+s.table("action_credential_lease_redemptions")+`
			WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2
		)`, now, actionCredentialLeaseCleanupBatch); err != nil {
		return err
	}
	digest := actionCredentialLeaseNonceDigest(request.Lease)
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
		return ErrActionCredentialLeaseReplay
	}
	return tx.Commit()
}

func digestActionCredentialFields(fields []ActionCredentialFieldReference) (string, error) {
	payload, err := json.Marshal(cloneActionCredentialFieldReferences(fields))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func digestActionCredentialFieldSelection(fields map[string][]string) (string, error) {
	type fieldSelection struct {
		Name   string   `json:"name"`
		Fields []string `json:"fields"`
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	selections := make([]fieldSelection, 0, len(names))
	for _, name := range names {
		selected := append([]string(nil), fields[name]...)
		sort.Strings(selected)
		selections = append(selections, fieldSelection{Name: name, Fields: selected})
	}
	payload, err := json.Marshal(selections)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func actionCredentialLeaseNonceDigest(lease ActionCredentialLease) [sha256.Size]byte {
	return sha256.Sum256([]byte(lease.Issuer + "\x00" + lease.Audience + "\x00" + lease.Nonce))
}

func matchCurrentActionCredentialBinding(lease ActionCredentialLease, call *ActionCall, binding *skill.Binding) error {
	if call == nil || binding == nil || binding.Disabled ||
		binding.Scope.Kind != lease.Scope.Kind || binding.Scope.ID != lease.Scope.ID ||
		binding.DeploymentID != lease.BindingOwnerID || binding.ID != lease.BindingID || binding.Revision != lease.BindingRevision ||
		binding.SkillID != lease.SkillID || binding.SkillVersion != lease.SkillVersion ||
		!stringSliceContains(binding.AllowedActions, lease.Action) ||
		riskLevelRank(call.Risk) < 0 || riskLevelRank(binding.MaximumRisk) < riskLevelRank(call.Risk) ||
		!credentialReferencesEqual(binding.Credentials, call.CredentialRefs) {
		return ErrActionCredentialLeaseMismatch
	}
	return nil
}

func credentialReferencesEqual(left, right map[string]skill.CredentialReference) bool {
	if len(left) != len(right) {
		return false
	}
	for name, reference := range left {
		if right[name] != reference {
			return false
		}
	}
	return true
}

func stringSliceContains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func riskLevelRank(value skill.RiskLevel) int {
	switch value {
	case skill.RiskLevelRead:
		return 0
	case skill.RiskLevelWrite:
		return 1
	case skill.RiskLevelExternal:
		return 2
	case skill.RiskLevelProduction:
		return 3
	case skill.RiskLevelDestructive:
		return 4
	default:
		return -1
	}
}
