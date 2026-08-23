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
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			if err := value.Validate(); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}
