package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestLocalStoreIsContentAddressedScopedAndRestartSafe(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	scope := runtime.Scope{Kind: "local", ID: "research"}
	content := []byte("cited report")
	digest := sha256.Sum256(content)
	written, err := store.Put(context.Background(), runtime.ArtifactContentWrite{
		Scope: scope, Reader: bytes.NewReader(content), SizeBytes: int64(len(content)),
		Digest: "sha256:" + hex.EncodeToString(digest[:]), MediaType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(written.ContentRef, localReferencePrefix) || written.SizeBytes != int64(len(content)) {
		t.Fatalf("stored content = %#v", written)
	}
	available, err := store.Available(context.Background(), scope, written.ContentRef)
	if err != nil || !available {
		t.Fatalf("stored content availability = %v, %v", available, err)
	}
	available, err = store.Available(context.Background(), runtime.Scope{Kind: "local", ID: "other"}, written.ContentRef)
	if err != nil || available {
		t.Fatalf("cross-scope content availability = %v, %v", available, err)
	}
	opened, err := store.Open(context.Background(), scope, written.ContentRef)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil || !bytes.Equal(loaded, content) {
		t.Fatalf("loaded = %q, %v", loaded, err)
	}

	restarted, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	opened, err = restarted.Open(context.Background(), scope, written.ContentRef)
	if err != nil {
		t.Fatal(err)
	}
	_ = opened.Close()
	if _, err := restarted.Open(context.Background(), runtime.Scope{Kind: "local", ID: "other"}, written.ContentRef); err == nil {
		t.Fatal("cross-scope content lookup succeeded")
	}
}

func TestLocalStoreRejectsIntegrityFailuresAndUnsafeReferences(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scope := runtime.Scope{Kind: "local", ID: "default"}
	if _, err := store.Put(context.Background(), runtime.ArtifactContentWrite{
		Scope: scope, Reader: strings.NewReader("content"), SizeBytes: 2,
	}); err == nil {
		t.Fatal("size mismatch was accepted")
	}
	if _, err := store.Put(context.Background(), runtime.ArtifactContentWrite{
		Scope: scope, Reader: strings.NewReader("content"), SizeBytes: -1,
		Digest: "sha256:" + strings.Repeat("0", 64),
	}); err == nil {
		t.Fatal("digest mismatch was accepted")
	}
	for _, reference := range []string{"../../secret", "local-sha256:../secret", "https://objects.test/file", "local-sha256:not-hex"} {
		if _, err := store.Open(context.Background(), scope, reference); err == nil {
			t.Fatalf("unsafe reference %q was accepted", reference)
		}
		if available, err := store.Available(context.Background(), scope, reference); err != nil || available {
			t.Fatalf("unsafe reference %q availability = %v, %v", reference, available, err)
		}
	}
}
