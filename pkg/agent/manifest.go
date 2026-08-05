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
	Channels            []ChannelRoute         `json:"channels,omitempty" yaml:"channels,omitempty"`
	Amendments          AmendmentPolicy        `json:"amendments,omitempty" yaml:"amendments,omitempty"`
}

// ManifestExportRequest projects one immutable deployed definition back into
// its portable artifact. Metadata.ID is deliberately supplied by the caller:
// definition IDs are host-scoped and must never become portable identity by
// accident. DeploymentID is used only to restore exact self references.
type ManifestExportRequest struct {
	Definition   *AgentDefinition
	DeploymentID string
	Metadata     ManifestMetadata
}

// ExportManifest is the inverse of CompileManifest for immutable Agent
// behavior. It strips host provenance and materialized deployment identity,
// while preserving every portable behavior, authority, Skill, Runbook, and
// channel-routing fact.
func ExportManifest(request ManifestExportRequest) (*Manifest, error) {
	if request.Definition == nil {
		return nil, errors.New("agent definition is required")
	}
	definition := cloneDefinition(request.Definition)
	if err := definition.Validate(); err != nil {
		return nil, fmt.Errorf("export agent definition: %w", err)
	}
	metadata := request.Metadata
	metadata.ID = strings.TrimSpace(metadata.ID)
	if metadata.ID == "" || !manifestIDPattern.MatchString(metadata.ID) {
		return nil, errors.New("agent manifest export requires a portable metadata id")
	}
	if metadata.Version == "" {
		metadata.Version = definition.Version
	}
	if metadata.Version != definition.Version {
		return nil, errors.New("agent manifest export version must match the immutable definition")
	}
	if metadata.DisplayName == "" {
		metadata.DisplayName = definition.DisplayName
	}
	if metadata.Description == "" {
		metadata.Description = definition.Purpose
	}
	if err := restorePortableSelfReferences(definition, strings.TrimSpace(request.DeploymentID)); err != nil {
		return nil, err
	}
	manifest := &Manifest{
		APIVersion: ManifestAPIVersion,
		Kind:       ManifestKind,
		Metadata:   metadata,
		Spec: ManifestSpec{
			SystemPrompt: definition.SystemPrompt, Personality: definition.Personality,
			OperatingPrinciples: definition.OperatingPrinciples, DomainContext: definition.DomainContext,
			SkillRequirements: definition.SkillRequirements, Authority: definition.Authority,
			Memory: definition.Memory, Escalation: definition.Escalation,
			ObjectiveTemplates: definition.ObjectiveTemplates, Evaluations: definition.Evaluations,
			Runbook: definition.Runbook, Channels: definition.Channels, Amendments: definition.Amendments,
		},
	}
	if _, err := CompileManifest(manifest, metadata.ID, DefinitionProvenance{}); err != nil {
		return nil, fmt.Errorf("validate exported agent manifest: %w", err)
	}
	return manifest, nil
}

// EncodeManifestYAML emits the same strict camelCase contract accepted by
// DecodeManifestYAML. The typed clone prevents callers from mutating the
// artifact while it is being encoded.
func EncodeManifestYAML(manifest *Manifest) ([]byte, error) {
	if manifest == nil {
		return nil, errors.New("agent manifest is required")
	}
	if _, err := CompileManifest(manifest, manifest.Metadata.ID, DefinitionProvenance{}); err != nil {
		return nil, err
	}
	jsonDocument, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("normalize agent manifest YAML: %w", err)
	}
	var document map[string]interface{}
	if err = json.Unmarshal(jsonDocument, &document); err != nil {
		return nil, fmt.Errorf("normalize agent manifest YAML: %w", err)
	}
	encoded, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode agent manifest YAML: %w", err)
	}
	return encoded, nil
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
		Runbook:            manifest.Spec.Runbook, Channels: manifest.Spec.Channels,
		Amendments: manifest.Spec.Amendments, Provenance: provenance,
	}
	canonicalizeDefinition(definition)
	if err := definition.Validate(); err != nil {
		return nil, err
	}
	return definition, nil
}

func restorePortableSelfReferences(definition *AgentDefinition, deploymentID string) error {
	if definition == nil || definition.Runbook == nil {
		return nil
	}
	for id, step := range definition.Runbook.Steps {
		if step.Delegate == nil || len(step.Delegate.AgentID.Literal) == 0 {
			continue
		}
		var target string
		if err := json.Unmarshal(step.Delegate.AgentID.Literal, &target); err != nil {
			return fmt.Errorf("decode Runbook delegate Agent identity at step %s: %w", id, err)
		}
		if deploymentID != "" && target == deploymentID {
			step.Delegate.AgentID.Literal, _ = json.Marshal("$self")
			definition.Runbook.Steps[id] = step
		}
	}
	objectivePrefix := "agent:" + definition.ID + ":"
	for id, trigger := range definition.Runbook.Triggers {
		if strings.HasPrefix(trigger.ObjectiveID, objectivePrefix) {
			trigger.ObjectiveID = "agent:$self:" + strings.TrimPrefix(trigger.ObjectiveID, objectivePrefix)
			definition.Runbook.Triggers[id] = trigger
		}
	}
	return nil
}
