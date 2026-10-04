package runtime

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func setupUpgradeProof(from, to string, revision int64) skill.BindingLifecycleEntry {
	return skill.BindingLifecycleEntry{Revision: revision, Action: skill.BindingLifecycleUpdated,
		SkillUpgrade: &capability.BindingSkillUpgradeProvenance{
			From:                    capability.NewSkillIdentity("connector", from, "verified-publisher"),
			To:                      capability.NewSkillIdentity("connector", to, "verified-publisher"),
			ExpectedBindingRevision: revision - 1, PlanDigest: "sha256:" + strings.Repeat("a", 64),
		}}
}

func setupUpgradeFixture() (*SkillSetupRequest, *skill.Binding) {
	now := time.Now().UTC()
	r := &SkillSetupRequest{ID: "setup", Scope: Scope{Kind: "tenant", ID: "one"}, DeploymentID: "agent", ConversationID: "chat",
		TriggerMessageID: "message", RunID: "run", ActionCallID: "call", Kind: "reauthorize", SkillID: "connector", SkillVersion: "1.1.0",
		SourceIdentity: "verified-publisher", SkillName: "Connector", BindingID: "account", BindingRevision: 7,
		Phase: SkillSetupPhaseBindingUpgrade, BindingVersion: "1.0.0", RequiredActions: []string{"read"}, Reason: "Reconnect the account",
		Status: "pending", Revision: 3, CreatedAt: now, UpdatedAt: now}
	b := &skill.Binding{ID: r.BindingID, Scope: skill.ScopeReference{Kind: r.Scope.Kind, ID: r.Scope.ID}, DeploymentID: r.DeploymentID,
		SkillID: r.SkillID, SkillVersion: r.SkillVersion, SourceIdentity: r.SourceIdentity, Revision: 8,
		Lifecycle: []skill.BindingLifecycleEntry{setupUpgradeProof("1.0.0", "1.1.0", 8)}}
	return r, b
}

func TestSkillSetupUpgradeRebaseRequiresCanonicalProofAndFreshConfiguration(t *testing.T) {
	r, b := setupUpgradeFixture()
	before := cloneSkillSetupRequest(r)
	rebased, err := RebaseSkillSetupRequestAfterBindingUpgrade(r, 3, b)
	if err != nil {
		t.Fatal(err)
	}
	if rebased.Phase != SkillSetupPhaseConfiguration || rebased.BindingVersion != "1.1.0" || rebased.BindingRevision != 8 ||
		rebased.Revision != 4 || rebased.Status != "pending" || rebased.Kind != "reauthorize" || !reflect.DeepEqual(r, before) {
		t.Fatalf("unsafe setup transition: %#v", rebased)
	}
	rebased.RequiredActions[0] = "changed"
	if r.RequiredActions[0] != "read" {
		t.Fatal("rebase aliased the original actions")
	}
	if err := rebased.Validate(); err != nil {
		t.Fatal(err)
	}
	// The target binding revision is the new baseline, never configuration evidence.
	if rebased.BindingRevision != b.Revision {
		t.Fatal("upgrade revision counted as a saved configuration")
	}
}

func TestSkillSetupUpgradeRejectsUnprovedOrForeignTransitions(t *testing.T) {
	for _, name := range []string{"stale", "phase", "ordinary", "scope", "deployment", "account", "source", "version", "disabled", "digest", "cas", "publisher", "chain", "unchanged"} {
		t.Run(name, func(t *testing.T) {
			r, b := setupUpgradeFixture()
			expected := int64(3)
			switch name {
			case "stale":
				expected = 2
			case "phase":
				r.Phase = SkillSetupPhaseConfiguration
			case "ordinary":
				b.Lifecycle[0].SkillUpgrade = nil
			case "scope":
				b.Scope.ID = "other"
			case "deployment":
				b.DeploymentID = "other"
			case "account":
				b.ID = "other"
			case "source":
				b.SourceIdentity = "other"
			case "version":
				b.SkillVersion = "1.2.0"
			case "disabled":
				b.Disabled = true
			case "digest":
				b.Lifecycle[0].SkillUpgrade.PlanDigest = "not-a-digest"
			case "cas":
				b.Lifecycle[0].SkillUpgrade.ExpectedBindingRevision = 6
			case "publisher":
				b.Lifecycle[0].SkillUpgrade.To.SourceIdentity = "other"
			case "chain":
				b.Lifecycle[0].SkillUpgrade.From.Version = "0.9.0"
			case "unchanged":
				b.Revision = 7
			}
			if _, err := RebaseSkillSetupRequestAfterBindingUpgrade(r, expected, b); err == nil {
				t.Fatal("unproved upgrade accepted")
			}
		})
	}
}

