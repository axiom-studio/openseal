package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"gopkg.in/yaml.v3"
)

const (
	ManifestAPIVersion = "openseal.dev/agent/v1alpha1"
	ManifestKind       = "Agent"
)

var manifestIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// Manifest is the portable, product-neutral Agent artifact. It contains only
// immutable behavior and capability requirements. Tenant scope, credentials,
// placement, rollout state, and capacity belong to AgentDeployment and are
// deliberately supplied by the installing host.
type Manifest struct {
	APIVersion string           `json:"apiVersion" yaml:"apiVersion"`
	Kind       string           `json:"kind" yaml:"kind"`
	Metadata   ManifestMetadata `json:"metadata" yaml:"metadata"`
	Spec       ManifestSpec     `json:"spec" yaml:"spec"`
}

type ManifestMetadata struct {
	ID          string   `json:"id" yaml:"id"`
	Version     string   `json:"version" yaml:"version"`
	DisplayName string   `json:"displayName" yaml:"displayName"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
	Tags        []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	License     string   `json:"license,omitempty" yaml:"license,omitempty"`
	Homepage    string   `json:"homepage,omitempty" yaml:"homepage,omitempty"`
}

type ManifestSpec struct {
	SystemPrompt        string                 `json:"systemPrompt,omitempty" yaml:"systemPrompt,omitempty"`
	Personality         string                 `json:"personality,omitempty" yaml:"personality,omitempty"`
	OperatingPrinciples []string               `json:"operatingPrinciples,omitempty" yaml:"operatingPrinciples,omitempty"`
	DomainContext       map[string]interface{} `json:"domainContext,omitempty" yaml:"domainContext,omitempty"`
	SkillRequirements   []SkillRequirement     `json:"skillRequirements,omitempty" yaml:"skillRequirements,omitempty"`
	Authority           AuthorityPolicy        `json:"authority,omitempty" yaml:"authority,omitempty"`
	Memory              MemoryPolicy           `json:"memory,omitempty" yaml:"memory,omitempty"`
	Escalation          EscalationPolicy       `json:"escalation,omitempty" yaml:"escalation,omitempty"`
	ObjectiveTemplates  []ObjectiveTemplate    `json:"objectiveTemplates,omitempty" yaml:"objectiveTemplates,omitempty"`
	Evaluations         []EvaluationCriterion  `json:"evaluations,omitempty" yaml:"evaluations,omitempty"`
	Runbook             *runbook.Definition    `json:"runbook,omitempty" yaml:"runbook,omitempty"`
	Amendments          AmendmentPolicy        `json:"amendments,omitempty" yaml:"amendments,omitempty"`
}

// DecodeManifestYAML decodes the public YAML artifact through its JSON
// contract. Nested portable types intentionally remain JSON-native, so this
// bridge provides one strict camelCase representation across every host.
func DecodeManifestYAML(data []byte) (*Manifest, error) {
	var document map[string]interface{}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode agent manifest YAML: %w", err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("normalize agent manifest YAML: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err = decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode agent manifest contract: %w", err)
	}
	return &manifest, nil
}

// CompileManifest converts a portable artifact into one immutable definition.
// The installing host owns the final namespaced identity and provenance.
func CompileManifest(manifest *Manifest, definitionID string, provenance DefinitionProvenance) (*AgentDefinition, error) {
	if manifest == nil {
		return nil, errors.New("agent manifest is required")
	}
	// The returned definition must remain immutable even if an installer reuses
	// or mutates its decoded manifest. A typed round trip also rejects values
	// that cannot be represented by the public artifact contract.
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode agent manifest: %w", err)
	}
	var portable Manifest
	if err = json.Unmarshal(encoded, &portable); err != nil {
		return nil, fmt.Errorf("decode agent manifest: %w", err)
	}
	manifest = &portable
	if manifest.APIVersion != ManifestAPIVersion || manifest.Kind != ManifestKind {
		return nil, errors.New("agent manifest apiVersion or kind is unsupported")
	}
	metadata := manifest.Metadata
	metadata.ID = strings.TrimSpace(metadata.ID)
	metadata.Version = strings.TrimSpace(metadata.Version)
	metadata.DisplayName = strings.TrimSpace(metadata.DisplayName)
	if !manifestIDPattern.MatchString(metadata.ID) || !versionPattern.MatchString(metadata.Version) || metadata.DisplayName == "" {
		return nil, errors.New("agent manifest requires a valid id, version, and display name")
	}
	definitionID = strings.TrimSpace(definitionID)
	if definitionID == "" {
		definitionID = metadata.ID
	}
	purpose := strings.TrimSpace(metadata.Description)
	if purpose == "" {
		purpose = "Operate as " + metadata.DisplayName
	}
	systemPrompt := strings.TrimSpace(manifest.Spec.SystemPrompt)
	if systemPrompt == "" {
		systemPrompt = "You are " + metadata.DisplayName + ". " + purpose
	}
	authority := manifest.Spec.Authority
	if authority.MaximumRisk == "" {
		authority.MaximumRisk = capability.RiskLevelRead
	}
	if authority.MaxConcurrentRuns == 0 {
		authority.MaxConcurrentRuns = 1
	}
	if authority.AllowedSkillIDs == nil {
		authority.AllowedSkillIDs = make([]string, 0, len(manifest.Spec.SkillRequirements))
		for _, requirement := range manifest.Spec.SkillRequirements {
			authority.AllowedSkillIDs = append(authority.AllowedSkillIDs, requirement.SkillID)
		}
	}
	definition := &AgentDefinition{
		ID: definitionID, Version: metadata.Version, DisplayName: metadata.DisplayName, Purpose: purpose,
		SystemPrompt: systemPrompt, Personality: strings.TrimSpace(manifest.Spec.Personality),
		OperatingPrinciples: manifest.Spec.OperatingPrinciples,
		DomainContext:       manifest.Spec.DomainContext,
		SkillRequirements:   manifest.Spec.SkillRequirements, Authority: authority,
		Memory: manifest.Spec.Memory, Escalation: manifest.Spec.Escalation,
		ObjectiveTemplates: manifest.Spec.ObjectiveTemplates,
		Evaluations:        manifest.Spec.Evaluations,
		Runbook:            manifest.Spec.Runbook, Amendments: manifest.Spec.Amendments, Provenance: provenance,
	}
	canonicalizeDefinition(definition)
	if err := definition.Validate(); err != nil {
		return nil, err
	}
	return definition, nil
}
