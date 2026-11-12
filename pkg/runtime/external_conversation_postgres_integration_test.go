//go:build integration

package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
)

func TestPostgresApplicationRouteMatching(t *testing.T) {
	store, _ := kerneltest.OpenPostgres(t, "app_route_")
	checkApplicationRouteMatching(t, store)
}
