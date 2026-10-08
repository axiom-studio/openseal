package runtime

import (
	"context"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const latestSetupTestSource = "clawhub::@alice/reddit.reader"

type latestSetupTestLocation struct {
	scope        skill.ScopeReference
	deploymentID string
}

type latestSetupTestFixture struct {
	ctx        context.Context
	scope      Scope
	store      *MemoryStore
	catalog    *skill.Catalog
	run        *AgentRun
	dispatcher *SkillBindingActionDispatcher
	source     string
	candidates []skill.DiscoveryCandidate
	locations  []latestSetupTestLocation
}

func newLatestSetupTestFixture(t *testing.T, source string) *latestSetupTestFixture {
	t.Helper()
	f := &latestSetupTestFixture{ctx: context.Background(), scope: Scope{Kind: "tenant", ID: "a"}, store: NewMemoryStore(), source: source}
	f.catalog = skillActionCatalog(t, f.ctx, f.scope, "agent")
	f.locations = []latestSetupTestLocation{{scope: skill.ScopeReference{Kind: "tenant", ID: "a"}, deploymentID: "agent"}}
	if source != "" {
		f.registerDefinition(t, "reddit.reader", "1.0.0", source)
	}
	f.registerDefinition(t, "reddit.reader", "2.0.0", source)
	f.candidates = []skill.DiscoveryCandidate{latestSetupTestCandidate("2.0.0", source), latestSetupTestCandidate("1.0.0", source)}
	provider := skill.DiscoveryProviderFunc(func(_ context.Context, r skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		if r.Scope.Kind != f.scope.Kind || r.Scope.ID != f.scope.ID || r.DeploymentID != "agent" {
			t.Fatal("setup discovery did not derive authority from the conversation")
		}
		return &skill.DiscoveryPage{Items: f.candidates}, nil
	})
	var err error
	f.dispatcher, err = NewSkillBindingActionDispatcher(f.store, f.catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	f.run = createClaimedSkillActionRun(t, f.ctx, f.store, f.scope, "agent", "worker")
	f.run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	return f
}

func latestSetupTestCandidate(version, source string) skill.DiscoveryCandidate {
	actions := []skill.DiscoveryAction{{Name: "read", Risk: skill.RiskLevelRead}}
	if version != "1.0.0" {
		actions = append(actions, skill.DiscoveryAction{Name: "read_latest", Risk: skill.RiskLevelRead})
	}
	return skill.DiscoveryCandidate{ID: "reddit.reader", Version: version, SourceIdentity: source, Name: "Reddit Reader", Readiness: skill.DiscoveryReadinessBindable, PromptAvailable: true, Actions: actions}
}

func (f *latestSetupTestFixture) registerDefinition(t *testing.T, id, version, source string) {
	t.Helper()
	definition := redditSkillDefinition()
	definition.ID, definition.Version = id, version
	if source != "" {
		definition.Source = &skill.SourceProvenance{Identity: source, Format: "openclaw.skill.v1", Publisher: "fixture"}
	}
	if version != "1.0.0" {
		action := definition.Actions["read"]
		action.Name = "read_latest"
		definition.Actions[action.Name] = action
	}
	if err := f.catalog.Register(f.ctx, definition); err != nil {
		t.Fatal(err)
	}
}

func (f *latestSetupTestFixture) bindAccount(t *testing.T, id, version, source string, location latestSetupTestLocation, accountOnly bool) *skill.Binding {
	t.Helper()
	binding := &skill.Binding{ID: id, Revision: 1, Scope: location.scope, DeploymentID: location.deploymentID, SkillID: "reddit.reader", SkillVersion: version, SourceIdentity: source, MaximumRisk: skill.RiskLevelRead, Credentials: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "credential://private-" + id}}}
	if !accountOnly {
		binding.AllowedActions = []string{"read"}
		binding.EnablePrompt = true
	}
	if err := f.catalog.Bind(f.ctx, binding); err != nil {
		t.Fatal(err)
	}
	f.locations = append(f.locations, location)
	saved, err := f.catalog.GetBinding(f.ctx, location.scope, location.deploymentID, id)
	if err != nil || saved == nil {
		t.Fatalf("load account: %#v %v", saved, err)
	}
	return saved
}

