package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
)

// ValidateAgentProfileImage checks content, not just caller-supplied metadata.
// The host must independently authorize the profile mutation and artifact read
// for its principal before calling this scoped validation.
func ValidateAgentProfileImage(ctx context.Context, store interface {
	GetArtifact(context.Context, Scope, string, int64) (*Artifact, error)
}, content ArtifactContentStore, scope Scope, reference *agent.ProfileImage) error {
	if reference == nil {
		return nil
	}
	if store == nil || content == nil || reference.ArtifactID == "" || reference.Version < 1 {
		return errors.New("profile image content is unavailable")
	}
	artifact, err := store.GetArtifact(ctx, scope, reference.ArtifactID, reference.Version)
	if err != nil {
		return err
	}
	const maximum = 1 << 20
	// Availability may be projected by the host rather than persisted. Opening
	// and validating the content below is the authoritative availability check.
	if artifact == nil || artifact.Scope != scope || artifact.ID != reference.ArtifactID || artifact.Version != reference.Version || artifact.SizeBytes < 1 || artifact.SizeBytes > maximum || artifact.ContentAvailability == ArtifactContentUnavailable {
		return errors.New("profile image must be an available image in this workspace, at most 1 MiB")
	}
	switch artifact.MediaType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return errors.New("profile image must be PNG, JPEG, WebP or GIF")
	}
	reader, err := content.Open(ctx, scope, artifact.ContentRef)
	if err != nil {
		return err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != artifact.SizeBytes || len(data) > maximum || http.DetectContentType(data) != artifact.MediaType || !strings.EqualFold("sha256:"+hex.EncodeToString(digest[:]), artifact.Digest) {
		return errors.New("profile image content does not match its recorded type, size or digest")
	}
	return nil
}
