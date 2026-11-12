//go:build integration

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestPostgresActionCredentialLeaseRedemptionIsReplicaAtomicAndDurable(t *testing.T) {
	ctx, primary, replica, pgSchema := newActionCredentialLeasePostgresStores(t)
	request, _ := createPostgresActionCredentialLeaseFixture(t, ctx, primary, pgSchema, "nonce-concurrent-00000001")
	cleanupCutoff := time.Now().UTC()
	if _, err := pgSchema.DB.ExecContext(ctx, `INSERT INTO `+pgSchema.Table("action_credential_lease_redemptions")+`
		(issuer,audience,nonce_digest,scope_kind,scope_id,run_id,action_call_id,action_lease_id,deployment_id,binding_id,binding_revision,transport,credential_fields_digest,expires_at,consumed_at)
		SELECT 'expired-issuer','expired-audience',md5(value::text),'tenant','expired','run','action','lease','agent','binding',1,'test',md5('fields'),$1,$1
		FROM generate_series(1,130) AS value`, cleanupCutoff.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	stores := []*postgres.Store{primary, replica}
	errorsFound := make(chan error, len(stores))
	var wait sync.WaitGroup
	for _, store := range stores {
		wait.Add(1)
		go func(store *postgres.Store) {
			defer wait.Done()
			errorsFound <- store.RedeemActionCredentialLease(ctx, request)
		}(store)
	}
	wait.Wait()
	close(errorsFound)
	succeeded, replayed := 0, 0
	for err := range errorsFound {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, models.ErrActionCredentialLeaseReplay):
			replayed++
		default:
			t.Fatalf("redemption error = %v", err)
		}
	}
	if succeeded != 1 || replayed != 1 {
		t.Fatalf("redemptions succeeded=%d replayed=%d, want 1/1", succeeded, replayed)
	}
	var expired int
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+pgSchema.Table("action_credential_lease_redemptions")+`
		WHERE expires_at <= $1`, cleanupCutoff).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 2 {
		t.Fatalf("bounded cleanup left %d expired rows, want 2", expired)
	}
	var nonceDigest, transport, fieldsDigest string
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT nonce_digest, transport, credential_fields_digest FROM `+pgSchema.Table("action_credential_lease_redemptions")+`
		WHERE issuer=$1 AND audience=$2`, request.Lease.Issuer, request.Lease.Audience).Scan(&nonceDigest, &transport, &fieldsDigest); err != nil {
		t.Fatal(err)
	}
	expectedFieldsDigest, err := models.ActionCredentialFieldSelectionDigest(request.CredentialFields)
	if err != nil {
		t.Fatal(err)
	}
	if len(nonceDigest) != 64 || nonceDigest == request.Lease.Nonce || transport != request.Transport || fieldsDigest != expectedFieldsDigest {
		t.Fatalf("safe audit projection nonceDigest=%q transport=%q fieldsDigest=%q", nonceDigest, transport, fieldsDigest)
	}

	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := kerneltest.OpenPostgresOn(t, pgSchema)
	if err := restarted.RedeemActionCredentialLease(ctx, request); !errors.Is(err, models.ErrActionCredentialLeaseReplay) {
		t.Fatalf("restart replay error = %v, want %v", err, models.ErrActionCredentialLeaseReplay)
	}
}

func TestPostgresActionCredentialLeaseAuthorityDriftDoesNotConsumeNonce(t *testing.T) {
	ctx, primary, replica, pgSchema := newActionCredentialLeasePostgresStores(t)
	request, binding := createPostgresActionCredentialLeaseFixture(t, ctx, primary, pgSchema, "nonce-drift-000000000002")
	catalog := skill.NewCatalogWithStore(replica)
	if _, err := catalog.DisableBinding(ctx, skill.DisableBindingRequest{
		Scope: binding.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ExpectedRevision: binding.Revision,
		Actor: skill.BindingActor{Type: "user", ID: "security"}, Reason: "test authority drift",
	}); err != nil {
		t.Fatal(err)
	}
	if err := primary.RedeemActionCredentialLease(ctx, request); !errors.Is(err, models.ErrActionCredentialLeaseMismatch) {
		t.Fatalf("authority drift error = %v, want %v", err, models.ErrActionCredentialLeaseMismatch)
	}
	var consumed int
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+pgSchema.Table("action_credential_lease_redemptions")+`
		WHERE issuer=$1 AND audience=$2`, request.Lease.Issuer, request.Lease.Audience).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != 0 {
		t.Fatalf("authority failure consumed %d nonces", consumed)
	}
}

// newActionCredentialLeasePostgresStores opens two stores on one fresh schema,
// as two replicas would, with the test's own schema connection.
func newActionCredentialLeasePostgresStores(t *testing.T) (context.Context, *postgres.Store, *postgres.Store, storetest.PostgresSchema) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pgSchema := storetest.NewPostgresSchema(t, "openseal_credential_lease_")
	return ctx, kerneltest.OpenPostgresOn(t, pgSchema), kerneltest.OpenPostgresOn(t, pgSchema), pgSchema
}

func createPostgresActionCredentialLeaseFixture(t *testing.T, ctx context.Context, store *postgres.Store, pgSchema storetest.PostgresSchema, nonce string) (models.ActionCredentialLeaseRedemptionRequest, *skill.Binding) {
	t.Helper()
	now := time.Now().UTC().Round(0)
	scope := models.Scope{Kind: "tenant", ID: "tenant-credential-lease"}
	const deploymentID = "research-agent"
	run, err := portfolio.NewService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deploymentID}, AssignedAgentID: deploymentID,
		Goal: "Research safely", Source: models.RunSourceManual,
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
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 1},
			Credentials: []skill.CredentialRequirement{{Name: "reddit", Kind: "reddit-oauth"}},
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
	call := &models.ActionCall{
		ID: uuid.NewString(), Scope: scope, RunID: run.ID, DeploymentID: deploymentID, BindingID: binding.ID, BindingRevision: binding.Revision,
		SkillID: definition.ID, SkillVersion: definition.Version, Action: "read", Status: models.ActionCallStatusRunning,
		Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, CredentialRefs: binding.Credentials,
		Attempt: 1, MaxAttempts: 3, AvailableAt: now, LeaseOwner: "runtime-worker", LeaseExpiresAt: &expires,
		Revision: 2, CreatedAt: now, UpdatedAt: now,
	}
	if err := call.Validate(); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `INSERT INTO `+pgSchema.Table("action_calls")+`
		(id,scope_kind,scope_id,run_id,status,available_at,lease_owner,lease_expires_at,revision,created_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`, call.ID, scope.Kind, scope.ID, run.ID, call.Status,
		call.AvailableAt, call.LeaseOwner, call.LeaseExpiresAt, call.Revision, call.CreatedAt, string(payload)); err != nil {
		t.Fatal(err)
	}
	fields := map[string][]string{"reddit": {"oauth.access_token"}}
	lease, err := models.NewActionCredentialLease(models.CreateActionCredentialLeaseRequest{
		TenantID: scope.ID, Call: call, Run: run, CredentialFields: fields, Transport: definition.Transport.Endpoint,
		Issuer: "credential-authority", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	return models.ActionCredentialLeaseRedemptionRequest{Lease: *lease, Transport: definition.Transport.Endpoint, CredentialFields: fields}, binding
}
