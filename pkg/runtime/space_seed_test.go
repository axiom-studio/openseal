package runtime

import (
	"context"
	"errors"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"testing"
)

// seedTestSpace gives the empty owner a default space in the scope, so a
// conversation created with no space and no person behind it has one to go to.
func seedTestSpace(t testing.TB, store models.SpaceStore, scope models.Scope) *models.Space {
	t.Helper()
	return seedTestSpaceFor(t, store, scope, "")
}

// seedTestSpaceFor creates a space owned by ownerID. The owner's first space
// in the scope is their default space. An owner who already has a default
// space keeps it and gets no second one, so a fixture that is built more than
// once on a store can seed every time.
func seedTestSpaceFor(t testing.TB, store models.SpaceStore, scope models.Scope, ownerID string) *models.Space {
	t.Helper()
	if existing, err := store.DefaultSpace(context.Background(), scope, ownerID); err == nil {
		return existing
	} else if !errors.Is(err, models.ErrNoDefaultSpace) {
		t.Fatalf("seed space: %v", err)
	}
	space, err := portfolio.NewSpaceService(store).CreateSpace(context.Background(), portfolio.CreateSpaceRequest{
		Scope: scope, Name: "Home", Look: models.DefaultSpaceLook, OwnerID: ownerID,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	return space
}
