package runtime

import (
	"strings"
	"testing"
)

func TestExternalOperationLockKeyIsStablePrintableAndScopeSeparated(t *testing.T) {
	digest := hashString("stable operation")
	first := externalOperationLockKey(Scope{Kind: "tenant", ID: "1"}, digest)
	if first != externalOperationLockKey(Scope{Kind: "tenant", ID: "1"}, digest) {
		t.Fatal("lock key is not stable")
	}
	if strings.ContainsRune(first, '\x00') || len(first) != 71 || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("lock key is not a printable SHA-256 digest: %q", first)
	}
	if first == externalOperationLockKey(Scope{Kind: "tenant", ID: "2"}, digest) ||
		first == externalOperationLockKey(Scope{Kind: "tenant", ID: "1"}, hashString("other operation")) {
		t.Fatal("lock key does not separate scope and operation identity")
	}
}
