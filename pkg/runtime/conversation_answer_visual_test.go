package runtime

import (
	"bytes"
	"context"
	"io"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

type visualTurnHost struct{ visual *presentation.Visual }

type answerVisualContent struct {
	puts int
	data []byte
}

func (s *answerVisualContent) Put(_ context.Context, req ArtifactContentWrite) (ArtifactStoredContent, error) {
	s.puts++
	s.data, _ = io.ReadAll(req.Reader)
	return ArtifactStoredContent{ContentRef: "visual-content", SizeBytes: int64(len(s.data)), Digest: hashBytes(s.data)}, nil
}

func (s *answerVisualContent) Open(context.Context, Scope, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}
