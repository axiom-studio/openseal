// Package collaborationtest holds AgentRequest inbox fixtures shared by the
// collaboration tests and the tests of packages above it: a Team catalog over
// a SQLite store and one pending AgentRequest. It uses only exported API.
package collaborationtest

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// TeamStore serves one Team deployment and its definition next to a kernel
// store; GetTeamDeployment and GetTeamDefinition report not found for any
// other id.
type TeamStore struct {
	*sqlite.Store
	Deployment *kernelteam.Deployment
	Definition *kernelteam.Definition
}

func (s *TeamStore) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	if s.Deployment == nil || s.Deployment.Scope != scope || s.Deployment.ID != id {
		return nil, kernelteam.ErrDeploymentNotFound
	}
	return s.Deployment, nil
}

func (s *TeamStore) GetTeamDefinition(_ context.Context, id, version string) (*kernelteam.Definition, error) {
	if s.Definition == nil || s.Definition.ID != id || s.Definition.Version != version {
		return nil, kernelteam.ErrDefinitionNotFound
	}
	return s.Definition, nil
}

// CreateInboxRequest creates a source Run for agent "requester" and one
// pending AgentRequest from it to recipient under policy.
func CreateInboxRequest(t *testing.T, store models.AgentRequestInboxStore, policy models.AgentRequestAcceptancePolicy, recipient models.CollaborationParty) (*models.AgentRun, *models.AgentRequest) {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "inbox"}
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "requester"}, AssignedAgentID: "requester",
		Goal: "Coordinate launch", Source: models.RunSourceManual, Priority: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := collaboration.NewCollaborationService(store).CreateAgentRequest(t.Context(), collaboration.CreateAgentRequestRequest{
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
