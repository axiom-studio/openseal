package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

var ErrManifestInstallationConflict = errors.New("agent manifest installation conflicts with an existing resource")

// ManifestInstallationRequest installs one reviewed, portable Agent manifest
// into a stable deployment. Credential values are never part of Manifest;
// Deployment may contain only opaque credential references supplied by the
// embedding host.
type ManifestInstallationRequest struct {
	Manifest   *Manifest        `json:"manifest"`
	Deployment *AgentDeployment `json:"deployment"`
	// DefinitionKey optionally gives an embedding compiler a stable logical
	// identity for the materialized definition. Portable bundle imports use a
	// target-deployment-specific key because self references and channel routes
	// become target-specific immutable behavior. Plain manifest installations
	// continue to use Manifest.Metadata.ID when this is empty.
	DefinitionKey string `json:"definitionKey,omitempty"`
	// EndpointIDs maps portable logical channel keys to target-host endpoint
	// identities. The installing host creates those endpoint resources; the
	// kernel materializes the same exact references into immutable behavior.
	EndpointIDs    map[string]string `json:"endpointIds,omitempty"`
	ActorType      string            `json:"actorType"`
	ActorID        string            `json:"actorId"`
	Reason         string            `json:"reason,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey"`
}

type ManifestInstallationResult struct {
	Definition *AgentDefinition `json:"definition"`
	Deployment *AgentDeployment `json:"deployment"`
	Replayed   bool             `json:"replayed"`
}

// ManifestInstallationRegistry is the narrow durable boundary needed by a
// portable Agent installation. Both the standalone kernel and embedding hosts
// implement it with their canonical Agent registry.
type ManifestInstallationRegistry interface {
	ListAgentDefinitionVersions(context.Context, string) ([]*AgentDefinition, error)
	GetAgentDefinition(context.Context, string, string) (*AgentDefinition, error)
	RegisterAgentDefinition(context.Context, *AgentDefinition) (*AgentDefinition, error)
	GetAgentDeployment(context.Context, capability.ScopeReference, string) (*AgentDeployment, error)
	CreateAgentDeployment(context.Context, *AgentDeployment, string, string, string) (*AgentDeployment, *DefinitionActivation, error)
	ActivateAgentDefinition(context.Context, capability.ScopeReference, string, string, int64, string, string, string) (*AgentDeployment, *DefinitionActivation, error)
}

