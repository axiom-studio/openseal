// Package artifact provides portable content-store adapters for the canonical
// runtime artifact catalog. Metadata remains owned by pkg/runtime.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

const localReferencePrefix = "local-sha256:"

type LocalStore struct {
	root string
}

func NewLocalStore(root string) (*LocalStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("artifact content root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, fmt.Errorf("create artifact content root: %w", err)
	}
	return &LocalStore{root: absolute}, nil
}

func (s *LocalStore) Put(ctx context.Context, write runtime.ArtifactContentWrite) (runtime.ArtifactStoredContent, error) {
	if s == nil || s.root == "" {
		return runtime.ArtifactStoredContent{}, errors.New("local artifact store is not configured")
	}
	if err := write.Scope.Validate(); err != nil {
		return runtime.ArtifactStoredContent{}, err
	}
	if write.Reader == nil {
		return runtime.ArtifactStoredContent{}, errors.New("artifact content reader is required")
	}
	if write.SizeBytes < -1 {
		return runtime.ArtifactStoredContent{}, errors.New("artifact content size cannot be less than -1")
	}
	if write.Digest != "" && !validDigest(write.Digest) {
		return runtime.ArtifactStoredContent{}, errors.New("artifact content digest must be sha256:<64 hex characters>")
	}
	directory := s.scopeDirectory(write.Scope)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return runtime.ArtifactStoredContent{}, err
	}
	temporary, err := os.CreateTemp(directory, ".upload-*")
	if err != nil {
		return runtime.ArtifactStoredContent{}, err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return runtime.ArtifactStoredContent{}, err
	}
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hasher), &contextReader{ctx: ctx, reader: write.Reader})
	if err != nil {
		return runtime.ArtifactStoredContent{}, err
	}
	if write.SizeBytes >= 0 && written != write.SizeBytes {
		return runtime.ArtifactStoredContent{}, fmt.Errorf("artifact content size mismatch: wrote %d bytes, expected %d", written, write.SizeBytes)
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if write.Digest != "" && !strings.EqualFold(strings.TrimSpace(write.Digest), digest) {
		return runtime.ArtifactStoredContent{}, errors.New("artifact content digest mismatch")
	}
	if err := temporary.Sync(); err != nil {
		return runtime.ArtifactStoredContent{}, err
	}
	if err := temporary.Close(); err != nil {
		return runtime.ArtifactStoredContent{}, err
	}
	finalPath := filepath.Join(directory, strings.TrimPrefix(digest, "sha256:"))
	if _, err := os.Stat(finalPath); err == nil {
		committed = true
		_ = os.Remove(temporaryPath)
	} else if !os.IsNotExist(err) {
		return runtime.ArtifactStoredContent{}, err
	} else if err := os.Rename(temporaryPath, finalPath); err != nil {
		if _, statErr := os.Stat(finalPath); statErr != nil {
			return runtime.ArtifactStoredContent{}, err
		}
		committed = true
	} else {
		committed = true
	}
	return runtime.ArtifactStoredContent{
		ContentRef: localReferencePrefix + strings.TrimPrefix(digest, "sha256:"),
		Digest:     digest, SizeBytes: written,
	}, nil
}

func (s *LocalStore) Open(_ context.Context, scope runtime.Scope, contentRef string) (io.ReadCloser, error) {
	if s == nil || s.root == "" {
		return nil, errors.New("local artifact store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	hexDigest, err := parseLocalReference(contentRef)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(s.scopeDirectory(scope), hexDigest))
	if os.IsNotExist(err) {
		return nil, runtime.ErrArtifactNotFound
	}
	return file, err
}

func (s *LocalStore) scopeDirectory(scope runtime.Scope) string {
	digest := sha256.Sum256([]byte(scope.Kind + "\x00" + scope.ID))
	return filepath.Join(s.root, hex.EncodeToString(digest[:]))
}

func parseLocalReference(reference string) (string, error) {
	value := strings.TrimSpace(reference)
	if !strings.HasPrefix(value, localReferencePrefix) {
		return "", errors.New("content reference is not owned by the local artifact store")
	}
	hexDigest := strings.TrimPrefix(value, localReferencePrefix)
	if len(hexDigest) != 64 {
		return "", errors.New("invalid local artifact content reference")
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", errors.New("invalid local artifact content reference")
	}
	return strings.ToLower(hexDigest), nil
}

func validDigest(value string) bool {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(value)), ":", 2)
	if len(parts) != 2 || parts[0] != "sha256" || len(parts[1]) != 64 {
		return false
	}
	_, err := hex.DecodeString(parts[1])
	return err == nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(buffer)
	}
}

var _ runtime.ArtifactContentStore = (*LocalStore)(nil)
