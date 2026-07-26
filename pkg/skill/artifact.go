package skill

import "github.com/axiom-studio/openseal/pkg/capability"

type PromptModule = capability.PromptModule
type Requirements = capability.Requirements
type StorageRequirement = capability.StorageRequirement
type StorageDurability = capability.StorageDurability
type StorageRetention = capability.StorageRetention
type ComputeRequirements = capability.ComputeRequirements
type ComputeResources = capability.ComputeResources
type Installer = capability.Installer
type Resource = capability.Resource
type SourceProvenance = capability.SourceProvenance

const (
	StorageDurabilityEphemeral  = capability.StorageDurabilityEphemeral
	StorageDurabilityPersistent = capability.StorageDurabilityPersistent
	StorageRetentionDelete      = capability.StorageRetentionDelete
	StorageRetentionRetain      = capability.StorageRetentionRetain
)
