package workspace

import "testing"

func TestDefaultSpecIsPortableAndValid(t *testing.T) {
	value := DefaultSpec()
	if err := value.Validate(); err != nil {
		t.Fatalf("default workspace: %v", err)
	}
	if value.ID != DefaultID || value.Storage.Durability != StorageDurabilityPersistent || value.Storage.Retention != StorageRetentionRetain || value.MaxConcurrency != 1 {
		t.Fatalf("default workspace = %#v", value)
	}
}

func TestSpecRejectsUnsafeOrUnboundedProfiles(t *testing.T) {
	tests := map[string]Spec{
		"unsafe id":          func() Spec { v := DefaultSpec(); v.ID = "../escape"; return v }(),
		"missing retention":  func() Spec { v := DefaultSpec(); v.Storage.Retention = ""; return v }(),
		"retained ephemeral": func() Spec { v := DefaultSpec(); v.Storage.Durability = StorageDurabilityEphemeral; return v }(),
		"zero concurrency":   func() Spec { v := DefaultSpec(); v.MaxConcurrency = 0; return v }(),
		"invalid gpu": func() Spec {
			v := DefaultSpec()
			v.Compute.Accelerator = &AcceleratorProfile{Type: "nvidia.com/gpu"}
			return v
		}(),
		"invalid credential binding": func() Spec {
			v := DefaultSpec()
			v.Policy.CredentialBindings = []string{"github token"}
			return v
		}(),
		"Git without binding": func() Spec {
			v := DefaultSpec()
			v.Policy.Git = GitPolicy{Enabled: true, CredentialBinding: "GITHUB", CredentialKind: "github_token", AllowedHosts: []string{"github.com"}, AllowedRepositories: []string{"github.com/axiom-studio/cortex"}, MaxDurationSeconds: 300}
			return v
		}(),
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			if err := value.Validate(); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}

func TestSpecRejectsGitRepositoryOutsideHostAuthority(t *testing.T) {
	value := DefaultSpec()
	value.Policy.CredentialBindings = []string{"GITHUB"}
	value.Policy.Git = GitPolicy{Enabled: true, CredentialBinding: "GITHUB", CredentialKind: "github_token", AllowedHosts: []string{"github.com"}, AllowedRepositories: []string{"gitlab.com/axiom-studio/cortex"}, MaxDurationSeconds: 300}
	if err := value.Validate(); err == nil {
		t.Fatal("expected repository host outside Git authority to be rejected")
	}
}
