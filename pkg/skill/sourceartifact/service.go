package sourceartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelskill "github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/openclaw"
)

type Service struct {
	store Store
	now   func() time.Time
}

type ImportOpenClawRequest struct {
	Scope              capability.ScopeReference
	Compilation        *openclaw.Compilation
	ReferenceID        string
	ReferenceKind      string
	ReferenceExpiresAt *time.Time
}

func NewService(store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("skill source artifact store is required")
	}
	return &Service{store: store, now: time.Now}, nil
}

func (s *Service) ImportOpenClaw(ctx context.Context, request ImportOpenClawRequest) (*Artifact, bool, error) {
	if s == nil || s.store == nil {
		return nil, false, errors.New("skill source artifact service is not configured")
	}
	bundle, err := openclaw.ExportBundle(request.Compilation)
	if err != nil {
		return nil, false, err
	}
	now := s.now().UTC()
	artifact := artifactFromBundle(request.Scope, request.Compilation.SourceDigest, bundle, now)
	reference := &Reference{
		Scope: request.Scope, Digest: artifact.Digest, ID: request.ReferenceID,
		Kind: request.ReferenceKind, CreatedAt: now, ExpiresAt: request.ReferenceExpiresAt,
	}
	if err := ValidateArtifact(artifact); err != nil {
		return nil, false, err
	}
	if err := ValidateReference(reference); err != nil {
		return nil, false, err
	}
	created, err := s.store.ImportSourceArtifact(ctx, artifact, reference)
	if err != nil {
		return nil, false, err
	}
	if !created {
		persisted, err := s.store.GetSourceArtifact(ctx, request.Scope, artifact.Digest)
		if err != nil {
			return nil, false, err
		}
		if persisted == nil || !EquivalentArtifacts(persisted, artifact) {
			return nil, false, ErrImmutable
		}
		return CloneArtifact(persisted), false, nil
	}
	return CloneArtifact(artifact), created, nil
}

func (s *Service) Get(ctx context.Context, scope capability.ScopeReference, digest string) (*Artifact, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("skill source artifact service is not configured")
	}
	artifact, err := s.store.GetSourceArtifact(ctx, scope, digest)
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, ErrNotFound
	}
	if err := ValidateArtifact(artifact); err != nil {
		return nil, fmt.Errorf("stored skill source artifact is invalid: %w", err)
	}
	return CloneArtifact(artifact), nil
}

func (s *Service) ExportOpenClaw(ctx context.Context, scope capability.ScopeReference, digest string) (openclaw.Bundle, error) {
	artifact, err := s.Get(ctx, scope, digest)
	if err != nil {
		return openclaw.Bundle{}, err
	}
	if artifact.Format != FormatOpenClawSkillV1 {
		return openclaw.Bundle{}, fmt.Errorf("skill source artifact %s is not OpenClaw format", artifact.Digest)
	}
	return artifactBundle(artifact)
}

func (s *Service) ReadResource(ctx context.Context, scope capability.ScopeReference, sourceDigest, resourcePath string) ([]byte, error) {
	artifact, err := s.Get(ctx, scope, sourceDigest)
	if err != nil {
		return nil, err
	}
	resourcePath, err = normalizePath(resourcePath)
	if err != nil || resourcePath == artifact.EntryPoint {
		return nil, errors.New("resource path is not a safe supporting source file")
	}
	for _, file := range artifact.Files {
		if file.Path == resourcePath {
			if int64(len(file.Content)) != file.Size || contentDigest(file.Content) != file.Digest {
				return nil, errors.New("stored skill resource failed integrity verification")
			}
			return bytes.Clone(file.Content), nil
		}
	}
	return nil, fmt.Errorf("resource %q is not present in source artifact", resourcePath)
}

func (s *Service) Release(ctx context.Context, scope capability.ScopeReference, digest, referenceID string) error {
	if s == nil || s.store == nil {
		return errors.New("skill source artifact service is not configured")
	}
	return s.store.DeleteSourceArtifactReference(ctx, scope, digest, referenceID)
}

func (s *Service) GarbageCollect(ctx context.Context, now time.Time, unreferencedGrace time.Duration, limit int) (*GarbageCollectionReport, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("skill source artifact service is not configured")
	}
	if now.IsZero() || unreferencedGrace < 0 {
		return nil, errors.New("garbage collection time and non-negative grace are required")
	}
	if limit <= 0 {
		limit = 100
	}
	expired, err := s.store.PurgeExpiredSourceArtifactReferences(ctx, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	keys, err := s.store.ListUnreferencedSourceArtifacts(ctx, now.UTC().Add(-unreferencedGrace), limit)
	if err != nil {
		return nil, err
	}
	report := &GarbageCollectionReport{ExpiredReferences: expired}
	for _, key := range keys {
		deleted, err := s.store.DeleteSourceArtifactIfUnreferenced(ctx, key, now.UTC().Add(-unreferencedGrace))
		if err != nil {
			return nil, err
		}
		if deleted {
			report.DeletedArtifacts++
		}
	}
	return report, nil
}

var _ kernelskill.ResourceContentProvider = (*Service)(nil)

func artifactFromBundle(scope capability.ScopeReference, digest string, bundle openclaw.Bundle, createdAt time.Time) *Artifact {
	files := make([]File, 0, len(bundle.Files)+1)
	files = append(files, sourceFile("SKILL.md", bundle.SkillMD))
	for _, file := range bundle.Files {
		files = append(files, sourceFile(file.Path, file.Content))
	}
	return &Artifact{
		Scope: scope, Digest: digest, Format: FormatOpenClawSkillV1, EntryPoint: "SKILL.md", CreatedAt: createdAt,
		Origin: Origin{
			Registry: bundle.Source.Registry, Publisher: bundle.Source.Publisher, Reference: bundle.Source.Reference,
			ExpectedName: bundle.Source.ExpectedName, Version: bundle.Source.Version, Trust: cloneMap(bundle.Source.Trust),
		},
		Files: files,
	}
}

func sourceFile(filePath string, content []byte) File {
	return File{Path: filePath, Digest: contentDigest(content), Size: int64(len(content)), Content: bytes.Clone(content)}
}

func contentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