func TestSkillSetupUpgradeRebaseAcceptsDurableChainAndOrdinaryRevisionChanges(t *testing.T) {
	r, b := setupUpgradeFixture()
	r.SkillVersion, b.SkillVersion, b.Revision = "1.2.0", "1.2.0", 11
	b.Lifecycle = append(b.Lifecycle, skill.BindingLifecycleEntry{Revision: 9, Action: skill.BindingLifecycleUpdated},
		setupUpgradeProof("1.1.0", "1.2.0", 10), skill.BindingLifecycleEntry{Revision: 11, Action: skill.BindingLifecycleUpdated})
	rebased, err := RebaseSkillSetupRequestAfterBindingUpgrade(r, 3, b)
	if err != nil || rebased.BindingRevision != 11 || rebased.BindingVersion != "1.2.0" {
		t.Fatalf("chain: %#v %v", rebased, err)
	}
}

func TestSkillSetupValidationDiagnosticsAreSafeAndMatchUnicodeSchema(t *testing.T) {
	r, _ := setupUpgradeFixture()
	r.Reason = strings.Repeat("海", 1000)
	if err := r.Validate(); err != nil {
		t.Fatalf("valid Unicode reason rejected: %v", err)
	}
	args := map[string]interface{}{"kind": "configure", "skillId": "connector", "skillVersion": "latest", "reason": r.Reason}
	if _, err := decodeSkillSetupArguments(args); err != nil {
		t.Fatal(err)
	}
	r.Reason += "海"
	args["reason"] = r.Reason
	for _, err := range []error{r.Validate(), func() error { _, err := decodeSkillSetupArguments(args); return err }()} {
		var diagnostic *SkillSetupValidationError
		if !errors.Is(err, ErrInvalidSkillSetup) || !errors.As(err, &diagnostic) || diagnostic.Field != "reason" || strings.Contains(err.Error(), "海") {
			t.Fatalf("unsafe or opaque diagnostic: %v", err)
		}
	}
	r, _ = setupUpgradeFixture()
	r.Phase = "unrecognized"
	var diagnostic *SkillSetupValidationError
	if err := r.Validate(); !errors.As(err, &diagnostic) || diagnostic.Field != "phase" {
		t.Fatalf("phase diagnostic: %v", err)
	}
}

func TestSkillSetupLegacyConfigurationPendingRequestIsReused(t *testing.T) {
	f := newLatestSetupTestFixture(t, latestSetupTestSource)
	f.bindAccount(t, "saved", "2.0.0", latestSetupTestSource, f.locations[0], true)
	request, _, err := f.request(t, "first", f.args("configure", "latest", "saved"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := cloneSkillSetupRequest(request)
	legacy.Phase, legacy.BindingVersion = "", ""
	legacy.Revision++
	if err := f.store.SaveSkillSetupRequest(f.ctx, legacy, request.Revision); err != nil {
		t.Fatal(err)
	}
	replayed, _, err := f.request(t, "second", f.args("configure", "latest", "saved"))
	if err != nil || replayed.ID != legacy.ID || replayed.Revision != legacy.Revision {
		t.Fatalf("legacy pending setup duplicated: %#v %v", replayed, err)
	}
	f.assertRequestCount(t, 1)
}
