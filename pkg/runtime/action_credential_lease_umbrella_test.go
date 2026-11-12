package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

// leaseUmbrellaCase is one redemption of a lease issued for a run.
type leaseUmbrellaCase struct {
	name        string
	owner       models.ObjectiveOwner
	assigned    string
	actingUser  string
	reference   string
	host        bool
	leaseActing *string
	want        error
}

func leaseUmbrellaCases() []leaseUmbrellaCase {
	other := "8"
	agent := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "research-agent"}
	team := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research-team"}
	return []leaseUmbrellaCase{
		{name: "agent stored binding", owner: agent, assigned: agent.ID, reference: "opaque://tenant/reddit"},
		// O1: a Team run redeems its Team's host binding.
		{name: "team host binding", owner: team, assigned: "member-agent", actingUser: "7", reference: "opaque://tenant/reddit", host: true},
		{name: "team stored binding", owner: team, assigned: "member-agent", reference: "opaque://tenant/reddit"},
		{name: "person ref with acting user", owner: team, assigned: "member-agent", actingUser: "7", reference: skill.PersonCredentialReferenceID, host: true},
		{name: "acting user mismatch", owner: agent, assigned: agent.ID, actingUser: "7", leaseActing: &other, reference: "opaque://tenant/reddit", want: models.ErrActionCredentialLeaseMismatch},
		{name: "person ref without acting user", owner: agent, assigned: agent.ID, reference: skill.PersonCredentialReferenceID, host: true, want: models.ErrActionCredentialLeaseMismatch},
		// D7: a run delegated to another Agent redeems nothing, not even the
		// owner's binding.
		{name: "delegated run", owner: agent, assigned: "helper-agent", reference: "opaque://tenant/reddit", want: models.ErrActionCredentialLeaseMismatch},
	}
}

type leaseCallInserter func(*testing.T, context.Context, *models.ActionCall)

func leaseUmbrellaRequest(t *testing.T, ctx context.Context, store models.KernelStore, catalogStore skill.CatalogStore, insert leaseCallInserter, test leaseUmbrellaCase) models.ActionCredentialLeaseRedemptionRequest {
	t.Helper()
	now := time.Now().UTC().Round(0)
	scope := models.Scope{Kind: "tenant", ID: "tenant-lease-" + uuid.NewString()[:8]}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: test.owner, AssignedAgentID: test.assigned, ActingUser: test.actingUser,
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
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, Credentials: []skill.CredentialRequirement{{Name: "reddit", Kind: "reddit-oauth"}},
		}},
	}
	binding := &skill.Binding{
		ID: "reddit-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: test.owner.ID,
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: test.reference}}, Revision: 1,
	}
	if !test.host {
		catalog := skill.NewCatalogWithStore(catalogStore)
		if existing, _ := catalog.GetDefinition(ctx, definition.ID, definition.Version); existing == nil {
			if err := catalog.Register(ctx, definition); err != nil {
				t.Fatal(err)
			}
		}
		if err := catalog.Bind(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}
	expires := now.Add(2 * time.Minute)
	call := &models.ActionCall{
		ID: uuid.NewString(), Scope: scope, RunID: run.ID, DeploymentID: binding.DeploymentID, BindingID: binding.ID, BindingRevision: binding.Revision,
		SkillID: definition.ID, SkillVersion: definition.Version, Action: "read", Status: models.ActionCallStatusRunning,
		Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, CredentialRefs: binding.Credentials,
		Attempt: 1, MaxAttempts: 3, AvailableAt: now, LeaseOwner: "runtime-worker", LeaseExpiresAt: &expires,
		Revision: 2, CreatedAt: now, UpdatedAt: now,
	}
	insert(t, ctx, call)
	fields := map[string][]string{"reddit": {"oauth.access_token"}}
	lease, err := models.NewActionCredentialLease(models.CreateActionCredentialLeaseRequest{
		TenantID: scope.ID, Call: call, Run: run, CredentialFields: fields, Transport: definition.Transport.Endpoint,
		Issuer: "credential-authority", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.ActingUser != test.actingUser {
		t.Fatalf("lease acting user = %q, want the run's %q", lease.ActingUser, test.actingUser)
	}
	if test.leaseActing != nil {
		lease.ActingUser = *test.leaseActing
	}
	request := models.ActionCredentialLeaseRedemptionRequest{Lease: *lease, Transport: definition.Transport.Endpoint, CredentialFields: fields}
	if test.host {
		request.HostBinding = binding
	}
	return request
}

func TestSQLiteActionCredentialLeaseRedeemsOnlyForTheRunUmbrellaAndPerson(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lease-umbrella.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := openTestSQLiteDB(t, path)
	defer store.Close()
	insert := func(t *testing.T, ctx context.Context, call *models.ActionCall) {
		payload, err := json.Marshal(call)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sqlDB.ExecContext(ctx, `INSERT INTO action_calls
			(id,scope_kind,scope_id,run_id,status,available_at,lease_owner,lease_expires_at,revision,created_at,payload)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, call.ID, call.Scope.Kind, call.Scope.ID, call.RunID, call.Status, call.AvailableAt,
			call.LeaseOwner, call.LeaseExpiresAt, call.Revision, call.CreatedAt, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range leaseUmbrellaCases() {
		t.Run(test.name, func(t *testing.T) {
			request := leaseUmbrellaRequest(t, ctx, store, store, insert, test)
			if err := store.RedeemActionCredentialLease(ctx, request); !errors.Is(err, test.want) {
				t.Fatalf("redeem error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPostgresActionCredentialLeaseRedeemsOnlyForTheRunUmbrellaAndPerson(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "openseal_lease_umbrella_")
	dsn := pgSchema.DSN
	ctx := context.Background()
	store, err := postgres.New(ctx, dsn, postgres.WithSchema(pgSchema.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	insert := func(t *testing.T, ctx context.Context, call *models.ActionCall) {
		payload, err := json.Marshal(call)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pgSchema.DB.ExecContext(ctx, `INSERT INTO `+pgSchema.Table("action_calls")+`
			(id,scope_kind,scope_id,run_id,status,available_at,lease_owner,lease_expires_at,revision,created_at,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`, call.ID, call.Scope.Kind, call.Scope.ID, call.RunID, call.Status,
			call.AvailableAt, call.LeaseOwner, call.LeaseExpiresAt, call.Revision, call.CreatedAt, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range leaseUmbrellaCases() {
		t.Run(test.name, func(t *testing.T) {
			request := leaseUmbrellaRequest(t, ctx, store, store, insert, test)
			if err := store.RedeemActionCredentialLease(ctx, request); !errors.Is(err, test.want) {
				t.Fatalf("redeem error = %v, want %v", err, test.want)
			}
		})
	}
}
