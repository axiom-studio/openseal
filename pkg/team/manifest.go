package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"gopkg.in/yaml.v3"
)

const (
	ManifestAPIVersion = "openseal.dev/team/v1alpha1"
	ManifestKind       = "Team"
)

const (
	defaultSharedContextRetention = 7 * 24 * time.Hour
	defaultSharedContextBytes     = 1 << 20
)

var manifestIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// Manifest is the portable, product-neutral Team artifact. It contains
// immutable collaboration behavior and semantic roster requirements. Scope,
// Agent deployment assignments, credentials, capacity, and rollout state are
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
	OperatingPrinciples []string                        `json:"operatingPrinciples,omitempty" yaml:"operatingPrinciples,omitempty"`
	Roles               []ManifestRoleSlot              `json:"roles" yaml:"roles"`
	Coordination        ManifestCoordinationPolicy      `json:"coordination,omitempty" yaml:"coordination,omitempty"`
	Delegation          ManifestDelegationPolicy        `json:"delegation,omitempty" yaml:"delegation,omitempty"`
	SharedContext       ManifestSharedContextPolicy     `json:"sharedContext,omitempty" yaml:"sharedContext,omitempty"`
	Approvals           ApprovalPolicy                  `json:"approvals,omitempty" yaml:"approvals,omitempty"`
	ObjectiveTemplates  []workforce.ObjectiveTemplate   `json:"objectiveTemplates,omitempty" yaml:"objectiveTemplates,omitempty"`
	Evaluations         []workforce.EvaluationCriterion `json:"evaluations,omitempty" yaml:"evaluations,omitempty"`
	Amendments          workforce.AmendmentPolicy       `json:"amendments,omitempty" yaml:"amendments,omitempty"`
}

type ManifestRoleSlot struct {
	ID                    string                   `json:"id" yaml:"id"`
	DisplayName           string                   `json:"displayName" yaml:"displayName"`
	Purpose               string                   `json:"purpose" yaml:"purpose"`
	MinimumMembers        int                      `json:"minimumMembers,omitempty" yaml:"minimumMembers,omitempty"`
	MaximumMembers        int                      `json:"maximumMembers,omitempty" yaml:"maximumMembers,omitempty"`
	RequiredSkillIDs      []string                 `json:"requiredSkillIds,omitempty" yaml:"requiredSkillIds,omitempty"`
	RequiredDefinitionIDs []string                 `json:"requiredDefinitionIds,omitempty" yaml:"requiredDefinitionIds,omitempty"`
	SkillGrants           []ManifestRoleSkillGrant `json:"skillGrants,omitempty" yaml:"skillGrants,omitempty"`
	ChannelParticipation  RoleChannelParticipation `json:"channelParticipation,omitempty" yaml:"channelParticipation,omitempty"`
}

// ManifestRoleSkillGrant excludes catalog and runtime placement identities.
// The installing host resolves those after review without rewriting portable
// Team authority.
type ManifestRoleSkillGrant struct {
	SkillID        string               `json:"skillId" yaml:"skillId"`
	SkillVersion   string               `json:"skillVersion" yaml:"skillVersion"`
	AllowedActions []string             `json:"allowedActions,omitempty" yaml:"allowedActions,omitempty"`
	EnablePrompt   bool                 `json:"enablePrompt,omitempty" yaml:"enablePrompt,omitempty"`
	MaximumRisk    capability.RiskLevel `json:"maximumRisk" yaml:"maximumRisk"`
}

type ManifestCoordinationPolicy struct {
	MaximumSpeakersPerRound  int   `json:"maximumSpeakersPerRound,omitempty" yaml:"maximumSpeakersPerRound,omitempty"`
	QuietByDefault           *bool `json:"quietByDefault,omitempty" yaml:"quietByDefault,omitempty"`
	RequireRoleRelevance     *bool `json:"requireRoleRelevance,omitempty" yaml:"requireRoleRelevance,omitempty"`
	SuppressDuplicateContent *bool `json:"suppressDuplicateContent,omitempty" yaml:"suppressDuplicateContent,omitempty"`
}

type ManifestDelegationPolicy struct {
	MaximumDepth            int   `json:"maximumDepth,omitempty" yaml:"maximumDepth,omitempty"`
	MaximumConcurrent       int   `json:"maximumConcurrent,omitempty" yaml:"maximumConcurrent,omitempty"`
	AllowPeerDelegation     *bool `json:"allowPeerDelegation,omitempty" yaml:"allowPeerDelegation,omitempty"`
	RequireAcceptance       *bool `json:"requireAcceptance,omitempty" yaml:"requireAcceptance,omitempty"`
	RequireCompletionReview *bool `json:"requireCompletionReview,omitempty" yaml:"requireCompletionReview,omitempty"`
	CompletionReviewQuorum  int   `json:"completionReviewQuorum,omitempty" yaml:"completionReviewQuorum,omitempty"`
	EscalateOnDisagreement  *bool `json:"escalateOnDisagreement,omitempty" yaml:"escalateOnDisagreement,omitempty"`
}

