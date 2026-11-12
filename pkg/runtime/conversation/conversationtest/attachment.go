package conversationtest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

// TextContent is artifact content that serves Text. It counts the opens
// and records the scope of the last one.
type TextContent struct {
	Text  string
	Opens int
	Scope models.Scope
}

func (s *TextContent) Put(context.Context, models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	return models.ArtifactStoredContent{}, errors.New("not supported")
}

func (s *TextContent) Open(_ context.Context, scope models.Scope, _ string) (io.ReadCloser, error) {
	s.Opens++
	s.Scope = scope
	return io.NopCloser(strings.NewReader(s.Text)), nil
}

// Digest is the artifact digest of content.
func Digest(content string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(content)))
}

// CSVArtifact is internal artifact file version 1 in tenant one, named
// file.csv; the test sets its media type, size and digest.
func CSVArtifact() *models.Artifact {
	return &models.Artifact{ID: "file", Version: 1, Scope: models.Scope{Kind: "tenant", ID: "one"},
		Name: "file.csv", ContentRef: "object-store:file", Classification: models.ArtifactClassificationInternal,
		Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "user", ID: "owner"}}}
}
