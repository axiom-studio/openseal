package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalFileStoreLifecycle(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalFileStore(Config{Root: root, TTL: time.Hour, MaxBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.StoreReader(context.Background(), strings.NewReader("report"), "../report.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID == "" || stored.Size != 6 || stored.Filename != "report.txt" || stored.MimeType != "text/plain" {
		t.Fatalf("stored file = %#v", stored)
	}
	info, err := os.Stat(filepath.Join(root, stored.ID))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file permission = %v", info.Mode().Perm())
	}
	reader, err := store.Open(context.Background(), stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil || string(content) != "report" {
		t.Fatalf("content = %q err=%v", content, readErr)
	}
	if err := store.AssociateWithRun(context.Background(), stored.ID, "run-1"); err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Get(context.Background(), stored.ID)
	if err != nil || metadata.RunID != "run-1" {
		t.Fatalf("metadata = %#v err=%v", metadata, err)
	}
	if err := store.DeleteByRunID(context.Background(), "run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), stored.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted lookup error = %v", err)
	}
}

func TestLocalFileStoreBoundsAndDoesNotResolvePaths(t *testing.T) {
	store, err := NewLocalFileStore(Config{Root: t.TempDir(), MaxBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StoreReader(context.Background(), strings.NewReader("12345"), "large", ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("large write error = %v", err)
	}
	if _, err := store.Open(context.Background(), "../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("path lookup error = %v", err)
	}
	entries, err := os.ReadDir(store.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries = %v err=%v", entries, err)
	}
}

func TestLocalFileStoreExpiryAndCleanup(t *testing.T) {
	store, err := NewLocalFileStore(Config{Root: t.TempDir(), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	first, err := store.Store(context.Background(), []byte("one"), "one", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Store(context.Background(), []byte("two"), "two", "")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := store.Open(context.Background(), first.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired open error = %v", err)
	}
	removed, err := store.Cleanup(context.Background())
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(store.root, second.ID)); !os.IsNotExist(err) {
		t.Fatalf("expired file remained: %v", err)
	}
}

func TestLocalFileStoreHonorsCancellation(t *testing.T) {
	store, err := NewLocalFileStore(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Store(ctx, []byte("data"), "file", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write error = %v", err)
	}
}
