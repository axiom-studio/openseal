package collaboration

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
)

func validateArtifactReferenceIntents(requirements []models.ArtifactRequirement, references []models.ArtifactReference) error {
	requirementsByName := make(map[string]models.ArtifactRequirement, len(requirements))
	for _, requirement := range requirements {
		requirementsByName[strings.TrimSpace(requirement.Name)] = requirement
	}
	seen := make(map[string]struct{}, len(references))
	provided := make(map[string]struct{}, len(references))
	for index, reference := range references {
		id := strings.TrimSpace(reference.ID)
		if id == "" || reference.Version <= 0 {
			return fmt.Errorf("artifact reference %d id and positive version are required", index)
		}
		key := fmt.Sprintf("%s:%d", id, reference.Version)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("artifact reference %q version %d is duplicated", id, reference.Version)
		}
		seen[key] = struct{}{}
		if reference.Name != "" || reference.Type != "" || reference.MediaType != "" || reference.ContentRef != "" ||
			reference.Digest != "" || reference.SizeBytes != 0 || reference.Metadata != nil || len(reference.EvidenceRefs) > 0 {
			return fmt.Errorf("artifact reference %q must contain only id, version, and requirementName", id)
		}
		requirementName := strings.TrimSpace(reference.RequirementName)
		if requirementName == "" {
			continue
		}
		if _, exists := requirementsByName[requirementName]; !exists {
			return fmt.Errorf("artifact reference %q names unknown requirement %q", id, requirementName)
		}
		provided[requirementName] = struct{}{}
	}
	for _, requirement := range requirements {
		if requirement.Required {
			if _, exists := provided[strings.TrimSpace(requirement.Name)]; !exists {
				return fmt.Errorf("required artifact %q was not provided", requirement.Name)
			}
		}
	}
	return nil
}

func (s *Service) resolveArtifactReferences(ctx context.Context, scope models.Scope, requirements []models.ArtifactRequirement, references []models.ArtifactReference) ([]models.ArtifactReference, error) {
	if err := validateArtifactReferenceIntents(requirements, references); err != nil {
		return nil, err
	}
	resolved := make([]models.ArtifactReference, 0, len(references))
	for _, reference := range references {
		artifact, err := s.artifacts.GetArtifact(ctx, scope, strings.TrimSpace(reference.ID), reference.Version)
		if err != nil {
			return nil, err
		}
		if artifact == nil {
			return nil, fmt.Errorf("%w: %s version %d", artifactcatalog.ErrArtifactNotFound, reference.ID, reference.Version)
		}
		evidenceRefs := make([]string, 0, len(artifact.Evidence))
		for _, evidence := range artifact.Evidence {
			evidenceRefs = append(evidenceRefs, evidence.TargetRef)
		}
		resolved = append(resolved, models.ArtifactReference{
			ID: artifact.ID, Version: artifact.Version, RequirementName: strings.TrimSpace(reference.RequirementName),
			Name: artifact.Name, Type: artifact.Type, MediaType: artifact.MediaType, ContentRef: artifact.ContentRef,
			Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, Metadata: maps.Clone(artifact.Metadata), EvidenceRefs: evidenceRefs,
		})
	}
	if err := models.ValidateArtifactReferences(requirements, resolved); err != nil {
		return nil, err
	}
	return normalizeArtifactReferences(resolved), nil
}

func normalizeArtifactReferences(in []models.ArtifactReference) []models.ArtifactReference {
	result := models.CloneArtifactReferences(in)
	for index := range result {
		result[index].ID = strings.TrimSpace(result[index].ID)
		result[index].RequirementName = strings.TrimSpace(result[index].RequirementName)
		result[index].Name = strings.TrimSpace(result[index].Name)
		result[index].Type = strings.TrimSpace(result[index].Type)
		result[index].MediaType = strings.TrimSpace(result[index].MediaType)
		result[index].ContentRef = strings.TrimSpace(result[index].ContentRef)
		result[index].Digest = strings.ToLower(strings.TrimSpace(result[index].Digest))
		for evidenceIndex := range result[index].EvidenceRefs {
			result[index].EvidenceRefs[evidenceIndex] = strings.TrimSpace(result[index].EvidenceRefs[evidenceIndex])
		}
	}
	return result
}
