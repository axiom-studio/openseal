//go:build integration

package runtime

import (
	"testing"
)

func TestPostgresApplicationRouteMatching(t *testing.T) {
	store, _ := openTestPostgresStore(t, "app_route_")
	checkApplicationRouteMatching(t, store)
}
