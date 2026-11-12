package openseal

import (
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// seedSpace returns ownerID's default space in the scope, creating it when
// they have none: the kernel creates no space on its own. A fixture seeds the
// space of the person its deployments are created by, or of the empty owner
// when no person is behind its conversations.
func seedSpace(t testing.TB, engine *Engine, scope runtime.Scope, ownerID string) *runtime.Space {
	t.Helper()
	space, err := engine.DefaultSpace(t.Context(), scope, ownerID)
	if err == nil {
		return space
	}
	if !errors.Is(err, ErrNoDefaultSpace) {
		t.Fatal(err)
	}
	space, err = engine.CreateSpace(t.Context(), runtime.CreateSpaceRequest{Scope: scope, Name: "Home", Look: runtime.DefaultSpaceLook, OwnerID: ownerID})
	if err != nil {
		t.Fatal(err)
	}
	return space
}
