package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyInstructionSurfaceIsRemoved(t *testing.T) {
	legacyPackage := filepath.Join("..", "..", "pkg", "instruction")
	if _, err := os.Stat(legacyPackage); err == nil {
		t.Fatalf("retired browser instruction package returned at %s", legacyPackage)
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect retired browser instruction package: %v", err)
	}
}
