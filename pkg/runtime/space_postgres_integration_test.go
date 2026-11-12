//go:build integration

package runtime

import (
	"context"
	"testing"
)

func TestPostgresSpaceStore(t *testing.T) {
	store, _ := openTestPostgresStore(t, "openseal_spaces_")
	if version, err := store.PostgresSchemaVersion(context.Background()); err != nil || version != postgresSchemaHead(t) {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
	exerciseSpaceStore(t, store)
}

func TestPostgresOwnerDefaultSpaces(t *testing.T) {
	store, _ := openTestPostgresStore(t, "openseal_owner_default_spaces_")
	exerciseOwnerDefaultSpaces(t, store)
}

func TestPostgresSpaceAccess(t *testing.T) {
	store, _ := openTestPostgresStore(t, "openseal_space_access_")
	exerciseSpaceAccess(t, store)
}
