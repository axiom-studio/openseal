package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
	"io"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
)

type profileImageFixture struct {
	ArtifactStore
	ArtifactContentStore
	artifact *Artifact
	data     []byte
	opened   bool
}

func (f *profileImageFixture) GetArtifact(_ context.Context, scope Scope, id string, version int64) (*Artifact, error) {
	if scope != f.artifact.Scope || id != f.artifact.ID || version != f.artifact.Version {
		return nil, ErrArtifactNotFound
	}
	return f.artifact, nil
}
func (f *profileImageFixture) Open(_ context.Context, scope Scope, ref string) (io.ReadCloser, error) {
	if scope != f.artifact.Scope || ref != f.artifact.ContentRef {
		return nil, ErrArtifactNotFound
	}
	f.opened = true
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func TestProfileImageValidatesScopedContent(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	digest := sha256.Sum256(data)
	scope := Scope{Kind: "tenant", ID: "one"}
	reference := &agent.ProfileImage{ArtifactID: "portrait", Version: 1}
	for _, test := range []struct {
		name    string
		mutate  func(*profileImageFixture)
		foreign bool
		valid   bool
	}{
		{name: "valid", valid: true},
		{name: "unprojected availability", valid: true, mutate: func(f *profileImageFixture) { f.artifact.ContentAvailability = "" }},
		{name: "foreign scope", foreign: true},
		{name: "wrong MIME", mutate: func(f *profileImageFixture) { f.artifact.MediaType = "image/svg+xml" }},
		{name: "unavailable", mutate: func(f *profileImageFixture) { f.artifact.ContentAvailability = "unavailable" }},
		{name: "oversized", mutate: func(f *profileImageFixture) { f.artifact.SizeBytes = 2 << 20 }},
		{name: "forged content", mutate: func(f *profileImageFixture) { f.data = []byte("not an image") }},
		{name: "wrong digest", mutate: func(f *profileImageFixture) { f.artifact.Digest = "wrong" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &profileImageFixture{artifact: &Artifact{ID: "portrait", Version: 1, Scope: scope, ContentRef: "content:portrait", MediaType: "image/png", SizeBytes: int64(len(data)), Digest: "sha256:" + hex.EncodeToString(digest[:]), ContentAvailability: ArtifactContentAvailable}, data: data}
			if test.mutate != nil {
				test.mutate(f)
			}
			requestedScope := scope
			if test.foreign {
				requestedScope.ID = "other"
			}
			err := ValidateAgentProfileImage(context.Background(), f, f, requestedScope, reference)
			if (err == nil) != test.valid {
				t.Fatalf("validation error = %v", err)
			}
			if test.foreign && f.opened {
				t.Fatal("foreign content opened")
			}
		})
	}
}
