package tui

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
	"unicode"

	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func downloadArtifact(ctx context.Context, artifactClient client.ArtifactClient, scope runtime.Scope, artifact *runtime.Artifact, directory string) (path string, err error) {
	if artifact == nil {
		return "", errors.New("artifact is required")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create artifact download directory: %w", err)
	}
	download, err := artifactClient.DownloadArtifactContent(ctx, scope, artifact.ID, artifact.Version)
	if err != nil {
		return "", err
	}
	defer download.Body.Close()

	temporary, err := os.CreateTemp(directory, ".openseal-artifact-*")
	if err != nil {
		return "", fmt.Errorf("create artifact download: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()

	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hasher), download.Body)
	closeErr := temporary.Close()
	if copyErr != nil {
		return "", fmt.Errorf("download artifact content: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close artifact download: %w", closeErr)
	}
	actualDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if written != artifact.SizeBytes {
		return "", fmt.Errorf("artifact size mismatch: expected %d bytes, received %d", artifact.SizeBytes, written)
	}
	if !strings.EqualFold(actualDigest, artifact.Digest) {
		return "", fmt.Errorf("artifact digest mismatch: expected %s, received %s", artifact.Digest, actualDigest)
	}
	if headerDigest := strings.TrimSpace(download.Digest); headerDigest != "" && !strings.EqualFold(headerDigest, artifact.Digest) {
		return "", errors.New("artifact response digest does not match catalog metadata")
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return "", fmt.Errorf("secure artifact download: %w", err)
	}
	destination := filepath.Join(directory, artifactDownloadName(artifact))
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", fmt.Errorf("publish artifact download: %w", err)
	}
	return destination, nil
}

func artifactDownloadName(artifact *runtime.Artifact) string {
	name := sanitizeFileComponent(filepath.Base(strings.TrimSpace(artifact.Name)))
	if name == "" || name == "." {
		name = "artifact"
	}
	id := sanitizeFileComponent(artifact.ID)
	if id == "" {
		id = "artifact"
	}
	return fmt.Sprintf("%s-v%d-%s", id, artifact.Version, name)
}

func sanitizeFileComponent(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, character := range value {
		switch {
		case unicode.IsLetter(character), unicode.IsDigit(character), character == '.', character == '-', character == '_':
			builder.WriteRune(character)
		case unicode.IsSpace(character):
			builder.WriteByte('-')
		}
		if builder.Len() >= 160 {
			break
		}
	}
	return strings.Trim(builder.String(), ".-")
}
