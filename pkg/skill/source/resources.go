package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	kernelskill "github.com/axiom-studio/openseal/pkg/skill"
	skillopenclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
)

// ArtifactProvider exposes only supporting resource bytes from the immutable
// effective source snapshot. SKILL.md and shadowed candidates are deliberately
// not addressable through the activation staging boundary.
type ArtifactProvider struct {
	resources map[string][]byte
}

var _ kernelskill.ResourceContentProvider = (*ArtifactProvider)(nil)

func NewArtifactProvider(snapshot *Snapshot) (*ArtifactProvider, error) {
	if snapshot == nil {
		return nil, errors.New("skill source snapshot is required")
	}
	provider := &ArtifactProvider{resources: make(map[string][]byte)}
	for _, candidate := range snapshot.Effective {
		if candidate.Compilation == nil || candidate.Compilation.Definition == nil {
			return nil, fmt.Errorf("effective skill %q has no compiled artifact", candidate.Name)
		}
		bundle, err := skillopenclaw.ExportBundle(candidate.Compilation)
		if err != nil {
			return nil, fmt.Errorf("export effective skill %q: %w", candidate.Name, err)
		}
		declared := make(map[string]struct {
			digest string
			size   int64
		}, len(candidate.Compilation.Definition.Resources))
		seen := make(map[string]bool, len(candidate.Compilation.Definition.Resources))
		for _, resource := range candidate.Compilation.Definition.Resources {
			declared[resource.Path] = struct {
				digest string
				size   int64
			}{digest: resource.Digest, size: resource.Size}
		}
		for _, file := range bundle.Files {
			resourcePath := path.Clean(strings.ReplaceAll(strings.TrimSpace(file.Path), "\\", "/"))
			metadata, ok := declared[resourcePath]
			if !ok {
				continue
			}
			digest := sha256.Sum256(file.Content)
			if int64(len(file.Content)) != metadata.size || hex.EncodeToString(digest[:]) != metadata.digest {
				return nil, fmt.Errorf("effective skill %q resource %q does not match its compiled declaration", candidate.Name, resourcePath)
			}
			key := artifactResourceKey(candidate.Compilation.SourceDigest, resourcePath)
			provider.resources[key] = append([]byte(nil), file.Content...)
			seen[resourcePath] = true
		}
		for resourcePath := range declared {
			if !seen[resourcePath] {
				return nil, fmt.Errorf("effective skill %q resource %q is missing from its retained source artifact", candidate.Name, resourcePath)
			}
		}
	}
	return provider, nil
}

func (p *ArtifactProvider) ReadResource(ctx context.Context, sourceDigest, resourcePath string) ([]byte, error) {
	if p == nil {
		return nil, errors.New("skill source artifact provider is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resourcePath = path.Clean(strings.ReplaceAll(strings.TrimSpace(resourcePath), "\\", "/"))
	if resourcePath == "." || path.IsAbs(resourcePath) || resourcePath == ".." || strings.HasPrefix(resourcePath, "../") {
		return nil, errors.New("resource path is not a safe relative path")
	}
	content, ok := p.resources[artifactResourceKey(strings.TrimSpace(sourceDigest), resourcePath)]
	if !ok {
		return nil, fmt.Errorf("resource %q is not present in the effective source snapshot", resourcePath)
	}
	return append([]byte(nil), content...), nil
}

func artifactResourceKey(sourceDigest, resourcePath string) string {
	return sourceDigest + "\x00" + resourcePath
}
