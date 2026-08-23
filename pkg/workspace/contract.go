// Package workspace defines the portable, domain-neutral storage and compute
// contract used by Agents. Hosts own provisioning, isolation, scheduling, and
// credential projection; OpenSeal only carries governed desired state.
package workspace

import (
	"errors"
	"regexp"
	"strings"
)

const (
	DefaultID              = "default"
	DefaultStorageCapacity = "10Gi"
	DefaultCPU             = "2"
	DefaultMemory          = "4Gi"
)

type StorageDurability string
type StorageRetention string

const (
	StorageDurabilityPersistent StorageDurability = "persistent"
	StorageDurabilityEphemeral  StorageDurability = "ephemeral"

	StorageRetentionRetain StorageRetention = "retain"
	StorageRetentionDelete StorageRetention = "delete"
)

type StorageProfile struct {
	Capacity   string            `json:"capacity"`
	Durability StorageDurability `json:"durability"`
	Retention  StorageRetention  `json:"retention"`
}

type AcceleratorProfile struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

type ComputeProfile struct {
	CPU         string              `json:"cpu"`
	Memory      string              `json:"memory"`
	Accelerator *AcceleratorProfile `json:"accelerator,omitempty"`
}

// Spec is deliberately domain-neutral. Repositories, media tools, notebooks,
// and deployment clients are capabilities attached to a Workspace rather than
// properties of the Workspace itself.
type Spec struct {
	ID             string         `json:"id"`
	DisplayName    string         `json:"displayName,omitempty"`
	Storage        StorageProfile `json:"storage"`
	Compute        ComputeProfile `json:"compute"`
	MaxConcurrency int            `json:"maxConcurrency"`
}

var (
	idPattern       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	quantityPattern = regexp.MustCompile(`^[1-9][0-9]*(?:m|Ki|Mi|Gi|Ti|Pi|Ei)?$`)
)

func DefaultSpec() Spec {
	return Spec{
		ID: DefaultID, DisplayName: "Default",
		Storage: StorageProfile{Capacity: DefaultStorageCapacity, Durability: StorageDurabilityPersistent, Retention: StorageRetentionRetain},
		Compute: ComputeProfile{CPU: DefaultCPU, Memory: DefaultMemory}, MaxConcurrency: 1,
	}
}

func (s Spec) Validate() error {
	if !idPattern.MatchString(strings.TrimSpace(s.ID)) {
		return errors.New("workspace id is invalid")
	}
	if len(strings.TrimSpace(s.DisplayName)) > 200 {
		return errors.New("workspace display name must not exceed 200 characters")
	}
	if !quantityPattern.MatchString(strings.TrimSpace(s.Storage.Capacity)) {
		return errors.New("workspace storage capacity is invalid")
	}
	if s.Storage.Durability != StorageDurabilityPersistent && s.Storage.Durability != StorageDurabilityEphemeral {
		return errors.New("workspace storage durability is invalid")
	}
	if s.Storage.Durability == StorageDurabilityPersistent {
		if s.Storage.Retention != StorageRetentionRetain && s.Storage.Retention != StorageRetentionDelete {
			return errors.New("persistent workspace requires an explicit retention policy")
		}
	} else if s.Storage.Retention != "" && s.Storage.Retention != StorageRetentionDelete {
		return errors.New("ephemeral workspace cannot be retained")
	}
	if !quantityPattern.MatchString(strings.TrimSpace(s.Compute.CPU)) || !quantityPattern.MatchString(strings.TrimSpace(s.Compute.Memory)) {
		return errors.New("workspace compute quantities are invalid")
	}
	if s.MaxConcurrency < 1 || s.MaxConcurrency > 256 {
		return errors.New("workspace concurrency must be between 1 and 256")
	}
	if s.Compute.Accelerator != nil && (strings.TrimSpace(s.Compute.Accelerator.Type) == "" || s.Compute.Accelerator.Count < 1 || s.Compute.Accelerator.Count > 64) {
		return errors.New("workspace accelerator profile is invalid")
	}
	return nil
}
