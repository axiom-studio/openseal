// The in-package collaboration tests' own copies of the SQLite session store
// opener, the chat roster Team catalog and the artifact digest helper that
// runtime's tests use.

package collaboration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// openCollaborationSessionStore opens a SQLite store on a fresh file, closed
// when the test ends.
func openCollaborationSessionStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// collaborationRosterTeams serves one Team deployment as a Team roster.
type collaborationRosterTeams struct {
	team *kernelteam.Deployment
}

func (c collaborationRosterTeams) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	if c.team == nil || c.team.ID != id || c.team.Scope != scope {
		return nil, kernelteam.ErrDeploymentNotFound
	}
	return c.team, nil
}

func collaborationRosterTeam(scope models.Scope, members ...string) *kernelteam.Deployment {
	team := &kernelteam.Deployment{ID: "crew", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive}
	for _, member := range members {
		team.Roster = append(team.Roster, kernelteam.RosterAssignment{ID: member, AgentDeploymentID: member})
	}
	return team
}

func collaborationDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}
