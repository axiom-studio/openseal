// The AgentRequest inbox fixtures of the external inbox reconciler tests:
// a Team catalog over a SQLite store and one pending AgentRequest.

package runtime_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type agentRequestInboxTeamStore struct {
	*sqlite.Store
	deployment *kernelteam.Deployment
	definition *kernelteam.Definition
}

func (s *agentRequestInboxTeamStore) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	if s.deployment == nil || s.deployment.Scope != scope || s.deployment.ID != id {
		return nil, kernelteam.ErrDeploymentNotFound
	}
	return s.deployment, nil
}

func (s *agentRequestInboxTeamStore) GetTeamDefinition(_ context.Context, id, version string) (*kernelteam.Definition, error) {
	if s.definition == nil || s.definition.ID != id || s.definition.Version != version {
		return nil, kernelteam.ErrDefinitionNotFound
	}
	return s.definition, nil
}

func createInboxRequest(t *testing.T, store models.AgentRequestInboxStore, policy models.AgentRequestAcceptancePolicy, recipient models.CollaborationParty) (*models.AgentRun, *models.AgentRequest) {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "inbox"}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "requester"}, AssignedAgentID: "requester",
		Goal: "Coordinate launch", Source: models.RunSourceManual, Priority: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := runtime.NewCollaborationService(store).CreateAgentRequest(t.Context(), runtime.CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "requester"},
		Recipient: recipient, SourceRunID: source.ID, Goal: "Review the launch evidence",
		Instructions:     "Check the evidence before accepting.",
		SharedContext:    map[string]interface{}{"evidenceRef": "artifact:launch"},
		AcceptancePolicy: policy, IdempotencyKey: "review-launch",
	})
	if err != nil {
		t.Fatal(err)
	}
	return source, created.Request
}

// newInboxSQLiteStore opens a SQLite store on a fresh file and the test's
// own connection to that file, both closed when the test ends.
func newInboxSQLiteStore(t testing.TB) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, conversationtest.OpenSQLiteDB(t, path)
}
