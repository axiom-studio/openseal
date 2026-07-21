package skill

import (
	"context"
	"errors"
	"strings"
)

// BindingActivationPreview is a side-effect-free projection of whether one
// exact proposed binding can become model-visible on an execution host. A
// ready preview may still require the host to stage immutable resources or
// prepare a governed runtime during activation; previewing never invokes those
// adapters.
type BindingActivationPreview struct {
	Ready                      bool                 `json:"ready"`
	HostRevision               string               `json:"hostRevision,omitempty"`
	RequiresResourceStaging    bool                 `json:"requiresResourceStaging,omitempty"`
	RequiresRuntimePreparation bool                 `json:"requiresRuntimePreparation,omitempty"`
	Reasons                    []AvailabilityReason `json:"reasons,omitempty"`
}

// PreviewBindingActivation evaluates an exact source-qualified proposed
// binding against non-secret execution-host capabilities without persisting
// the binding, staging resources, or preparing a runtime.
func (c *Catalog) PreviewBindingActivation(ctx context.Context, binding *Binding, host HostCapabilityState) (*BindingActivationPreview, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	if err := validateBindingShape(binding); err != nil {
		return nil, err
	}
	definition, err := c.definitionFor(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
	if err != nil {
		return nil, err
	}
	if definition == nil {
		return &BindingActivationPreview{
			HostRevision: strings.TrimSpace(host.Revision),
			Reasons:      []AvailabilityReason{{Code: "definition_missing", Message: "proposed skill definition is unavailable"}},
		}, nil
	}
	candidate := cloneBinding(binding)
	candidate.SourceIdentity = DefinitionSourceIdentity(definition)
	return PreviewBindingActivation(definition, candidate, host)
}

// PreviewBindingActivation evaluates an already-resolved definition. It is
// useful at catalog and marketplace authorization boundaries where the host
// has verified an immutable candidate but has intentionally not installed or
// persisted a binding yet.
func PreviewBindingActivation(definition *Definition, binding *Binding, host HostCapabilityState) (*BindingActivationPreview, error) {
	if err := validateDefinition(definition); err != nil {
		return nil, err
	}
	if err := validateBindingShape(binding); err != nil {
		return nil, err
	}
	if binding.SkillID != definition.ID || binding.SkillVersion != definition.Version {
		return nil, errors.New("proposed binding does not identify the supplied skill definition")
	}
	exactSource := DefinitionSourceIdentity(definition)
	if strings.TrimSpace(binding.SourceIdentity) != exactSource {
		return nil, errors.New("proposed binding source does not identify the supplied skill definition")
	}
	candidate := cloneBinding(binding)
	candidate.SourceIdentity = exactSource
	if err := validateBindingAgainstDefinition(candidate, definition); err != nil {
		return nil, err
	}
	binding = candidate
	adapters, err := normalizeAdapterCapabilities(host.Adapters)
	if err != nil {
		return nil, err
	}
	host.Adapters = adapters
	preview := &BindingActivationPreview{HostRevision: strings.TrimSpace(host.Revision)}
	preview.Reasons = append(preview.Reasons, evaluateAvailability(definition, binding, host)...)

	resourceRoot := strings.TrimSpace(host.ResourceRoots[binding.ID])
	if resourceRoot == "" {
		resourceRoot = strings.TrimSpace(host.ResourceRoots[definition.ID+"@"+definition.Version])
	}
	resourcesRequired := bindingRequiresStagedResources(definition, binding)
	if resourceRoot == "" && len(preview.Reasons) == 0 && resourcesRequired {
		switch {
		case host.ResourceStager == nil:
			preview.Reasons = append(preview.Reasons, AvailabilityReason{Code: "resource_staging_unavailable", Requirement: AdapterResourceStaging, Message: "declared skill resources require a configured host staging adapter"})
		case !adapterAvailable(adapters, AdapterResourceStaging):
			preview.Reasons = append(preview.Reasons, AvailabilityReason{Code: "resource_staging_unavailable", Requirement: AdapterResourceStaging, Message: "the configured resource stager is not advertised as available by this host"})
		default:
			preview.RequiresResourceStaging = true
		}
	}
	if binding.EnablePrompt && definition.Prompt != nil && strings.Contains(definition.Prompt.Instructions, "{baseDir}") &&
		resourceRoot == "" && !preview.RequiresResourceStaging {
		preview.Reasons = append(preview.Reasons, AvailabilityReason{Code: "resource_root_missing", Requirement: "{baseDir}", Message: "skill instructions require a trusted absolute resource root"})
	} else if binding.EnablePrompt && definition.Prompt != nil && strings.Contains(definition.Prompt.Instructions, "{baseDir}") &&
		resourceRoot != "" && !validResourceRoot(host.OperatingSystem, resourceRoot) {
		preview.Reasons = append(preview.Reasons, AvailabilityReason{Code: "resource_root_missing", Requirement: "{baseDir}", Message: "skill instructions require a trusted absolute resource root"})
	}

	if len(preview.Reasons) == 0 && adapterAvailable(adapters, AdapterPreparedRuntime) {
		if host.RuntimePreparer == nil {
			preview.Reasons = append(preview.Reasons, AvailabilityReason{Code: "runtime_preparation_unavailable", Requirement: AdapterPreparedRuntime, Message: "the host advertises prepared runtimes without a configured preparation adapter"})
		} else {
			request, requestErr := runtimePreparationRequest(binding.Scope, binding.DeploymentID, binding, definition, host)
			if requestErr != nil {
				preview.Reasons = append(preview.Reasons, AvailabilityReason{Code: "runtime_preparation_invalid", Requirement: AdapterPreparedRuntime, Message: "the Skill runtime requirements cannot be prepared safely"})
			} else if request != nil {
				preview.RequiresRuntimePreparation = true
			}
		}
	}
	preview.Ready = len(preview.Reasons) == 0
	return preview, nil
}
