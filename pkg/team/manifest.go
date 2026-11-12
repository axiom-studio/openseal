package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"gopkg.in/yaml.v3"
)

const (
	ManifestAPIVersion = "openseal.dev/team/v1alpha2"
	// retiredManifestAPIVersion described a Team as role slots.
	retiredManifestAPIVersion = "openseal.dev/team/v1alpha1"
	ManifestKind              = "Team"
)

const (
	defaultSharedContextRetention = 7 * 24 * time.Hour
	defaultSharedContextBytes     = 1 << 20
)

var manifestIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// Manifest is the portable, product-neutral Team artifact. It contains
// immutable collaboration behavior and the members by portable Agent
// definition. Scope, the Agent deployments the members become, credentials,
// capacity, and rollout state are deliberately supplied by the installing
// host.
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
	Members             []ManifestMember                `json:"members" yaml:"members"`
	Routing             RoutingPolicy                   `json:"routing,omitempty" yaml:"routing,omitempty"`
	Delegation          ManifestDelegationPolicy        `json:"delegation,omitempty" yaml:"delegation,omitempty"`
	SharedContext       ManifestSharedContextPolicy     `json:"sharedContext,omitempty" yaml:"sharedContext,omitempty"`
	Approvals           ApprovalPolicy                  `json:"approvals,omitempty" yaml:"approvals,omitempty"`
	ObjectiveTemplates  []workforce.ObjectiveTemplate   `json:"objectiveTemplates,omitempty" yaml:"objectiveTemplates,omitempty"`
	Evaluations         []workforce.EvaluationCriterion `json:"evaluations,omitempty" yaml:"evaluations,omitempty"`
	Amendments          workforce.AmendmentPolicy       `json:"amendments,omitempty" yaml:"amendments,omitempty"`
}

// ManifestMember is one member of the Team. AgentDefinitionID is a portable
// reference the installing host resolves to an Agent it deploys or already
// runs.
type ManifestMember struct {
	ID                   string               `json:"id" yaml:"id"`
	DisplayName          string               `json:"displayName" yaml:"displayName"`
	Purpose              string               `json:"purpose" yaml:"purpose"`
	AgentDefinitionID    string               `json:"agentDefinitionId" yaml:"agentDefinitionId"`
	ChannelParticipation ChannelParticipation `json:"channelParticipation,omitempty" yaml:"channelParticipation,omitempty"`
	RequiredSkillIDs     []string             `json:"requiredSkillIds,omitempty" yaml:"requiredSkillIds,omitempty"`
	Approver             bool                 `json:"approver,omitempty" yaml:"approver,omitempty"`
	Lead                 bool                 `json:"lead,omitempty" yaml:"lead,omitempty"`
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
	if manifest.APIVersion == retiredManifestAPIVersion {
		return nil, fmt.Errorf("team manifest apiVersion %s (role slots) is no longer supported; list the Team's members under spec.members and use %s", retiredManifestAPIVersion, ManifestAPIVersion)
	}
	if manifest.APIVersion != ManifestAPIVersion || manifest.Kind != ManifestKind {
		return nil, errors.New("team manifest apiVersion or kind is unsupported")
	}
	if err = validateManifestMembers(manifest.Spec.Members); err != nil {
		return nil, err
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
	definition := &Definition{
		ID: definitionID, Version: metadata.Version, DisplayName: metadata.DisplayName, Purpose: purpose,
		OperatingPrinciples: manifest.Spec.OperatingPrinciples, Routing: manifest.Spec.Routing,
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
	candidate.Approvals.ApproverPrincipals = normalizedStrings(candidate.Approvals.ApproverPrincipals)
	candidate.Amendments.AllowedFields = normalizedStrings(candidate.Amendments.AllowedFields)
	candidate.Amendments.ApproverPrincipals = normalizedStrings(candidate.Amendments.ApproverPrincipals)
	candidate.Routing.Notes = strings.TrimSpace(candidate.Routing.Notes)
}

func validateManifestMembers(members []ManifestMember) error {
	if len(members) == 0 {
		return errors.New("team manifest requires at least one member")
	}
	seen := make(map[string]bool, len(members))
	for _, member := range members {
		id := strings.TrimSpace(member.ID)
		if id == "" || id != member.ID || seen[id] || strings.TrimSpace(member.DisplayName) == "" || strings.TrimSpace(member.Purpose) == "" ||
			strings.TrimSpace(member.AgentDefinitionID) == "" || !validChannelParticipation(member.ChannelParticipation) {
			return errors.New("team manifest members require unique ids, a name, a purpose, an Agent definition, and a valid channel participation")
		}
		seen[id] = true
	}
	return nil
}

// ManifestRoster returns the manifest's members as roster assignments. agents
// maps each member id to the Agent deployment the host placed for it.
func ManifestRoster(manifest *Manifest, agents map[string]string) ([]RosterAssignment, error) {
	if manifest == nil {
		return nil, errors.New("team manifest is required")
	}
	if err := validateManifestMembers(manifest.Spec.Members); err != nil {
		return nil, err
	}
	roster := make([]RosterAssignment, 0, len(manifest.Spec.Members))
	for _, member := range manifest.Spec.Members {
		agentID := strings.TrimSpace(agents[member.ID])
		if agentID == "" {
			return nil, fmt.Errorf("team manifest member %q has no Agent deployment", member.ID)
		}
		participation := member.ChannelParticipation
		if participation == "" {
			participation = ChannelActive
		}
		roster = append(roster, RosterAssignment{
			ID: member.ID, AgentDeploymentID: agentID, DisplayName: strings.TrimSpace(member.DisplayName), Purpose: strings.TrimSpace(member.Purpose),
			ChannelParticipation: participation, RequiredSkillIDs: slices.Clone(member.RequiredSkillIDs),
			Approver: member.Approver, Lead: member.Lead,
		})
	}
	return roster, nil
}
