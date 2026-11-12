// The in-package tests' own copies of externalconversationtest's endpoint
// fixtures and crash dispatcher (that package imports this one), and of
// runtime's SQLite file store and run store contract fixtures.

package externalconversation

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// endpointFixture is endpointFixtureWithOperations for message sends only.
func endpointFixture(
	t *testing.T,
	ctx context.Context,
	provider string,
) (*sqlite.Store, *skill.Catalog, *models.ExternalConversationEndpoint) {
	return endpointFixtureWithOperations(t, ctx, provider, []skill.ConversationDeliveryOperation{skill.ConversationDeliveryMessageSend})
}

// endpointFixtureWithOperations creates the fixture endpoint of provider,
// with the delivery operations listed, in a new in-memory store.
func endpointFixtureWithOperations(
	t *testing.T,
	ctx context.Context,
	provider string,
	operations []skill.ConversationDeliveryOperation,
) (*sqlite.Store, *skill.Catalog, *models.ExternalConversationEndpoint) {
	t.Helper()
	store := kerneltest.NewStore(t)
	catalog, endpoint := endpointFixtureOn(t, ctx, store, provider, operations)
	return store, catalog, endpoint
}

// endpointFixtureOn creates the fixture endpoint in any endpoint store
// (SQLite or PostgreSQL).
func endpointFixtureOn(
	t *testing.T,
	ctx context.Context,
	store models.ExternalConversationEndpointStore,
	provider string,
	operations []skill.ConversationDeliveryOperation,
) (*skill.Catalog, *models.ExternalConversationEndpoint) {
	t.Helper()
	return endpointFixtureOwnedBy(t, ctx, store, provider, operations, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: provider + "-agent"})
}

// endpointFixtureOwnedBy creates the fixture endpoint for an Agent or a
// Team owner.
func endpointFixtureOwnedBy(
	t *testing.T,
	ctx context.Context,
	store models.ExternalConversationEndpointStore,
	provider string,
	operations []skill.ConversationDeliveryOperation,
	owner models.ObjectiveOwner,
) (*skill.Catalog, *models.ExternalConversationEndpoint) {
	t.Helper()
	catalog := skill.NewCatalog()
	definition := skillbindingtest.SlackConversationSkillDefinition()
	definition.ID, definition.Name = provider, provider
	adapter := definition.ConversationAdapters["conversations"]
	adapter.Delivery.Operations = append([]skill.ConversationDeliveryOperation(nil), operations...)
	adapter.Provider = provider
	adapter.Name = provider + " conversations"
	adapter.Transport.IngressEndpoint = provider + ".conversation.ingress"
	adapter.Transport.DeliveryEndpoint = provider + ".conversation.deliver"
	if provider == "webchat" {
		adapter.Credentials = nil
		adapter.Transport.DeliveryCredentials = nil
	}
	definition.ConversationAdapters["conversations"] = adapter
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := models.Scope{Kind: "tenant", ID: provider}
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	deploymentID := owner.ID
	handler := models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: deploymentID}
	if owner.Type == models.OwnerTypeTeam {
		handler.Kind = models.ExternalConversationHandlerTeam
	}
	binding := &skill.Binding{
		ID: provider, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}
	if provider == "slack" {
		binding.Credentials = map[string]skill.CredentialReference{
			"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://tenant/slack"},
		}
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	endpoint, err := NewEndpointService(store, catalog).Create(
		ctx,
		CreateExternalConversationEndpointRequest{
			ID: provider + "-endpoint", Scope: scope,
			Owner: owner, DeploymentID: deploymentID,
			Name: provider + " endpoint",
			Adapter: models.ExternalConversationAdapterReference{
				SkillID: definition.ID, SkillVersion: definition.Version,
				BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
			},
			Mode: capability.ConversationEndpointChannel, Address: provider + "-address",
			Handler: handler,
			Policy: models.ExternalConversationPolicy{
				MessageSelection: models.ExternalConversationSelectAllMessages,
				ReplyMode:        models.ExternalConversationReplyThread, IgnoreBots: true,
			},
			Status: models.ExternalConversationEndpointActive,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, endpoint
}

// crashAfterDispatch dispatches through next and, with crashOnce, fails the
// first dispatch after its Run is durable, as a crash would.
type crashAfterDispatch struct {
	next       Dispatcher
	crashOnce  bool
	dispatches int
	runID      string
}

func (d *crashAfterDispatch) DispatchExternalConversation(
	ctx context.Context,
	req DispatchRequest,
) (*DispatchResult, error) {
	d.dispatches++
	result, err := d.next.DispatchExternalConversation(ctx, req)
	if err != nil {
		return nil, err
	}
	d.runID = result.RunID
	if d.crashOnce {
		d.crashOnce = false
		return nil, errors.New("synthetic crash after durable Run dispatch")
	}
	return result, nil
}

// newFileStore opens a SQLite store on a fresh file and the test's own
// connection to that file, both closed when the test ends. Use it instead
// of conversationtest.NewStore when the test reads or writes rows no store
// method offers.
func newFileStore(t testing.TB) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, kerneltest.OpenSQLiteDB(t, path)
}

// runStore is what the run store contracts use: the same public commands
// and durable scheduling boundary on the SQLite and PostgreSQL stores.
type runStore interface {
	models.RunCommandStore
	models.AgentRunScheduleStore
}

type runStoreFixture struct {
	store  runStore
	reopen func() runStore
	// db is the test's own connection to the store's database, for rows no
	// store method reads or writes; table names a table for SQL on it. Both
	// stay valid across reopen.
	db    *sql.DB
	table func(name string) string
}

// forEachRunStore runs test on a SQLite file store and on a Postgres schema
// (skipped without a Postgres DSN), each of which can be reopened.
func forEachRunStore(t *testing.T, test func(*testing.T, runStoreFixture)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "contract.db")
		current, err := sqlite.New(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = current.Close() })
		test(t, runStoreFixture{store: current, reopen: func() runStore {
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			current, err = sqlite.New(path)
			if err != nil {
				t.Fatal(err)
			}
			return current
		}, db: kerneltest.OpenSQLiteDB(t, path), table: func(name string) string { return name }})
	})
	t.Run("postgres", func(t *testing.T) {
		pgSchema := storetest.NewPostgresSchema(t, "run_store_contract_")
		schema, dsn := pgSchema.Name, pgSchema.DSN
		current, err := postgres.New(t.Context(), dsn, postgres.WithSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = current.Close() })
		test(t, runStoreFixture{store: current, reopen: func() runStore {
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			current, err = postgres.New(t.Context(), dsn, postgres.WithSchema(schema))
			if err != nil {
				t.Fatal(err)
			}
			return current
		}, db: pgSchema.DB, table: pgSchema.Table})
	})
}

var runStoreEpoch = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
