package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/source"
)

func TestLoadDaemonConfigDefaultsToDurableSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	cfg, err := LoadDaemonConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Driver != "sqlite" || cfg.Storage.Path != "data/openseal.db" || cfg.Storage.ArtifactsPath != "data/artifacts" {
		t.Fatalf("storage = %#v", cfg.Storage)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("default config was not written")
	}
}

func TestSourcePolicyCatalogIsScopedAndFeedsOnlyEnabledOutreachWorkers(t *testing.T) {
	local := runtime.Scope{Kind: "local", ID: "default"}
	other := runtime.Scope{Kind: "local", ID: "other"}
	policy := source.Policy{ID: "community", Version: "1", Enabled: true, MaximumItems: 10,
		Sources:  []source.PolicySource{{Host: "community.example.com", PathPrefixes: []string{"/threads"}}},
		Outreach: &source.OutreachPolicy{Enabled: true, ApprovalPolicy: "operator-review", MaximumBytes: 2000}}
	catalog, err := NewSourcePolicyCatalog([]ScopedSourcePolicy{{Scope: local, Policy: policy}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.ResolveOutreachPolicy(t.Context(), skill.ScopeReference{Kind: local.Kind, ID: local.ID}, "community@1")
	if err != nil || resolved.ID != policy.ID {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	if _, err := catalog.ResolveOutreachPolicy(t.Context(), skill.ScopeReference{Kind: other.Kind, ID: other.ID}, "community@1"); err == nil {
		t.Fatal("policy crossed its configured scope")
	}
	scopes, err := catalog.ListWorkerScopes(context.Background())
	if err != nil || len(scopes) != 1 || scopes[0] != local || !catalog.OutreachEnabled(local) || catalog.OutreachEnabled(other) {
		t.Fatalf("scopes=%#v err=%v", scopes, err)
	}
	resolved.Sources[0].PathPrefixes[0] = "/mutated"
	again, _ := catalog.ResolveOutreachPolicy(t.Context(), skill.ScopeReference{Kind: local.Kind, ID: local.ID}, "community@1")
	if again.Sources[0].PathPrefixes[0] != "/threads" {
		t.Fatal("resolved policy mutated the catalog")
	}
}

func TestDaemonConfigRejectsDuplicateOrInvalidSourcePolicies(t *testing.T) {
	cfg := DefaultDaemonConfig()
	entry := ScopedSourcePolicy{Scope: runtime.Scope{Kind: "local", ID: "default"}, Policy: source.Policy{ID: "community", Version: "1", Enabled: true, MaximumItems: 10,
		Sources: []source.PolicySource{{Host: "community.example.com"}}, Outreach: &source.OutreachPolicy{Enabled: true, ApprovalPolicy: "review", MaximumBytes: 1000}}}
	cfg.SourcePolicies = []ScopedSourcePolicy{entry, entry}
	if err := cfg.Validate(); err == nil {
		t.Fatal("duplicate source policy was accepted")
	}
	cfg.SourcePolicies = []ScopedSourcePolicy{{Scope: runtime.Scope{Kind: "", ID: "default"}, Policy: entry.Policy}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid source policy scope was accepted")
	}
}

func TestDaemonConfigRejectsEphemeralOrUnknownStorage(t *testing.T) {
	cfg := DefaultDaemonConfig()
	cfg.Storage.Driver = "memory"
	if err := cfg.Validate(); err == nil {
		t.Fatal("memory storage should not be accepted by the durable daemon")
	}
}
