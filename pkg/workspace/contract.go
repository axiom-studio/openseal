// Package workspace defines the portable, domain-neutral storage and compute
// contract used by Agents. Hosts own provisioning, isolation, scheduling, and
// credential projection; OpenSeal only carries governed desired state.
package workspace

import (
	"errors"
	"regexp"
	"slices"
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

type NetworkAccess string

const (
	NetworkDenied NetworkAccess = "denied"
	NetworkEgress NetworkAccess = "egress"
)

// CommandPolicy bounds native process execution inside a Workspace. Enabled
// command authority always names an explicit executable allow-list.
type CommandPolicy struct {
	Enabled            bool          `json:"enabled"`
	Network            NetworkAccess `json:"network"`
	MaxDurationSeconds int           `json:"maxDurationSeconds"`
	AllowedExecutables []string      `json:"allowedExecutables,omitempty"`
}

// GitPolicy grants fixed repository operations. Credentials are projected only
// into those operations and never into arbitrary Workspace commands.
type GitPolicy struct {
	Enabled             bool     `json:"enabled"`
	PushEnabled         bool     `json:"pushEnabled"`
	CredentialBinding   string   `json:"credentialBinding,omitempty"`
	CredentialKind      string   `json:"credentialKind,omitempty"`
	AllowedHosts        []string `json:"allowedHosts,omitempty"`
	AllowedRepositories []string `json:"allowedRepositories,omitempty"`
	MaxDurationSeconds  int      `json:"maxDurationSeconds"`
}

// Policy is framework authority, not a Skill binding. The kernel projects it
// into every hosted turn and the execution host must enforce it again.
type Policy struct {
	Filesystem         Access        `json:"filesystem"`
	Commands           CommandPolicy `json:"commands"`
	Git                GitPolicy     `json:"git"`
	CredentialBindings []string      `json:"credentialBindings,omitempty"`
}

// Spec is deliberately domain-neutral. Repositories, media tools, notebooks,
// and deployment clients are capabilities attached to a Workspace rather than
// properties of the Workspace itself.
type Spec struct {
	ID             string         `json:"id"`
	DisplayName    string         `json:"displayName,omitempty"`
	Storage        StorageProfile `json:"storage"`
	Compute        ComputeProfile `json:"compute"`
	Policy         Policy         `json:"policy"`
	MaxConcurrency int            `json:"maxConcurrency"`
}

var (
	idPattern             = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	quantityPattern       = regexp.MustCompile(`^[1-9][0-9]*(?:m|Ki|Mi|Gi|Ti|Pi|Ei)?$`)
	bindingPattern        = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
	credentialKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	hostPattern           = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	repositoryPattern     = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?/[a-z0-9._-]+(?:/[a-z0-9._-]+)*$`)
)

func DefaultSpec() Spec {
	return Spec{
		ID: DefaultID, DisplayName: "Default",
		Storage: StorageProfile{Capacity: DefaultStorageCapacity, Durability: StorageDurabilityPersistent, Retention: StorageRetentionRetain},
		Compute: ComputeProfile{CPU: DefaultCPU, Memory: DefaultMemory},
		Policy:  Policy{Filesystem: AccessReadWrite, Commands: CommandPolicy{Network: NetworkDenied}}, MaxConcurrency: 1,
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
	if s.Policy.Filesystem != AccessReadOnly && s.Policy.Filesystem != AccessReadWrite {
		return errors.New("workspace filesystem access is invalid")
	}
	if s.Policy.Commands.Network != NetworkDenied && s.Policy.Commands.Network != NetworkEgress {
		return errors.New("workspace command network access is invalid")
	}
	if !s.Policy.Commands.Enabled {
		if s.Policy.Commands.MaxDurationSeconds != 0 || len(s.Policy.Commands.AllowedExecutables) != 0 || s.Policy.Commands.Network != NetworkDenied {
			return errors.New("disabled workspace commands cannot grant execution authority")
		}
	} else {
		if s.Policy.Commands.MaxDurationSeconds < 1 || s.Policy.Commands.MaxDurationSeconds > 900 {
			return errors.New("workspace command duration must be between 1 and 900 seconds")
		}
		if len(s.Policy.Commands.AllowedExecutables) == 0 {
			return errors.New("enabled workspace commands require an executable allow-list")
		}
	}
	for _, executable := range s.Policy.Commands.AllowedExecutables {
		if strings.TrimSpace(executable) == "" || strings.ContainsAny(executable, " \t\r\n") {
			return errors.New("workspace allowed executable is invalid")
		}
	}
	if len(s.Policy.Commands.AllowedExecutables) != len(slices.Compact(append([]string(nil), s.Policy.Commands.AllowedExecutables...))) {
		return errors.New("workspace allowed executables must be unique")
	}
	if len(s.Policy.CredentialBindings) > 16 {
		return errors.New("workspace credential bindings must not exceed 16")
	}
	for _, binding := range s.Policy.CredentialBindings {
		if !bindingPattern.MatchString(binding) {
			return errors.New("workspace credential binding is invalid")
		}
	}
	if !s.Policy.Git.Enabled {
		if s.Policy.Git.PushEnabled || s.Policy.Git.CredentialBinding != "" || s.Policy.Git.CredentialKind != "" || len(s.Policy.Git.AllowedHosts) != 0 || len(s.Policy.Git.AllowedRepositories) != 0 || s.Policy.Git.MaxDurationSeconds != 0 {
			return errors.New("disabled workspace Git cannot grant repository authority")
		}
	} else {
		if s.Policy.Filesystem != AccessReadWrite || !bindingPattern.MatchString(s.Policy.Git.CredentialBinding) || !credentialKindPattern.MatchString(s.Policy.Git.CredentialKind) ||
			!slices.Contains(s.Policy.CredentialBindings, s.Policy.Git.CredentialBinding) || len(s.Policy.Git.AllowedHosts) == 0 || len(s.Policy.Git.AllowedHosts) > 16 ||
			len(s.Policy.Git.AllowedRepositories) == 0 || len(s.Policy.Git.AllowedRepositories) > 64 ||
			s.Policy.Git.MaxDurationSeconds < 1 || s.Policy.Git.MaxDurationSeconds > 900 {
			return errors.New("workspace Git authority is invalid")
		}
		for _, host := range s.Policy.Git.AllowedHosts {
			if !hostPattern.MatchString(host) {
				return errors.New("workspace Git host is invalid")
			}
		}
		if len(s.Policy.Git.AllowedHosts) != len(slices.Compact(append([]string(nil), s.Policy.Git.AllowedHosts...))) {
			return errors.New("workspace Git hosts must be unique")
		}
		for _, repository := range s.Policy.Git.AllowedRepositories {
			if !repositoryPattern.MatchString(repository) {
				return errors.New("workspace Git repository is invalid")
			}
			host, _, _ := strings.Cut(repository, "/")
			if !slices.Contains(s.Policy.Git.AllowedHosts, host) {
				return errors.New("workspace Git repository host is not allowed")
			}
		}
		if len(s.Policy.Git.AllowedRepositories) != len(slices.Compact(append([]string(nil), s.Policy.Git.AllowedRepositories...))) {
			return errors.New("workspace Git repositories must be unique")
		}
	}
	if len(s.Policy.CredentialBindings) != len(slices.Compact(append([]string(nil), s.Policy.CredentialBindings...))) {
		return errors.New("workspace credential bindings must be unique")
	}
	return nil
}
