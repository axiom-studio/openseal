//go:build integration

package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
)

func TestPostgresSpaceStore(t *testing.T) {
	store, _ := kerneltest.OpenPostgres(t, "openseal_spaces_")
	if version, err := store.PostgresSchemaVersion(context.Background()); err != nil || version != kerneltest.PostgresSchemaHead(t) {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
	exerciseSpaceStore(t, store)
}

func TestPostgresOwnerDefaultSpaces(t *testing.T) {
	store, _ := kerneltest.OpenPostgres(t, "openseal_owner_default_spaces_")
	exerciseOwnerDefaultSpaces(t, store)
}

func TestPostgresSpaceAccess(t *testing.T) {
	store, _ := kerneltest.OpenPostgres(t, "openseal_space_access_")
	exerciseSpaceAccess(t, store)
}

func TestPostgresSetSpaceOwnerKeepsDefaults(t *testing.T) {
	store, _ := kerneltest.OpenPostgres(t, "openseal_space_owner_change_")
	exerciseSetSpaceOwnerDefaults(t, store)
}
