package postgres

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Store test: externalOperationLockKey is the store's advisory lock key.

func TestExternalOperationLockKeyIsStablePrintableAndScopeSeparated(t *testing.T) {
	digest := models.SHA256DigestString("stable operation")
	first := externalOperationLockKey(models.Scope{Kind: "tenant", ID: "1"}, digest)
	if first != externalOperationLockKey(models.Scope{Kind: "tenant", ID: "1"}, digest) {
		t.Fatal("lock key is not stable")
	}
	if strings.ContainsRune(first, '\x00') || len(first) != 71 || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("lock key is not a printable SHA-256 digest: %q", first)
	}
	if first == externalOperationLockKey(models.Scope{Kind: "tenant", ID: "2"}, digest) ||
		first == externalOperationLockKey(models.Scope{Kind: "tenant", ID: "1"}, models.SHA256DigestString("other operation")) {
		t.Fatal("lock key does not separate scope and operation identity")
	}
}