type ManifestSharedContextPolicy struct {
	Retention        string `json:"retention,omitempty" yaml:"retention,omitempty"`
	MaximumBytes     int64  `json:"maximumBytes,omitempty" yaml:"maximumBytes,omitempty"`
	AllowMemberRead  *bool  `json:"allowMemberRead,omitempty" yaml:"allowMemberRead,omitempty"`
	AllowMemberWrite *bool  `json:"allowMemberWrite,omitempty" yaml:"allowMemberWrite,omitempty"`
}

// DecodeManifestYAML strictly decodes the public YAML artifact through its
// JSON contract so every host observes the same camelCase representation.
func DecodeManifestYAML(data []byte) (*Manifest, error) {
	var document map[string]interface{}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode team manifest YAML: %w", err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("normalize team manifest YAML: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err = decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode team manifest contract: %w", err)
	}
	return &manifest, nil
}

// CompileManifest converts a portable artifact into one immutable Team
// definition. The installing host owns its final namespaced identity and
// provenance.
func CompileManifest(manifest *Manifest, definitionID string, provenance workforce.DefinitionProvenance) (*Definition, error) {
	if manifest == nil {
		return nil, errors.New("team manifest is required")
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode team manifest: %w", err)
	}
	var portable Manifest
	if err = json.Unmarshal(encoded, &portable); err != nil {
		return nil, fmt.Errorf("decode team manifest: %w", err)
	}
	manifest = &portable
	if manifest.APIVersion != ManifestAPIVersion || manifest.Kind != ManifestKind {
		return nil, errors.New("team manifest apiVersion or kind is unsupported")
	}
	metadata := manifest.Metadata
	metadata.ID = strings.TrimSpace(metadata.ID)
	metadata.Version = strings.TrimSpace(metadata.Version)
	metadata.DisplayName = strings.TrimSpace(metadata.DisplayName)
	if !manifestIDPattern.MatchString(metadata.ID) || !versionPattern.MatchString(metadata.Version) || metadata.DisplayName == "" {
		return nil, errors.New("team manifest requires a valid id, version, and display name")
	}
	definitionID = strings.TrimSpace(definitionID)
	if definitionID == "" {
		definitionID = metadata.ID
	}
	purpose := strings.TrimSpace(metadata.Description)
	if purpose == "" {
		purpose = "Coordinate " + metadata.DisplayName
	}
	retention := defaultSharedContextRetention
	if value := strings.TrimSpace(manifest.Spec.SharedContext.Retention); value != "" {
		retention, err = time.ParseDuration(value)
		if err != nil {
			return nil, fmt.Errorf("team manifest shared context retention: %w", err)
		}
	}
	maximumBytes := manifest.Spec.SharedContext.MaximumBytes
	if maximumBytes == 0 {
		maximumBytes = defaultSharedContextBytes
	}
	coordination := CoordinationPolicy{
		MaximumSpeakersPerRound:  manifest.Spec.Coordination.MaximumSpeakersPerRound,
		QuietByDefault:           booleanDefault(manifest.Spec.Coordination.QuietByDefault, true),
		RequireRoleRelevance:     booleanDefault(manifest.Spec.Coordination.RequireRoleRelevance, true),
		SuppressDuplicateContent: booleanDefault(manifest.Spec.Coordination.SuppressDuplicateContent, true),
	}
	if coordination.MaximumSpeakersPerRound == 0 {
		coordination.MaximumSpeakersPerRound = 2
	}
	delegation := DelegationPolicy{
		MaximumDepth:            manifest.Spec.Delegation.MaximumDepth,
		MaximumConcurrent:       manifest.Spec.Delegation.MaximumConcurrent,
		AllowPeerDelegation:     booleanDefault(manifest.Spec.Delegation.AllowPeerDelegation, false),
		RequireAcceptance:       booleanDefault(manifest.Spec.Delegation.RequireAcceptance, true),
		RequireCompletionReview: booleanDefault(manifest.Spec.Delegation.RequireCompletionReview, true),
		CompletionReviewQuorum:  manifest.Spec.Delegation.CompletionReviewQuorum,
		EscalateOnDisagreement:  booleanDefault(manifest.Spec.Delegation.EscalateOnDisagreement, false),
	}
	if delegation.RequireCompletionReview && delegation.CompletionReviewQuorum == 0 {
		delegation.CompletionReviewQuorum = 1
	}
	if delegation.MaximumDepth == 0 {
		delegation.MaximumDepth = 2
	}
	if delegation.MaximumConcurrent == 0 {
		delegation.MaximumConcurrent = 4
	}
	approvals := manifest.Spec.Approvals
	if approvals.MaximumRisk == "" {
		approvals.MaximumRisk = capability.RiskLevelRead
	}
	roles := make([]RoleSlot, len(manifest.Spec.Roles))
	for index, portableRole := range manifest.Spec.Roles {
		participation := portableRole.ChannelParticipation
		if participation == "" {
			participation = RoleChannelActive
		}
		grants := make([]RoleSkillGrant, len(portableRole.SkillGrants))
		for grantIndex, grant := range portableRole.SkillGrants {
			grants[grantIndex] = RoleSkillGrant{
				SkillID: grant.SkillID, SkillVersion: grant.SkillVersion, AllowedActions: grant.AllowedActions,
				EnablePrompt: grant.EnablePrompt, MaximumRisk: grant.MaximumRisk,
			}
		}
		roles[index] = RoleSlot{
			ID: portableRole.ID, DisplayName: portableRole.DisplayName, Purpose: portableRole.Purpose,
			MinimumMembers: portableRole.MinimumMembers, MaximumMembers: portableRole.MaximumMembers,
			RequiredSkillIDs: portableRole.RequiredSkillIDs, RequiredDefinitionIDs: portableRole.RequiredDefinitionIDs,
			SkillGrants: grants, ChannelParticipation: participation,
		}
	}
	definition := &Definition{
		ID: definitionID, Version: metadata.Version, DisplayName: metadata.DisplayName, Purpose: purpose,
		OperatingPrinciples: manifest.Spec.OperatingPrinciples, Roles: roles, Coordination: coordination,
		Delegation: delegation,
		SharedContext: workforce.SharedContextPolicy{
			Retention: retention, MaximumBytes: maximumBytes,
			AllowMemberRead:  booleanDefault(manifest.Spec.SharedContext.AllowMemberRead, true),
			AllowMemberWrite: booleanDefault(manifest.Spec.SharedContext.AllowMemberWrite, true),
		},
		Approvals: approvals, ObjectiveTemplates: manifest.Spec.ObjectiveTemplates,
		Evaluations: manifest.Spec.Evaluations, Amendments: manifest.Spec.Amendments, Provenance: provenance,
	}
	canonicalizeManifestDefinition(definition)
	if err := definition.Validate(); err != nil {
		return nil, err
	}
	return definition, nil
}

func booleanDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func canonicalizeManifestDefinition(candidate *Definition) {
	candidate.ID = strings.TrimSpace(candidate.ID)
	candidate.Version = strings.TrimSpace(candidate.Version)
	candidate.DisplayName = strings.TrimSpace(candidate.DisplayName)
	candidate.Purpose = strings.TrimSpace(candidate.Purpose)
	candidate.OperatingPrinciples = normalizedStrings(candidate.OperatingPrinciples)
	candidate.Approvals.ApproverRoleIDs = normalizedStrings(candidate.Approvals.ApproverRoleIDs)
	candidate.Approvals.ApproverPrincipals = normalizedStrings(candidate.Approvals.ApproverPrincipals)
	candidate.Amendments.AllowedFields = normalizedStrings(candidate.Amendments.AllowedFields)
	candidate.Amendments.ApproverPrincipals = normalizedStrings(candidate.Amendments.ApproverPrincipals)
	for index := range candidate.Roles {
		role := &candidate.Roles[index]
		role.ID = strings.TrimSpace(role.ID)
		role.DisplayName = strings.TrimSpace(role.DisplayName)
		role.Purpose = strings.TrimSpace(role.Purpose)
		role.RequiredSkillIDs = normalizedStrings(role.RequiredSkillIDs)
		role.RequiredDefinitionIDs = normalizedStrings(role.RequiredDefinitionIDs)
		for grantIndex := range role.SkillGrants {
			grant := &role.SkillGrants[grantIndex]
			grant.SkillID = strings.TrimSpace(grant.SkillID)
			grant.SkillVersion = strings.TrimSpace(grant.SkillVersion)
			grant.AllowedActions = normalizedStrings(grant.AllowedActions)
		}
	}
}