func (f *latestSetupTestFixture) args(kind, version, bindingID string) map[string]interface{} {
	args := map[string]interface{}{"kind": kind, "skillId": "reddit.reader", "skillVersion": version, "reason": "Use the saved account", "requiredActions": []interface{}{"read_latest"}, "sourceIdentity": f.source}
	if bindingID != "" {
		args["bindingId"] = bindingID
	}
	return args
}

func (f *latestSetupTestFixture) snapshotBindings(t *testing.T) [][]*skill.Binding {
	t.Helper()
	var result [][]*skill.Binding
	for _, location := range f.locations {
		bindings, err := f.catalog.ListBindings(f.ctx, location.scope, location.deploymentID)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, bindings)
	}
	return result
}

// A setup request is an interaction. Verify both successful and rejected requests
// leave local and foreign account credentials, revisions and permissions intact.
func (f *latestSetupTestFixture) request(t *testing.T, callID string, args map[string]interface{}) (*SkillSetupRequest, map[string]interface{}, error) {
	t.Helper()
	before := f.snapshotBindings(t)
	result, err := f.dispatcher.requestSkillSetup(f.ctx, ActionDispatchInput{Call: &ActionCall{ID: callID, Scope: f.scope, RunID: f.run.ID}, Arguments: args}, f.run, "agent")
	if after := f.snapshotBindings(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("setup changed account authority:\nbefore %#v\nafter %#v", before, after)
	}
	if err != nil {
		return nil, nil, err
	}
	value, ok := result["setupRequest"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing setup request: %#v", result)
	}
	id, ok := value["id"].(string)
	if !ok {
		t.Fatalf("missing setup ID: %#v", value)
	}
	record, err := f.store.GetSkillSetupRequest(f.ctx, f.scope, id)
	if err != nil || record == nil {
		t.Fatalf("setup was not durable: %#v %v", record, err)
	}
	return record, value, nil
}

func (f *latestSetupTestFixture) assertRequestCount(t *testing.T, want int) {
	t.Helper()
	requests, err := f.store.ListSkillSetupRequests(f.ctx, f.scope, "agent", "chat")
	if err != nil || len(requests) != want {
		t.Fatalf("pending request count = %d, want %d: %v", len(requests), want, err)
	}
}

func TestSkillSetupLatestAccountRetainsExactSavedIdentity(t *testing.T) {
	for _, source := range []string{"", latestSetupTestSource} {
		for _, kind := range []string{"configure", "reauthorize"} {
			for _, explicit := range []bool{false, true} {
				name := source + "/" + kind + "/implicit"
				if explicit {
					name = source + "/" + kind + "/explicit"
				}
				t.Run(name, func(t *testing.T) {
					f := newLatestSetupTestFixture(t, source)
					account := f.bindAccount(t, "saved", "1.0.0", source, f.locations[0], true)
					// An existing noninitial revision must be retained exactly as well.
					account.Revision++
					if err := f.catalog.Bind(f.ctx, account); err != nil {
						t.Fatal(err)
					}
					bindingID := ""
					if explicit {
						bindingID = account.ID
					}
					record, value, err := f.request(t, "request", f.args(kind, "latest", bindingID))
					if err != nil {
						t.Fatal(err)
					}
					if record.SkillVersion != "2.0.0" || record.SourceIdentity != source || record.BindingID != account.ID || record.BindingRevision != account.Revision || record.BindingVersion != "1.0.0" || record.Phase != SkillSetupPhaseBindingUpgrade || record.Status != "pending" {
						t.Fatalf("latest lost retained account or upgrade phase: %#v", record)
					}
					if value["phase"] != "binding_upgrade" || value["bindingVersion"] != "1.0.0" || value["skillVersion"] != "2.0.0" || !reflect.DeepEqual(record.RequiredActions, []string{"read_latest"}) {
						t.Fatalf("setup did not expose the exact requested target: %#v", value)
					}
					f.assertRequestCount(t, 1)
				})
			}
		}
	}
}

