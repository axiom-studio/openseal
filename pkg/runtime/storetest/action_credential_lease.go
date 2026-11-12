package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

// ActionCredentialLeaseFixtureStore is the public store surface
// ActionCredentialLeaseFixture writes through.
type ActionCredentialLeaseFixtureStore interface {
	ActionCallSeedStore
	skill.CatalogStore
	ClaimNextAgentRun(ctx context.Context, claim models.AgentRunClaim) (*models.AgentRun, error)
}

// ActionCredentialLeaseFixture binds a credentialed skill, has
// runtime-worker claim a call of it through the store's commands, and issues
// that call's lease.
func ActionCredentialLeaseFixture(t *testing.T, ctx context.Context, store ActionCredentialLeaseFixtureStore, nonce string) (models.ActionCredentialLeaseRedemptionRequest, *skill.Binding) {
	t.Helper()
	now := time.Now().UTC().Round(0)
	scope := models.Scope{Kind: "tenant", ID: "tenant-credential-lease"}
	const deploymentID = "research-agent"
	// The Run runtime's portfolio.Service builds for a manual request.
	run := QueuedAgentRun(scope, deploymentID, "Research safely", now)
	run.Source = models.RunSourceManual
	if err := store.CreateAgentRun(ctx, run); err != nil {
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
	// The call is a step's nested call, admitted under its claimed Run's
	// lease, which runtime-worker then claims.
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "run-worker", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil {
		t.Fatalf("claim run = %#v, %v", run, err)
	}
	SeedActionCall(t, store, &models.ActionCall{
		ID: uuid.NewString(), Scope: scope, RunID: run.ID, TurnID: "step-1", DeploymentID: deploymentID, BindingID: binding.ID, BindingRevision: binding.Revision,
		SkillID: definition.ID, SkillVersion: definition.Version, Action: "read", Status: models.ActionCallStatusReady, Origin: models.ActionOriginVM,
		Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, CredentialRefs: binding.Credentials,
		MaxAttempts: 3, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	call, err := store.ClaimNextAction(ctx, models.ActionClaim{Scope: scope, WorkerID: "runtime-worker", Now: now, LeaseDuration: 2 * time.Minute})
	if err != nil || call == nil {
		t.Fatalf("claim call = %#v, %v", call, err)
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
