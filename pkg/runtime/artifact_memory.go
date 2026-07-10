package runtime

import (
	"context"
	"sort"
)

func (s *MemoryStore) CreateArtifactVersion(_ context.Context, artifact *Artifact, expectedLatestVersion int64) error {
	if err := artifact.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := artifactStorageKey(artifact.Scope, artifact.ID)
	versions := s.artifacts[key]
	var latest int64
	for version := range versions {
		if version > latest {
			latest = version
		}
	}
	if latest != expectedLatestVersion {
		return ErrArtifactVersionConflict
	}
	if versions == nil {
		versions = make(map[int64]*Artifact)
		s.artifacts[key] = versions
	}
	if versions[artifact.Version] != nil {
		return ErrArtifactImmutable
	}
	versions[artifact.Version] = cloneArtifact(artifact)
	return nil
}

func (s *MemoryStore) GetArtifact(_ context.Context, scope Scope, id string, version int64) (*Artifact, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	versions := s.artifacts[artifactStorageKey(scope, id)]
	if version == 0 {
		for candidate := range versions {
			if candidate > version {
				version = candidate
			}
		}
	}
	return cloneArtifact(versions[version]), nil
}

func (s *MemoryStore) ListArtifacts(_ context.Context, filter ArtifactFilter) ([]*Artifact, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Artifact, 0)
	for _, versions := range s.artifacts {
		var latest int64
		if filter.LatestOnly {
			for version := range versions {
				if version > latest {
					latest = version
				}
			}
		}
		for version, artifact := range versions {
			if filter.LatestOnly && version != latest {
				continue
			}
			if matchesArtifactFilter(artifact, filter) {
				result = append(result, cloneArtifact(artifact))
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			if result[i].ID == result[j].ID {
				return result[i].Version > result[j].Version
			}
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	start := min(filter.Offset, len(result))
	end := min(start+filter.Limit, len(result))
	return result[start:end], nil
}

var _ ArtifactStore = (*MemoryStore)(nil)

func artifactStorageKey(scope Scope, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}