func TestSkillSetupLatestAccountConfigurationPhase(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new_account"
		if existing {
			name = "same_version_account"
		}
		t.Run(name, func(t *testing.T) {
			f := newLatestSetupTestFixture(t, "")
			bindingID, bindingVersion := "", ""
			var revision int64
			if existing {
				account := f.bindAccount(t, "saved", "2.0.0", "", f.locations[0], false)
				bindingID, bindingVersion, revision = account.ID, account.SkillVersion, account.Revision
			}
			record, _, err := f.request(t, "request", f.args("configure", "latest", ""))
			if err != nil {
				t.Fatal(err)
			}
			if record.Phase != SkillSetupPhaseConfiguration || record.SkillVersion != "2.0.0" || record.BindingID != bindingID || record.BindingRevision != revision || record.BindingVersion != bindingVersion {
				t.Fatalf("configuration identity: %#v", record)
			}
		})
	}
}

func TestSkillSetupLatestAccountPendingUpgradeIsReused(t *testing.T) {
	f := newLatestSetupTestFixture(t, latestSetupTestSource)
	account := f.bindAccount(t, "saved", "1.0.0", f.source, f.locations[0], false)
	first, _, err := f.request(t, "first", f.args("configure", "latest", ""))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := f.request(t, "second", f.args("configure", "latest", account.ID))
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("equivalent explicit/implicit upgrade was not reused: %#v %v", second, err)
	}
	f.assertRequestCount(t, 1)
}

func TestSkillSetupLatestAccountAmbiguityIncludesRetainedVersions(t *testing.T) {
	f := newLatestSetupTestFixture(t, "")
	old := f.bindAccount(t, "retained", "1.0.0", "", f.locations[0], true)
	f.bindAccount(t, "current", "2.0.0", "", f.locations[0], true)
	if _, _, err := f.request(t, "ambiguous", f.args("configure", "latest", "")); err == nil {
		t.Fatal("latest version silently chose one of multiple saved accounts")
	}
	f.assertRequestCount(t, 0)
	record, _, err := f.request(t, "explicit", f.args("configure", "latest", old.ID))
	if err != nil || record == nil || record.BindingID != old.ID || record.BindingVersion != "1.0.0" || record.Phase != SkillSetupPhaseBindingUpgrade {
		t.Fatalf("explicit retained account could not resolve ambiguity: %#v %v", record, err)
	}
}

func TestSkillSetupLatestAccountPublisherCollisionStaysExact(t *testing.T) {
	f := newLatestSetupTestFixture(t, latestSetupTestSource)
	alice := f.bindAccount(t, "alice-account", "1.0.0", f.source, f.locations[0], true)
	otherSource := "clawhub::@bob/reddit.reader"
	f.registerDefinition(t, "reddit.reader", "1.0.0", otherSource)
	f.registerDefinition(t, "reddit.reader", "9.0.0", otherSource)
	bob := f.bindAccount(t, "bob-account", "1.0.0", otherSource, f.locations[0], true)
	f.candidates = append(f.candidates, latestSetupTestCandidate("9.0.0", otherSource))
	if _, _, err := f.request(t, "foreign", f.args("configure", "latest", bob.ID)); err == nil {
		t.Fatal("an explicit account from another publisher was accepted")
	}
	f.assertRequestCount(t, 0)
	record, _, err := f.request(t, "correct", f.args("reauthorize", "latest", ""))
	if err != nil || record == nil || record.BindingID != alice.ID || record.SourceIdentity != f.source || record.SkillVersion != "2.0.0" {
		t.Fatalf("publisher collision selected a foreign account or version: %#v %v", record, err)
	}
}

func TestSkillSetupLatestAccountExplicitForeignSkillIsRejected(t *testing.T) {
	f := newLatestSetupTestFixture(t, latestSetupTestSource)
	f.registerDefinition(t, "reddit.other", "1.0.0", f.source)
	binding := &skill.Binding{ID: "other-skill", Revision: 1, Scope: f.locations[0].scope, DeploymentID: "agent", SkillID: "reddit.other", SkillVersion: "1.0.0", SourceIdentity: f.source, MaximumRisk: skill.RiskLevelRead, Credentials: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "credential://other"}}}
	if err := f.catalog.Bind(f.ctx, binding); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.request(t, "foreign", f.args("configure", "latest", binding.ID)); err == nil {
		t.Fatal("explicit account for a different Skill ID was accepted")
	}
	f.assertRequestCount(t, 0)
}

