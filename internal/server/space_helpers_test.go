package server

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// localOperatorID is the person the server names on every conversation it
// creates.
const localOperatorID = "local-operator"

// seedSpace creates a space owned by ownerID in the store; an owner's first
// space in a scope is their default space. The kernel and this server create
// none: the daemon creates the local operator's at start. Conversations made
// through the API need the local operator's space, those made straight on
// the service with no person need the empty owner's.
func seedSpace(t testing.TB, store runtime.SpaceStore, scope runtime.Scope, ownerID string) *runtime.Space {
	t.Helper()
	space, err := runtime.NewSpaceService(store).CreateSpace(t.Context(), runtime.CreateSpaceRequest{
		Scope: scope, Name: "Home", Look: runtime.DefaultSpaceLook, OwnerID: ownerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return space
}
