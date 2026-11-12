package callback

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestCallbackRegistryPinsExactAdapterAndLifecycle(t *testing.T) {
	ctx := context.Background()
	store := newCallbackTestStore(t)
	catalog := skill.NewCatalogWithStore(store)
	definition, binding := registerCallbackFixture(t, ctx, catalog)
	clock := time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC)
	registry := NewCallbackRegistry(store, catalog, models.WithClock(func() time.Time { return clock }))
	registry.newID = func() string { return "opaque-route" }
	created, err := registry.Create(ctx, CreateCallbackRegistrationRequest{
		ID: "slack-approval", Scope: models.Scope{Kind: "tenant", ID: "one"},
		Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-one"}, DeploymentID: "agent-one",
		Name: "Slack approvals", Provider: "slack",
		Adapter: models.CallbackAdapterReference{
			SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID,
			BindingRevision: binding.Revision, AdapterID: "interactions",
		},
		Subscriptions: []models.CallbackSubscription{{EventType: "approval.decided", Consumer: "approvals", TargetID: "slack-destination"}},
		Configuration: map[string]interface{}{"channel": "C123"},
		Actor:         models.ActivityActor{Type: "user", ID: "operator"}, Reason: "receive signed approval decisions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != models.CallbackRegistrationPaused || created.IngressRoute != "opaque-route" || created.Revision != 1 {
		t.Fatalf("created callback = %#v", created)
	}
	active := models.CallbackRegistrationActive
	clock = clock.Add(time.Minute)
	updated, err := registry.Update(ctx, created.Scope, created.ID, UpdateCallbackRegistrationRequest{
		ExpectedRevision: 1, Status: &active,
		Actor: models.ActivityActor{Type: "user", ID: "operator"}, Reason: "provider endpoint configured",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != models.CallbackRegistrationActive || updated.Revision != 2 || updated.Lifecycle[1].Action != models.CallbackRegistrationActivated {
		t.Fatalf("active callback = %#v", updated)
	}
	nextDefinition := *definition
	nextDefinition.Version = "2.2.1"
	if err := catalog.Register(ctx, &nextDefinition); err != nil {
		t.Fatal(err)
	}
	nextBinding := *binding
	nextBinding.SkillVersion = nextDefinition.Version
	nextBinding.Revision = 2
	if err := catalog.Bind(ctx, &nextBinding); err != nil {
		t.Fatal(err)
	}
	nextAdapter := updated.Adapter
	nextAdapter.SkillVersion = nextDefinition.Version
	nextAdapter.BindingRevision = nextBinding.Revision
	clock = clock.Add(time.Minute)
	upgraded, err := registry.Update(ctx, created.Scope, created.ID, UpdateCallbackRegistrationRequest{
		ExpectedRevision: updated.Revision, Adapter: &nextAdapter,
		Actor: models.ActivityActor{Type: "user", ID: "operator"}, Reason: "upgrade the exact callback adapter",
	})
	if err != nil || upgraded.Adapter != nextAdapter || upgraded.Revision != updated.Revision+1 ||
		upgraded.Lifecycle[len(upgraded.Lifecycle)-1].Action != models.CallbackRegistrationUpdated {
		t.Fatalf("upgraded callback = %#v, %v", upgraded, err)
	}
	byRoute, err := store.GetCallbackRegistrationByIngressRoute(ctx, "opaque-route")
	if err != nil || byRoute.ID != created.ID {
		t.Fatalf("route lookup = %#v, %v", byRoute, err)
	}
	if _, err := registry.Update(ctx, created.Scope, created.ID, UpdateCallbackRegistrationRequest{
		ExpectedRevision: 1, Actor: models.ActivityActor{Type: "user", ID: "operator"}, Reason: "stale update",
	}); !errors.Is(err, models.ErrCallbackRegistrationConflict) {
		t.Fatalf("stale update error = %v", err)
	}
}

func TestCallbackRegistryRejectsUndeclaredEvent(t *testing.T) {
	ctx := context.Background()
	store := newCallbackTestStore(t)
	catalog := skill.NewCatalogWithStore(store)
	definition, binding := registerCallbackFixture(t, ctx, catalog)
	_, err := NewCallbackRegistry(store, catalog).Create(ctx, CreateCallbackRegistrationRequest{
		Scope: models.Scope{Kind: "tenant", ID: "one"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-one"},
		DeploymentID: "agent-one", Name: "Invalid callback", Provider: "slack",
		Adapter: models.CallbackAdapterReference{
			SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID,
			BindingRevision: binding.Revision, AdapterID: "interactions",
		},
		Subscriptions: []models.CallbackSubscription{{EventType: "source.changed", Consumer: "approvals"}},
		Actor:         models.ActivityActor{Type: "user", ID: "operator"}, Reason: "invalid event acceptance",
	})
	if !errors.Is(err, models.ErrInvalidCallbackRegistration) {
		t.Fatalf("undeclared event error = %v", err)
	}
}

func registerCallbackFixture(t *testing.T, ctx context.Context, catalog *skill.Catalog) (*skill.Definition, *skill.Binding) {
	t.Helper()
	definition := &skill.Definition{
		ID: "skill-slack", Version: "2.2.0", Name: "Slack", Actions: map[string]skill.Action{},
		CallbackAdapters: map[string]skill.CallbackAdapter{
			"interactions": {
				ProtocolVersion: skill.CallbackAdapterProtocolV1, Name: "Slack interactions",
				Description: "Verify signed interactive callbacks.", Provider: "slack",
				EventTypes:  []string{"approval.decided"},
				Credentials: []capability.CredentialRequirement{{Name: "signing_secret", Kind: "slack_signing_secret"}},
				Transport: skill.CallbackAdapterTransport{
					Kind: "http", IngressEndpoint: "/v1/callbacks/slack",
					IngressCredentials: []string{"signing_secret"},
				},
			},
		},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{
		ID: "slack", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent-one",
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledCallbackAdapters: []string{"interactions"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
		Credentials: map[string]skill.CredentialReference{
			"signing_secret": {Kind: "slack_signing_secret", ID: "credential-store-signing-secret"},
		},
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	return definition, binding
}

// The store helpers below copy runtime's newTestKernelStore,
// newTestSQLiteStore and countRows, so the callback tests do not depend on
// runtime's test helpers and can live with the callback code.

// newCallbackTestStore returns an empty in-memory kernel store that is closed
// when the test ends.
func newCallbackTestStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// newCallbackTestSQLiteStore opens a SQLite store on a fresh file and the
// test's own connection to that file, both closed when the test ends.
func newCallbackTestSQLiteStore(t testing.TB) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return store, db
}

// countCallbackRows counts the rows of table through the test's own
// connection.
func countCallbackRows(t testing.TB, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