func TestSkillSetupLatestAccountForeignScopeIsNeverRetained(t *testing.T) {
	for _, location := range []latestSetupTestLocation{{scope: skill.ScopeReference{Kind: "tenant", ID: "b"}, deploymentID: "agent"}, {scope: skill.ScopeReference{Kind: "tenant", ID: "a"}, deploymentID: "other-agent"}} {
		t.Run(location.scope.ID+"/"+location.deploymentID, func(t *testing.T) {
			f := newLatestSetupTestFixture(t, latestSetupTestSource)
			account := f.bindAccount(t, "saved", "1.0.0", f.source, location, true)
			if _, _, err := f.request(t, "explicit", f.args("configure", "latest", account.ID)); err == nil {
				t.Fatal("foreign-scope explicit account was accepted")
			}
			if _, _, err := f.request(t, "reauthorize", f.args("reauthorize", "latest", "")); err == nil {
				t.Fatal("foreign-scope account satisfied implicit reauthorization")
			}
			f.assertRequestCount(t, 0)
			record, _, err := f.request(t, "configure", f.args("configure", "latest", ""))
			if err != nil || record == nil || record.BindingID != "" || record.BindingRevision != 0 || record.BindingVersion != "" || record.Phase != SkillSetupPhaseConfiguration {
				t.Fatalf("new local configuration retained foreign account: %#v %v", record, err)
			}
		})
	}
}

func TestSkillSetupLatestAccountDisabledBehavior(t *testing.T) {
	f := newLatestSetupTestFixture(t, "")
	account := f.bindAccount(t, "saved", "1.0.0", "", f.locations[0], false)
	if _, err := f.catalog.DisableBinding(f.ctx, skill.DisableBindingRequest{Scope: account.Scope, DeploymentID: account.DeploymentID, BindingID: account.ID, ExpectedRevision: account.Revision, Actor: skill.BindingActor{Type: "user", ID: "owner"}, Reason: "Disconnect account"}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"configure", "reauthorize"} {
		if _, _, err := f.request(t, "explicit-"+kind, f.args(kind, "latest", account.ID)); err == nil {
			t.Fatalf("disabled retained account was upgraded by %s", kind)
		}
	}
	if _, _, err := f.request(t, "implicit-reauthorize", f.args("reauthorize", "latest", "")); err == nil {
		t.Fatal("disabled account satisfied implicit reauthorization")
	}
	f.assertRequestCount(t, 0)
	newRecord, _, err := f.request(t, "implicit-configure", f.args("configure", "latest", ""))
	if err != nil || newRecord == nil || newRecord.BindingID != "" || newRecord.Phase != SkillSetupPhaseConfiguration {
		t.Fatalf("disabled account was not excluded from implicit configuration: %#v %v", newRecord, err)
	}
	args := f.args("configure", "1.0.0", account.ID)
	args["requiredActions"] = []interface{}{"read"}
	sameVersion, _, err := f.request(t, "same-version", args)
	if err != nil || sameVersion == nil || sameVersion.BindingID != account.ID || sameVersion.BindingRevision != account.Revision+1 || sameVersion.BindingVersion != "1.0.0" || sameVersion.Phase != SkillSetupPhaseConfiguration {
		t.Fatalf("same-version disabled configuration changed existing semantics: %#v %v", sameVersion, err)
	}
}