// InstallManifest creates or converges one Agent definition/deployment pair.
// Replays are identified by immutable resource identity and content, so a
// process restart does not require client-side state. Existing mutable
// placement is never silently overwritten: drift fails explicitly.
func InstallManifest(ctx context.Context, registry ManifestInstallationRegistry, request ManifestInstallationRequest) (*ManifestInstallationResult, error) {
	if registry == nil || request.Manifest == nil || request.Deployment == nil {
		return nil, errors.New("agent manifest, deployment, and registry are required")
	}
	request.ActorType = strings.TrimSpace(request.ActorType)
	request.ActorID = strings.TrimSpace(request.ActorID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.ActorType == "" || request.ActorID == "" || request.IdempotencyKey == "" {
		return nil, errors.New("agent manifest installation actor and idempotency key are required")
	}
	if len(request.IdempotencyKey) > 255 {
		return nil, errors.New("agent manifest installation idempotency key must not exceed 255 characters")
	}

	desiredDeployment := cloneDeployment(request.Deployment)
	definitionKey := strings.TrimSpace(request.DefinitionKey)
	if definitionKey == "" {
		definitionKey = request.Manifest.Metadata.ID
	}
	definitionID, err := manifestDefinitionID(desiredDeployment.Scope, definitionKey)
	if err != nil {
		return nil, err
	}
	if supplied := strings.TrimSpace(desiredDeployment.DefinitionID); supplied != "" && supplied != definitionID {
		return nil, errors.New("agent deployment definition does not match its manifest and scope")
	}
	desiredDeployment.DefinitionID = definitionID
	desiredDeployment.ActiveVersion = strings.TrimSpace(request.Manifest.Metadata.Version)
	desiredDeployment.PreviousVersion = ""
	desiredDeployment.Revision = 1
	desiredDeployment.Health = DeploymentHealth{}
	desiredDeployment.Activation = nil
	EnsureDefaultWorkspace(desiredDeployment)
	desiredDeployment.CreatedAt = desiredDeployment.CreatedAt.UTC()
	desiredDeployment.UpdatedAt = desiredDeployment.UpdatedAt.UTC()
	if desiredDeployment.RolloutStatus == "" {
		desiredDeployment.RolloutStatus = RolloutActive
	}
	if err = desiredDeployment.Validate(); err != nil {
		return nil, err
	}

	definition, err := CompileManifest(request.Manifest, definitionID, DefinitionProvenance{
		Source: "portable-agent-manifest", Reference: request.Manifest.Metadata.ID + "@" + request.Manifest.Metadata.Version,
		CreatedBy: request.ActorType + ":" + request.ActorID,
	})
	if err != nil {
		return nil, err
	}
	if err = materializeSelfReferences(definition, desiredDeployment.ID); err != nil {
		return nil, err
	}
	if err = materializeEndpointReferences(definition, request.EndpointIDs); err != nil {
		return nil, err
	}
	if err = definition.Validate(); err != nil {
		return nil, err
	}
	if err = validateNarrowing(definition, desiredDeployment); err != nil {
		return nil, err
	}
	existingDeployment, deploymentErr := registry.GetAgentDeployment(ctx, desiredDeployment.Scope, desiredDeployment.ID)
	deploymentExists := deploymentErr == nil
	if deploymentErr != nil && !errors.Is(deploymentErr, ErrDeploymentNotFound) {
		return nil, deploymentErr
	}
	if deploymentExists && !sameInstalledManifestDeployment(existingDeployment, desiredDeployment) {
		return nil, ErrManifestInstallationConflict
	}

	versions, err := registry.ListAgentDefinitionVersions(ctx, definition.ID)
	if err != nil {
		return nil, err
	}
	registered := false
	for _, existing := range versions {
		if existing == nil || existing.Version != definition.Version {
			continue
		}
		if !sameInstalledManifestDefinition(existing, definition) {
			return nil, ErrManifestInstallationConflict
		}
		definition = existing
		registered = true
		break
	}
	if !registered {
		definition, err = registry.RegisterAgentDefinition(ctx, definition)
		if err != nil {
			return nil, err
		}
	}

	if !deploymentExists {
		created, _, createErr := registry.CreateAgentDeployment(ctx, desiredDeployment, request.ActorType, request.ActorID, strings.TrimSpace(request.Reason))
		if createErr != nil {
			return nil, createErr
		}
		return &ManifestInstallationResult{Definition: definition, Deployment: created}, nil
	}
	existing := existingDeployment
	if existing.ActiveVersion == definition.Version && existing.RolloutStatus == desiredDeployment.RolloutStatus {
		return &ManifestInstallationResult{Definition: definition, Deployment: existing, Replayed: true}, nil
	}
	if desiredDeployment.RolloutStatus != RolloutActive || existing.RolloutStatus == RolloutRetired {
		return nil, ErrManifestInstallationConflict
	}
	activated, _, err := registry.ActivateAgentDefinition(ctx, existing.Scope, existing.ID, definition.Version, existing.Revision,
		request.ActorType, request.ActorID, strings.TrimSpace(request.Reason))
	if err != nil {
		return nil, err
	}
	return &ManifestInstallationResult{Definition: definition, Deployment: activated}, nil
}

func materializeEndpointReferences(definition *AgentDefinition, endpointIDs map[string]string) error {
	if definition == nil || len(endpointIDs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for portableID, targetID := range endpointIDs {
		if !manifestIDPattern.MatchString(strings.TrimSpace(portableID)) || strings.TrimSpace(targetID) == "" || len(targetID) > 256 || seen[targetID] {
			return errors.New("Agent manifest installation endpoint mappings require unique portable and target ids")
		}
		seen[targetID] = true
	}
	for index := range definition.Channels {
		if targetID := endpointIDs[definition.Channels[index].EndpointID]; targetID != "" {
			definition.Channels[index].EndpointID = targetID
		}
	}
	for index := range definition.Authority.ApprovalDestinations {
		if targetID := endpointIDs[definition.Authority.ApprovalDestinations[index].EndpointID]; targetID != "" {
			definition.Authority.ApprovalDestinations[index].EndpointID = targetID
		}
	}
	return nil
}

func manifestDefinitionID(scope capability.ScopeReference, manifestID string) (string, error) {
	kind, scopeID, manifestID := strings.TrimSpace(scope.Kind), strings.TrimSpace(scope.ID), strings.TrimSpace(manifestID)
	if kind == "" || scopeID == "" || !manifestIDPattern.MatchString(manifestID) {
		return "", errors.New("agent manifest installation requires a valid scope and manifest id")
	}
	return kind + "/" + scopeID + "/" + manifestID, nil
}

func materializeSelfReferences(definition *AgentDefinition, deploymentID string) error {
	if definition == nil || definition.Runbook == nil {
		return nil
	}
	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return errors.New("agent deployment id is required to materialize Runbook self references")
	}
	for _, step := range definition.Runbook.Steps {
		if step.Delegate == nil || len(step.Delegate.AgentID.Literal) == 0 {
			continue
		}
		var target string
		if err := json.Unmarshal(step.Delegate.AgentID.Literal, &target); err != nil {
			return fmt.Errorf("decode Runbook delegate Agent identity: %w", err)
		}
		if target == "$self" {
			encoded, _ := json.Marshal(deploymentID)
			step.Delegate.AgentID.Literal = encoded
		}
	}
	for id, trigger := range definition.Runbook.Triggers {
		if strings.HasPrefix(trigger.ObjectiveID, "agent:$self:") {
			trigger.ObjectiveID = "agent:" + definition.ID + ":" + strings.TrimPrefix(trigger.ObjectiveID, "agent:$self:")
			definition.Runbook.Triggers[id] = trigger
		}
	}
	return nil
}

func sameInstalledManifestDefinition(left, right *AgentDefinition) bool {
	if left == nil || right == nil {
		return false
	}
	a, b := *left, *right
	a.Digest, b.Digest = "", ""
	a.CreatedAt = b.CreatedAt
	a.Provenance, b.Provenance = DefinitionProvenance{}, DefinitionProvenance{}
	leftJSON, _ := json.Marshal(a)
	rightJSON, _ := json.Marshal(b)
	return bytes.Equal(leftJSON, rightJSON)
}

func sameInstalledManifestDeployment(left, right *AgentDeployment) bool {
	if left == nil || right == nil {
		return false
	}
	a, b := *left, *right
	a.ActiveVersion, b.ActiveVersion = "", ""
	a.PreviousVersion, b.PreviousVersion = "", ""
	a.RolloutStatus, b.RolloutStatus = "", ""
	a.SkillBindingIDs, b.SkillBindingIDs = nil, nil
	a.Health, b.Health = DeploymentHealth{}, DeploymentHealth{}
	a.Activation, b.Activation = nil, nil
	a.Revision, b.Revision = 0, 0
	a.CreatedAt = b.CreatedAt
	a.UpdatedAt = b.UpdatedAt
	return reflect.DeepEqual(a, b)
}
