package runtime

import (
	"context"
	"errors"
	"testing"
)

// seedTestSpace gives the empty owner a default space in the scope, so a
// conversation created with no space and no person behind it has one to go to.
func seedTestSpace(t testing.TB, store SpaceStore, scope Scope) *Space {
	t.Helper()
	return seedTestSpaceFor(t, store, scope, "")
}

// seedTestSpaceFor creates a space owned by ownerID. The owner's first space
// in the scope is their default space. An owner who already has a default
// space keeps it and gets no second one, so a fixture that is built more than
// once on a store can seed every time.
func seedTestSpaceFor(t testing.TB, store SpaceStore, scope Scope, ownerID string) *Space {
	t.Helper()
	if existing, err := store.DefaultSpace(context.Background(), scope, ownerID); err == nil {
		return existing
	} else if !errors.Is(err, ErrNoDefaultSpace) {
		t.Fatalf("seed space: %v", err)
	}
	space, err := NewSpaceService(store).CreateSpace(context.Background(), CreateSpaceRequest{
		Scope: scope, Name: "Home", Look: DefaultSpaceLook, OwnerID: ownerID,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	return space
}