func TestSkillSetupLatestAccountRequiredActionsUseExactSelectedVersion(t *testing.T) {
	f := newLatestSetupTestFixture(t, "")
	f.bindAccount(t, "saved", "1.0.0", "", f.locations[0], true)
	unknown := f.args("configure", "latest", "")
	unknown["requiredActions"] = []interface{}{"unknown"}
	if _, _, err := f.request(t, "unknown", unknown); err == nil {
		t.Fatal("unknown action was accepted from latest")
	}
	if _, _, err := f.request(t, "old-version", f.args("configure", "1.0.0", "")); err == nil {
		t.Fatal("new-version action was accepted against the exact old version")
	}
	f.assertRequestCount(t, 0)
	record, _, err := f.request(t, "new-version", f.args("configure", "latest", ""))
	if err != nil || record == nil || record.SkillVersion != "2.0.0" || !reflect.DeepEqual(record.RequiredActions, []string{"read_latest"}) {
		t.Fatalf("selected latest action was validated against retained version: %#v %v", record, err)
	}
}

func TestSkillSetupLatestAccountAllowsAdvertisedMarketplaceNamespace(t *testing.T) {
	f := newLatestSetupTestFixture(t, latestSetupTestSource)
	const skillID = "openseal.meet.voice"
	const source = "clawhub::@alice/openseal.meet.voice"
	f.registerDefinition(t, skillID, "2.0.0", source)
	candidate := latestSetupTestCandidate("2.0.0", source)
	candidate.ID = skillID
	f.candidates = []skill.DiscoveryCandidate{candidate}
	for _, tc := range []struct{ id, source string }{
		{skillID, "clawhub::@bob/openseal.meet.voice"},
		{"openseal.unadvertised", source},
	} {
		args := f.args("configure", "latest", "")
		args["skillId"], args["sourceIdentity"] = tc.id, tc.source
		if _, _, err := f.request(t, "rejected-"+tc.id, args); err == nil {
			t.Fatalf("unadvertised Skill/source was accepted: %#v", tc)
		}
		f.assertRequestCount(t, 0)
	}
	args := f.args("configure", "latest", "")
	args["skillId"], args["sourceIdentity"] = skillID, source
	record, _, err := f.request(t, "authorized", args)
	if err != nil || record == nil || record.SkillID != skillID || record.SourceIdentity != source || record.SkillVersion != "2.0.0" || record.Phase != SkillSetupPhaseConfiguration {
		t.Fatalf("advertised marketplace Skill was blocked by its namespace: %#v %v", record, err)
	}
	f.assertRequestCount(t, 1)
}

// A model that asks again after the user completed setup must not put the
// same setup card back in the chat while the saved account is unchanged.
func TestSkillSetupCompletedSetupDoesNotReappear(t *testing.T) {
	f := newLatestSetupTestFixture(t, "")
	args := map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Connect Reddit"}
	first, _, err := f.request(t, "first", args)
	if err != nil || first.Status != "pending" || first.BindingID != "" {
		t.Fatalf("first request: %#v %v", first, err)
	}
	account := f.bindAccount(t, "saved", "2.0.0", "", f.locations[0], false)
	resolved := cloneSkillSetupRequest(first)
	resolved.Status, resolved.ResolvedBy, resolved.ResolvedBindingID, resolved.ResolvedBindingRevision = "resolved", "user", account.ID, account.Revision
	resolved.Revision++
	if err := f.store.SaveSkillSetupRequest(f.ctx, resolved, first.Revision); err != nil {
		t.Fatal(err)
	}
	again, value, err := f.request(t, "second", args)
	if err != nil || again.ID != first.ID || again.Status != "resolved" || value["status"] != "resolved" {
		t.Fatalf("completed setup was requested again: %#v %#v %v", again, value, err)
	}
	f.assertRequestCount(t, 1)
	// Asking for access the saved account does not grant is a new request.
	wider := map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Read latest", "requiredActions": []interface{}{"read_latest"}}
	if next, _, err := f.request(t, "third", wider); err != nil || next.ID == first.ID || next.Status != "pending" {
		t.Fatalf("new access request: %#v %v", next, err)
	}
	// Credentials that need replacing are always a new request.
	if next, _, err := f.request(t, "fourth", map[string]interface{}{"kind": "reauthorize", "skillId": "reddit.reader", "skillVersion": "latest", "reason": "Reconnect"}); err != nil || next.ID == first.ID || next.Status != "pending" {
		t.Fatalf("reauthorization request: %#v %v", next, err)
	}
	f.assertRequestCount(t, 3)
}
