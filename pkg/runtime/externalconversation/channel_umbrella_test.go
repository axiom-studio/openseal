package externalconversation_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	_ "github.com/mattn/go-sqlite3"
)

func TestGatewayOwnerNamesItsDeploymentAndFiltersLists(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "tenant-1"}
	gateway := storetest.IngressGateway(scope)
	for name, mutate := range map[string]func(*models.ExternalConversationIngressGateway){
		"no owner":           func(g *models.ExternalConversationIngressGateway) { g.Owner = models.ObjectiveOwner{} },
		"another deployment": func(g *models.ExternalConversationIngressGateway) { g.Owner.ID = "other" },
		"unknown owner kind": func(g *models.ExternalConversationIngressGateway) { g.Owner.Type = "user" },
	} {
		candidate := gateway
		mutate(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Fatalf("%s: gateway accepted", name)
		}
	}
	for _, store := range []models.ExternalConversationGatewayStore{kerneltest.NewStore(t), func() models.ExternalConversationGatewayStore {
		sqlite, err := sqlite.New(filepath.Join(t.TempDir(), "gateways.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sqlite.Close() })
		return sqlite
	}()} {
		service := externalconversation.NewExternalConversationGatewayService(store, nil)
		team := gateway
		team.Owner, team.DeploymentID = models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "crew"}, "crew"
		for id, value := range map[string]models.ExternalConversationIngressGateway{"agent-gateway": gateway, "team-gateway": team} {
			if _, err := service.Create(context.Background(), externalconversation.CreateExternalConversationGatewayRequest{ID: id, Name: id, Gateway: value,
				Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "create"}); err != nil {
				t.Fatal(err)
			}
		}
		owner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "crew"}
		listed, err := service.List(context.Background(), models.ExternalConversationGatewayFilter{Scope: scope, Owner: &owner})
		if err != nil || len(listed) != 1 || listed[0].ID != "team-gateway" {
			t.Fatalf("%T Team gateways = %#v, %v", store, listed, err)
		}
	}
}

func TestSQLiteChannelUmbrellaBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channels.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := models.Scope{Kind: "tenant", ID: "tenant-1"}
	if _, err := externalconversation.NewExternalConversationGatewayService(store, nil).Create(context.Background(), externalconversation.CreateExternalConversationGatewayRequest{ID: "legacy", Name: "legacy",
		Gateway: storetest.IngressGateway(scope), Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "create"}); err != nil {
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
	reopened, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	gateway, err := reopened.GetExternalConversationGateway(context.Background(), scope, "legacy")
	if err != nil || gateway.Gateway.Owner != (models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "skill-deployment-1"}) {
		t.Fatalf("backfilled gateway = %#v, %v", gateway, err)
	}
	if !strings.Contains(string(gateway.Gateway.Owner.Type), "agent") {
		t.Fatal("owner type")
	}
}
