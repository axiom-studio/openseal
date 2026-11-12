package runtime

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	_ "github.com/mattn/go-sqlite3"
)

func TestGatewayOwnerNamesItsDeploymentAndFiltersLists(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "tenant-1"}
	gateway := storetest.IngressGateway(scope)
	for name, mutate := range map[string]func(*ExternalConversationIngressGateway){
		"no owner":           func(g *ExternalConversationIngressGateway) { g.Owner = ObjectiveOwner{} },
		"another deployment": func(g *ExternalConversationIngressGateway) { g.Owner.ID = "other" },
		"unknown owner kind": func(g *ExternalConversationIngressGateway) { g.Owner.Type = "user" },
	} {
		candidate := gateway
		mutate(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Fatalf("%s: gateway accepted", name)
		}
	}
	for _, store := range []ExternalConversationGatewayStore{newTestKernelStore(t), func() ExternalConversationGatewayStore {
		sqlite, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "gateways.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sqlite.Close() })
		return sqlite
	}()} {
		service := NewExternalConversationGatewayService(store)
		team := gateway
		team.Owner, team.DeploymentID = ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, "crew"
		for id, value := range map[string]ExternalConversationIngressGateway{"agent-gateway": gateway, "team-gateway": team} {
			if _, err := service.Create(context.Background(), CreateExternalConversationGatewayRequest{ID: id, Name: id, Gateway: value,
				Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "create"}); err != nil {
				t.Fatal(err)
			}
		}
		owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}
		listed, err := service.List(context.Background(), ExternalConversationGatewayFilter{Scope: scope, Owner: &owner})
		if err != nil || len(listed) != 1 || listed[0].ID != "team-gateway" {
			t.Fatalf("%T Team gateways = %#v, %v", store, listed, err)
		}
	}
}

func TestSQLiteChannelUmbrellaBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channels.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "tenant-1"}
	if _, err := NewExternalConversationGatewayService(store).Create(context.Background(), CreateExternalConversationGatewayRequest{ID: "legacy", Name: "legacy",
		Gateway: storetest.IngressGateway(scope), Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "create"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE external_conversation_gateways SET payload = json_remove(payload, '$.gateway.owner')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	reopened, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	gateway, err := reopened.GetExternalConversationGateway(context.Background(), scope, "legacy")
	if err != nil || gateway.Gateway.Owner != (ObjectiveOwner{Type: OwnerTypeAgent, ID: "skill-deployment-1"}) {
		t.Fatalf("backfilled gateway = %#v, %v", gateway, err)
	}
	if !strings.Contains(string(gateway.Gateway.Owner.Type), "agent") {
		t.Fatal("owner type")
	}
}
