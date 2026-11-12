package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const sharedAgentAdopter = "person-2"

type sharedAgentFixture struct {
	*agentBuilderDispatchFixture
	installer *AgentInstaller
}

func newSharedAgentFixture(t *testing.T) *sharedAgentFixture {
	t.Helper()
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	oauthAction := func(name string, risk skill.RiskLevel, slot, provider string) skill.Action {
		return skill.Action{
			Name: name, Description: name, Risk: risk, SideEffect: skill.SideEffectRead,
			Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
			Credentials: []skill.CredentialRequirement{{Name: slot, Kind: "oauth", OAuth2: &skill.OAuth2Requirement{
				Provider: provider, Subject: skill.OAuth2SubjectUser, Scopes: []string{provider + ".all"},
			}}},
			InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
		}
	}
	plainAction := func(name string, risk skill.RiskLevel) skill.Action {
		return skill.Action{
			Name: name, Description: name, Risk: risk, SideEffect: skill.SideEffectRead,
			Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
			InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
		}
	}
	create := oauthAction("create_event", skill.RiskLevelWrite, "google", "google")
	create.SideEffect = skill.SideEffectWrite
	crm := plainAction("lookup", skill.RiskLevelRead)
	crm.Credentials = []skill.CredentialRequirement{{Name: "apikey", Kind: "api_key"}}
	for _, definition := range []*skill.Definition{
		{
			ID: "acme.search", Version: "1.0.0", Name: "Search", Description: "Search the web",
			Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://search"},
			Actions:   map[string]skill.Action{"search": plainAction("search", skill.RiskLevelRead)},
		},
		{
			ID: "acme.calendar", Version: "1.0.0", Name: "Calendar", Description: "Calendar",
			Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://calendar"},
			Actions: map[string]skill.Action{
				"list_events": oauthAction("list_events", skill.RiskLevelRead, "google", "google"), "create_event": create,
			},
		},
		{
			ID: "acme.drive", Version: "1.0.0", Name: "Drive", Description: "Drive",
			Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://drive"},
			Actions:   map[string]skill.Action{"list_files": oauthAction("list_files", skill.RiskLevelRead, "drive", "google")},
		},
		{
			ID: "acme.crm", Version: "1.0.0", Name: "CRM", Description: "CRM",
			Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://crm"},
			Actions:   map[string]skill.Action{"lookup": crm},
		},
		{
			ID: sharedAgentMCPSkillID, Version: "1.0.0", Name: "Connector", Description: "MCP connector",
			Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://mcp"},
			Actions:   map[string]skill.Action{"call": plainAction("call", skill.RiskLevelRead)},
		},
		skillbindingtest.SlackConversationSkillDefinition(),
	} {
		if err := f.catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	return &sharedAgentFixture{agentBuilderDispatchFixture: f, installer: f.dispatcher.AgentInstaller}
}

func (f *sharedAgentFixture) bind(t *testing.T, binding *skill.Binding) {
	t.Helper()
	binding.Scope = skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
	binding.Revision = 1
	if err := f.catalog.Bind(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
}

func sharedAgentSpecV1() SharedAgentSpec {
	return SharedAgentSpec{
		Profile: SharedAgentProfile{
			DisplayName: "Planner", Purpose: "Plan the week.", SystemPrompt: "Plan the week from the calendar.",
			Personality: "Calm", OperatingPrinciples: []string{"Be brief."},
		},
		Grants: []SharedAgentSkill{{SkillID: "acme.search", SkillVersion: "1.0.0", Name: "Search", AllowedActions: []string{"search"}}},
		AccountSkills: []SharedAgentSkill{{
			SkillID: "acme.calendar", SkillVersion: "1.0.0", Name: "Calendar", AllowedActions: []string{"list_events"},
			Slots: []SharedAgentSlot{{Slot: "google", Provider: "google"}},
		}},
	}
}

func (f *sharedAgentFixture) installRequest(spec SharedAgentSpec, key string, version int, risk skill.RiskLevel) SharedAgentInstallRequest {
	return SharedAgentInstallRequest{
		Scope: f.scope, ActingUserID: sharedAgentAdopter, IdempotencyKey: key,
		Source: SharedAgentSource{ShareID: "sa1", Version: version}, Profile: spec.Profile, SpaceID: f.salesID,
		Grants: spec.Grants, AccountSkills: spec.AccountSkills, ExpectedMaximumRisk: risk,
	}
}

func (f *sharedAgentFixture) install(t *testing.T, request SharedAgentInstallRequest) *SharedAgentInstalled {
	t.Helper()
	plan, err := f.installer.PlanSharedAgentInstall(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := f.installer.InstallSharedAgent(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	return installed
}

func jsonKeys(t *testing.T, value interface{}) []string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func subsetOf(t *testing.T, what string, keys, allowed []string) {
	t.Helper()
	set := map[string]bool{}
	for _, key := range allowed {
		set[key] = true
	}
	for _, key := range keys {
		if !set[key] {
			t.Fatalf("%s has key %q outside the whitelist %v", what, key, allowed)
		}
	}
}

func TestSharedAgentExportIsAWhitelist(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	f.bind(t, &skill.Binding{
		ID: "builder:researcher:acme.calendar", DeploymentID: "researcher", SkillID: "acme.calendar", SkillVersion: "1.0.0",
		AllowedActions: []string{"list_events"}, MaximumRisk: skill.RiskLevelDestructive,
		Credentials: map[string]skill.CredentialReference{"google": {Kind: "oauth", ID: "vault://creator-google-secret"}},
	})
	f.bind(t, &skill.Binding{
		ID: "builder:researcher:acme.search", DeploymentID: "researcher", SkillID: "acme.search", SkillVersion: "1.0.0",
		AllowedActions: []string{"search"}, MaximumRisk: skill.RiskLevelDestructive,
	})
	drive := &skill.Binding{
		ID: "builder:researcher:acme.drive", DeploymentID: "researcher", SkillID: "acme.drive", SkillVersion: "1.0.0",
		AllowedActions: []string{"list_files"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{"drive": {Kind: "oauth", ID: "vault://drive"}},
	}
	f.bind(t, drive)
	if _, err := f.catalog.DisableBinding(ctx, skill.DisableBindingRequest{
		Scope: drive.Scope, DeploymentID: "researcher", BindingID: drive.ID, ExpectedRevision: 1,
		Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "off",
	}); err != nil {
		t.Fatal(err)
	}

	export, err := f.installer.ExportSharedAgent(ctx, f.scope, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	if got := jsonKeys(t, export); !reflect.DeepEqual(got, []string{"accountSkills", "definitionVersion", "excluded", "grants", "profile", "refusals"}) {
		t.Fatalf("export keys = %v", got)
	}
	subsetOf(t, "profile", jsonKeys(t, export.Profile), []string{"displayName", "purpose", "systemPrompt", "personality", "operatingPrinciples"})
	skillKeys := []string{"skillId", "skillVersion", "sourceIdentity", "name", "allowedActions", "enablePrompt", "slots", "risk"}
	risks := map[string]skill.RiskLevel{}
	for _, value := range append(append([]SharedAgentExportSkill{}, export.Grants...), export.AccountSkills...) {
		subsetOf(t, "skill "+value.SkillID, jsonKeys(t, value), skillKeys)
		risks[value.SkillID] = value.Risk
		for _, slot := range value.Slots {
			subsetOf(t, "slot", jsonKeys(t, slot), []string{"slot", "provider", "providerName"})
		}
	}
	encoded, _ := json.Marshal(export)
	for _, leaked := range []string{"vault://", "creator-google-secret", "maximumRisk", "credentials", "config", "bundled:", "conversation", "destructive"} {
		if strings.Contains(string(encoded), leaked) {
			t.Fatalf("export leaks %q: %s", leaked, encoded)
		}
	}
	if export.Profile.DisplayName != "Researcher" || export.Profile.SystemPrompt != "Work as Researcher." || export.DefinitionVersion != "1.0.0" {
		t.Fatalf("profile = %#v, version %s", export.Profile, export.DefinitionVersion)
	}
	var grantIDs []string
	for _, value := range export.Grants {
		grantIDs = append(grantIDs, value.SkillID)
	}
	if !reflect.DeepEqual(grantIDs, []string{"acme.notes", "acme.search"}) {
		t.Fatalf("grants = %v (the disabled and bundled bindings must stay out)", grantIDs)
	}
	if len(export.AccountSkills) != 1 || export.AccountSkills[0].SkillID != "acme.calendar" ||
		!reflect.DeepEqual(export.AccountSkills[0].Slots, []SharedAgentSlot{{Slot: "google", Provider: "google"}}) {
		t.Fatalf("account skills = %#v", export.AccountSkills)
	}
	if len(export.Refusals) != 0 {
		t.Fatalf("refusals = %#v", export.Refusals)
	}
	// Each skill carries its catalog risk for the public page, never the
	// binding's declared ceiling.
	if !reflect.DeepEqual(risks, map[string]skill.RiskLevel{
		"acme.calendar": skill.RiskLevelRead, "acme.search": skill.RiskLevelRead, "acme.notes": skill.RiskLevelWrite,
	}) {
		t.Fatalf("export risks = %v", risks)
	}

	// The host refuses what only it knows, such as the workspace's own skill.
	if !export.RefuseSkill("acme.calendar", SharedAgentRefusalNotInCatalog) || export.RefuseSkill("acme.calendar", SharedAgentRefusalNotInCatalog) {
		t.Fatal("RefuseSkill must refuse a listed skill once")
	}
	if len(export.AccountSkills) != 0 || len(export.Grants) != 2 ||
		!reflect.DeepEqual(export.Refusals, []SharedAgentRefusal{{SkillID: "acme.calendar", Name: "Calendar", Reason: SharedAgentRefusalNotInCatalog}}) {
		t.Fatalf("after RefuseSkill: grants %#v, account skills %#v, refusals %#v", export.Grants, export.AccountSkills, export.Refusals)
	}
}

func TestSharedAgentExportSlotsMatchInstall(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	action := func(name string, credentials ...skill.CredentialRequirement) skill.Action {
		return skill.Action{
			Name: name, Description: name, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
			Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, Credentials: credentials,
			InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
		}
	}
	google := skill.CredentialRequirement{Name: "google", Kind: "oauth", OAuth2: &skill.OAuth2Requirement{
		Provider: "google", Subject: skill.OAuth2SubjectUser, Scopes: []string{"google.all"},
	}}
	if err := f.catalog.Register(ctx, &skill.Definition{
		ID: "acme.mail", Version: "1.0.0", Name: "Mail", Description: "Mail",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://mail"},
		Actions: map[string]skill.Action{
			// An optional API key the binding never fills, and an OAuth slot
			// only an action that is not allowed uses.
			"list_labels": action("list_labels", skill.CredentialRequirement{Name: "apikey", Kind: "api_key", Optional: true}),
			"send":        action("send", google),
		},
	}); err != nil {
		t.Fatal(err)
	}
	f.bind(t, &skill.Binding{
		ID: "builder:researcher:acme.mail", DeploymentID: "researcher", SkillID: "acme.mail", SkillVersion: "1.0.0",
		AllowedActions: []string{"list_labels"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{"google": {Kind: "oauth", ID: "vault://stale-google"}},
	})
	export, err := f.installer.ExportSharedAgent(ctx, f.scope, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	if len(export.Refusals) != 0 {
		t.Fatalf("refusals = %#v", export.Refusals)
	}
	var mail *SharedAgentExportSkill
	for index := range export.Grants {
		if export.Grants[index].SkillID == "acme.mail" {
			mail = &export.Grants[index]
		}
	}
	if mail == nil || len(mail.Slots) != 0 {
		t.Fatalf("mail must export as a plain grant: grants %#v, account skills %#v", export.Grants, export.AccountSkills)
	}
	spec := sharedAgentSpecV1()
	spec.Grants = []SharedAgentSkill{mail.SharedAgentSkill}
	spec.AccountSkills = nil
	plan, err := f.installer.PlanSharedAgentInstall(ctx, f.installRequest(spec, "", 1, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Skills) != 1 || !plan.Skills[0].Available || len(plan.Slots) != 0 {
		t.Fatalf("mail plan = skills %#v, slots %#v", plan.Skills, plan.Slots)
	}

	// Once an allowed action uses the slot, it is exported and installs as an
	// account skill.
	if err := f.catalog.Bind(ctx, &skill.Binding{
		Scope: skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}, Revision: 2,
		ID: "builder:researcher:acme.mail", DeploymentID: "researcher", SkillID: "acme.mail", SkillVersion: "1.0.0",
		AllowedActions: []string{"list_labels", "send"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{"google": {Kind: "oauth", ID: "vault://google"}},
	}); err != nil {
		t.Fatal(err)
	}
	export, err = f.installer.ExportSharedAgent(ctx, f.scope, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	if len(export.AccountSkills) != 1 || export.AccountSkills[0].SkillID != "acme.mail" ||
		!reflect.DeepEqual(export.AccountSkills[0].Slots, []SharedAgentSlot{{Slot: "google", Provider: "google"}}) {
		t.Fatalf("account skills = %#v, refusals %#v", export.AccountSkills, export.Refusals)
	}
	spec.Grants = nil
	spec.AccountSkills = []SharedAgentSkill{export.AccountSkills[0].SharedAgentSkill}
	plan, err = f.installer.PlanSharedAgentInstall(ctx, f.installRequest(spec, "", 1, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Skills) != 1 || !plan.Skills[0].Available || len(plan.Slots) != 1 || plan.Slots[0].Slot != "google" {
		t.Fatalf("mail account plan = skills %#v, slots %#v", plan.Skills, plan.Slots)
	}
}

func TestSharedAgentExportRefusals(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	f.bind(t, &skill.Binding{
		ID: "builder:researcher:acme.crm", DeploymentID: "researcher", SkillID: "acme.crm", SkillVersion: "1.0.0",
		AllowedActions: []string{"lookup"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{"apikey": {Kind: "api_key", ID: "vault://crm-key"}},
	})
	f.bind(t, &skill.Binding{
		ID: "slack-channel", DeploymentID: "researcher", SkillID: "slack", SkillVersion: "1.0.0",
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "vault://slack"}},
	})
	f.bind(t, &skill.Binding{
		ID: "mcp-app", DeploymentID: "researcher", SkillID: sharedAgentMCPSkillID, SkillVersion: "1.0.0",
		AllowedActions: []string{"call"}, MaximumRisk: skill.RiskLevelRead,
	})
	export, err := f.installer.ExportSharedAgent(ctx, f.scope, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	want := []SharedAgentRefusal{
		{SkillID: "acme.crm", Name: "CRM", Reason: SharedAgentRefusalVaultCredential},
		{SkillID: sharedAgentMCPSkillID, Name: "Connector", Reason: SharedAgentRefusalNotInCatalog},
		{SkillID: "slack", Name: "Slack", Reason: SharedAgentRefusalChannelAdapter},
	}
	if !reflect.DeepEqual(export.Refusals, want) {
		t.Fatalf("refusals = %#v", export.Refusals)
	}
	encoded, _ := json.Marshal(export)
	if strings.Contains(string(encoded), "vault://") {
		t.Fatalf("refused skills leak credentials: %s", encoded)
	}
	if _, err := f.installer.ExportSharedAgent(ctx, f.scope, "finn"); !errors.Is(err, ErrSharedAgentNotShareable) {
		t.Fatalf("export of Finn = %v", err)
	}
	if _, err := f.installer.ExportSharedAgent(ctx, f.scope, "old-agent"); !errors.Is(err, kernelagent.ErrDeploymentNotFound) {
		t.Fatalf("export of a retired agent = %v", err)
	}
}

func TestSharedAgentPlanResolvesAgainstTheCatalog(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	spec := sharedAgentSpecV1()
	spec.Profile.DisplayName = "Researcher"
	spec.AccountSkills[0].AllowedActions = []string{"list_events", "create_event"}
	spec.Grants = append(spec.Grants,
		SharedAgentSkill{SkillID: "acme.unknown", SkillVersion: "1.0.0", Name: "Unknown", AllowedActions: []string{"x"}},
		SharedAgentSkill{SkillID: "acme.drive", SkillVersion: "1.0.0", AllowedActions: []string{"list_files"}},
	)
	spec.AccountSkills = append(spec.AccountSkills, SharedAgentSkill{SkillID: "acme.crm", SkillVersion: "1.0.0", AllowedActions: []string{"lookup"}})

	// A preview marks unavailable skills and resolves the name collision.
	preview := f.installRequest(spec, "", 1, "")
	plan, err := f.installer.PlanSharedAgentInstall(ctx, preview)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DisplayName != "Researcher 2" || plan.DeploymentID != "" || plan.MaximumRisk != skill.RiskLevelWrite {
		t.Fatalf("preview plan = %#v", plan)
	}
	availability := map[string]bool{}
	for _, planned := range plan.Skills {
		availability[planned.SkillID] = planned.Available
		if !planned.Available && planned.Reason != SharedAgentSkillUnavailable {
			t.Fatalf("unavailable skill reason = %#v", planned)
		}
	}
	// A grant that needs an account and an account skill on an API key are
	// unavailable, like a skill this catalog lacks.
	if !reflect.DeepEqual(availability, map[string]bool{
		"acme.search": true, "acme.calendar": true, "acme.unknown": false, "acme.drive": false, "acme.crm": false,
	}) {
		t.Fatalf("availability = %v", availability)
	}
	var calendar *skill.Binding
	for _, binding := range plan.Bindings {
		if binding.SkillID == "acme.calendar" {
			calendar = binding
		}
	}
	if calendar == nil || calendar.MaximumRisk != skill.RiskLevelWrite ||
		!reflect.DeepEqual(calendar.Credentials, map[string]skill.CredentialReference{"google": {Kind: "oauth", ID: skill.PersonCredentialReferenceID}}) {
		t.Fatalf("account skill binding = %#v", calendar)
	}
	if len(plan.Slots) != 1 || plan.Slots[0].Slot != "google" || plan.Slots[0].Provider != "google" || !plan.Slots[0].PersonAllowed {
		t.Fatalf("slots = %#v", plan.Slots)
	}

	// An install refuses the unavailable skill by name.
	install := f.installRequest(spec, "adoption-1", 1, skill.RiskLevelWrite)
	install.DisplayName = "Researcher 2"
	_, err = f.installer.PlanSharedAgentInstall(ctx, install)
	var unavailable SharedAgentSkillUnavailableError
	if !errors.Is(err, ErrSharedAgentSkillUnavailable) || !errors.As(err, &unavailable) || unavailable.SkillID != "acme.unknown" {
		t.Fatalf("install with an unavailable skill = %v", err)
	}

	// The ceiling counts account skills and is never the snapshot's.
	spec = sharedAgentSpecV1()
	spec.AccountSkills[0].AllowedActions = []string{"list_events", "create_event"}
	if _, err := f.installer.PlanSharedAgentInstall(ctx, f.installRequest(spec, "adoption-1", 1, skill.RiskLevelRead)); !errors.Is(err, ErrSharedAgentReviewChanged) {
		t.Fatalf("install reviewed at read = %v", err)
	}
	plan, err = f.installer.PlanSharedAgentInstall(ctx, f.installRequest(spec, "adoption-1", 1, skill.RiskLevelWrite))
	if err != nil || plan.MaximumRisk != skill.RiskLevelWrite || plan.DeploymentID == "" ||
		plan.Slots[0].BindingID != agentBuilderBindingID(plan.DeploymentID, "acme.calendar") {
		t.Fatalf("install plan = %#v, %v", plan, err)
	}

	// A requested name must be free.
	taken := f.installRequest(sharedAgentSpecV1(), "adoption-2", 1, skill.RiskLevelRead)
	taken.DisplayName = "Researcher"
	if _, err := f.installer.PlanSharedAgentInstall(ctx, taken); !errors.Is(err, ErrAgentNameTaken) {
		t.Fatalf("install with a taken name = %v", err)
	}
	// Secrets in the profile are refused like in create_agent.
	secret := f.installRequest(sharedAgentSpecV1(), "adoption-3", 1, skill.RiskLevelRead)
	secret.Profile.SystemPrompt = "Use the key sk-ant-api03-" + strings.Repeat("a", 90)
	if _, err := f.installer.PlanSharedAgentInstall(ctx, secret); err == nil {
		t.Fatal("a secret in the instructions was accepted")
	}
}

func TestSharedAgentInstall(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	request := f.installRequest(sharedAgentSpecV1(), "adoption-1", 1, skill.RiskLevelRead)
	installed := f.install(t, request)
	if installed.Replayed || installed.DisplayName != "Planner" || installed.Space == nil || installed.Space.ID != f.salesID || installed.ConversationID == "" {
		t.Fatalf("installed = %#v", installed)
	}
	if installed.AgentID != agentBuilderResourceID("agent-deployment", f.scope, "shared:adoption-1") || !f.spaceMember(t, f.salesID, installed.AgentID) {
		t.Fatalf("copy %s is not in its space", installed.AgentID)
	}
	deployment, err := f.agents.GetDeployment(ctx, f.reference(), installed.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := f.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Provenance.Source != SharedAgentProvenanceSource || definition.Provenance.Reference != "sa1@1" ||
		definition.Provenance.CreatedBy != sharedAgentAdopter || definition.Authority.MaximumRisk != capability.RiskLevelRead ||
		definition.SystemPrompt != "Plan the week from the calendar." {
		t.Fatalf("definition = %#v", definition)
	}
	if len(f.recorder.prepared) != 1 || f.recorder.prepared[0].OwnerUserID != sharedAgentAdopter || f.recorder.prepared[0].ActionCallID != "shared:adoption-1" {
		t.Fatalf("model prepared = %#v", f.recorder.prepared)
	}
	if len(f.recorder.created) != 1 || f.recorder.created[0].RunID != "" || f.recorder.created[0].InstallKey != "adoption-1" ||
		f.recorder.created[0].BillingUserID != sharedAgentAdopter {
		t.Fatalf("after created = %#v", f.recorder.created)
	}
	if len(installed.Bindings) != 2 || len(installed.Unconnected) != 1 || installed.Unconnected[0].Slot != "google" {
		t.Fatalf("bindings %#v, unconnected %#v", installed.Bindings, installed.Unconnected)
	}
	for _, binding := range installed.Bindings {
		if binding.SkillID == "acme.calendar" && binding.Credentials["google"].ID != skill.PersonCredentialReferenceID {
			t.Fatalf("calendar binding = %#v", binding)
		}
	}

	// A replay returns the same Agent and creates nothing.
	replay := f.install(t, request)
	if !replay.Replayed || replay.AgentID != installed.AgentID || f.provision.created != 1 {
		t.Fatalf("replay = %#v, deployments created %d", replay, f.provision.created)
	}

	// A copy can't be shared again.
	if _, err := f.installer.ExportSharedAgent(ctx, f.scope, installed.AgentID); !errors.Is(err, ErrSharedAgentIsCopy) || !errors.Is(err, ErrSharedAgentNotShareable) {
		t.Fatalf("export of a copy = %v", err)
	}

	// The name is now taken for an explicit second install.
	second := f.installRequest(sharedAgentSpecV1(), "adoption-2", 1, skill.RiskLevelRead)
	second.DisplayName = "Planner"
	if _, err := f.installer.PlanSharedAgentInstall(ctx, second); !errors.Is(err, ErrAgentNameTaken) {
		t.Fatalf("second install with the same name = %v", err)
	}
	// Even a plan built before the copy fails at install.
	second.DisplayName = ""
	plan, err := f.installer.PlanSharedAgentInstall(ctx, second)
	if err != nil || plan.DisplayName != "Planner 2" {
		t.Fatalf("second plan = %#v, %v", plan, err)
	}
}

func TestSharedAgentInstallNameTakenAfterPlan(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	first, err := f.installer.PlanSharedAgentInstall(ctx, f.installRequest(sharedAgentSpecV1(), "adoption-1", 1, skill.RiskLevelRead))
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.installer.PlanSharedAgentInstall(ctx, f.installRequest(sharedAgentSpecV1(), "adoption-2", 1, skill.RiskLevelRead))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.InstallSharedAgent(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.InstallSharedAgent(ctx, second); !errors.Is(err, ErrAgentNameTaken) {
		t.Fatalf("install after the name was taken = %v", err)
	}
	if _, err := f.installer.InstallSharedAgent(ctx, &SharedAgentInstallPlan{DeploymentID: "forged"}); err == nil {
		t.Fatal("a plan the kernel did not resolve was installed")
	}
}

func TestSharedAgentInstallUsesTheHostsSlotReference(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	plan, err := f.installer.PlanSharedAgentInstall(ctx, f.installRequest(sharedAgentSpecV1(), "adoption-1", 1, skill.RiskLevelRead))
	if err != nil {
		t.Fatal(err)
	}
	var calendar *skill.Binding
	for _, binding := range plan.Bindings {
		if binding.SkillID == "acme.calendar" {
			calendar = binding
		}
	}
	calendar.Credentials = map[string]skill.CredentialReference{"google": {Kind: "api_key", ID: "vault://wrong-kind"}}
	if _, err := f.installer.InstallSharedAgent(ctx, plan); err == nil || !strings.Contains(err.Error(), "needs a oauth account") {
		t.Fatalf("install with a wrong-kind reference = %v", err)
	}
	calendar.Credentials = map[string]skill.CredentialReference{"other": {Kind: "oauth", ID: "vault://x"}}
	if _, err := f.installer.InstallSharedAgent(ctx, plan); err == nil {
		t.Fatal("install with an unknown slot was accepted")
	}
	// Exported fields other than the slot references are not read back.
	calendar.Credentials = map[string]skill.CredentialReference{"google": {Kind: "oauth", ID: "vault://adopter-google"}}
	calendar.AllowedActions = []string{"list_events", "create_event"}
	calendar.MaximumRisk = skill.RiskLevelDestructive
	plan.MaximumRisk = skill.RiskLevelDestructive
	installed, err := f.installer.InstallSharedAgent(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed.Unconnected) != 0 {
		t.Fatalf("unconnected = %#v", installed.Unconnected)
	}
	binding, err := f.catalog.GetBinding(ctx, skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}, installed.AgentID, agentBuilderBindingID(installed.AgentID, "acme.calendar"))
	if err != nil || binding.Credentials["google"].ID != "vault://adopter-google" || binding.MaximumRisk != skill.RiskLevelRead ||
		!reflect.DeepEqual(binding.AllowedActions, []string{"list_events"}) {
		t.Fatalf("calendar binding = %#v, %v", binding, err)
	}
}

func TestSharedAgentInstallRetiresOnBindFailure(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	f.provision.putErr = errors.New("space store unavailable")
	plan, err := f.installer.PlanSharedAgentInstall(ctx, f.installRequest(sharedAgentSpecV1(), "adoption-1", 1, skill.RiskLevelRead))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.InstallSharedAgent(ctx, plan); err == nil || !strings.Contains(err.Error(), "space store unavailable") {
		t.Fatalf("install with a failing placement = %v", err)
	}
	deployment, err := f.agents.GetDeployment(ctx, f.reference(), plan.DeploymentID)
	if err != nil || deployment.RolloutStatus != kernelagent.RolloutRetired {
		t.Fatalf("deployment after the failure = %#v, %v", deployment, err)
	}
	if len(f.recorder.created) != 0 {
		t.Fatalf("AfterAgentCreated ran for a failed install: %#v", f.recorder.created)
	}
	f.provision.putErr = nil
	retry := f.installRequest(sharedAgentSpecV1(), "adoption-2", 1, skill.RiskLevelRead)
	retry.DisplayName = "Planner"
	if installed := f.install(t, retry); installed.DisplayName != "Planner" {
		t.Fatalf("retry = %#v", installed)
	}
}

func TestSharedAgentRetireInstall(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	installed := f.install(t, f.installRequest(sharedAgentSpecV1(), "adoption-1", 1, skill.RiskLevelRead))
	if err := f.installer.RetireSharedAgentInstall(ctx, f.scope, "researcher", sharedAgentAdopter); !errors.Is(err, ErrSharedAgentNotCopy) {
		t.Fatalf("retire of a non-copy = %v", err)
	}
	for range 2 {
		if err := f.installer.RetireSharedAgentInstall(ctx, f.scope, installed.AgentID, sharedAgentAdopter); err != nil {
			t.Fatal(err)
		}
	}
	deployment, err := f.agents.GetDeployment(ctx, f.reference(), installed.AgentID)
	if err != nil || deployment.RolloutStatus != kernelagent.RolloutRetired {
		t.Fatalf("deployment = %#v, %v", deployment, err)
	}
	again := f.installRequest(sharedAgentSpecV1(), "adoption-2", 1, skill.RiskLevelRead)
	again.DisplayName = "Planner"
	if _, err := f.installer.PlanSharedAgentInstall(ctx, again); err != nil {
		t.Fatalf("the retired copy's name is not free: %v", err)
	}
}

func sharedAgentSpecV2() SharedAgentSpec {
	spec := sharedAgentSpecV1()
	spec.Profile.Purpose = "Plan the week and take notes."
	spec.Profile.SystemPrompt = "Plan the week from the calendar and write notes."
	spec.Grants = []SharedAgentSkill{{SkillID: "acme.notes", SkillVersion: "1.0.0", Name: "Notes", AllowedActions: []string{"write_note"}}}
	spec.AccountSkills = []SharedAgentSkill{
		{
			SkillID: "acme.calendar", SkillVersion: "1.0.0", Name: "Calendar", AllowedActions: []string{"list_events", "create_event"},
			Slots: []SharedAgentSlot{{Slot: "google", Provider: "google"}},
		},
		{
			SkillID: "acme.drive", SkillVersion: "1.0.0", Name: "Drive", AllowedActions: []string{"list_files"},
			Slots: []SharedAgentSlot{{Slot: "drive", Provider: "google"}},
		},
	}
	return spec
}

func TestSharedAgentUpdate(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	plan, err := f.installer.PlanSharedAgentInstall(ctx, f.installRequest(sharedAgentSpecV1(), "adoption-1", 1, skill.RiskLevelRead))
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range plan.Bindings {
		if binding.SkillID == "acme.calendar" {
			binding.Credentials = map[string]skill.CredentialReference{"google": {Kind: "oauth", ID: "vault://adopter-google"}}
		}
	}
	installed, err := f.installer.InstallSharedAgent(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	reference := f.reference()
	// The adopter edits the copy's purpose.
	deployment, _ := f.agents.GetDeployment(ctx, reference, installed.AgentID)
	definition, _ := f.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	edited := cloneAgentDefinitionForAction(definition)
	edited.Version = "1.0.1"
	edited.Purpose = "My own purpose."
	if _, _, err := f.installer.activateApprovedAgentVersion(ctx, reference, deployment, edited, sharedAgentAdopter); err != nil {
		t.Fatal(err)
	}
	deployment, _ = f.agents.GetDeployment(ctx, reference, installed.AgentID)

	request := SharedAgentUpdateRequest{
		Scope: f.scope, DeploymentID: installed.AgentID, ActingUserID: sharedAgentAdopter, IdempotencyKey: "update-1",
		Source: SharedAgentSource{ShareID: "sa1", Version: 2}, From: sharedAgentSpecV1(), To: sharedAgentSpecV2(),
		ExpectedDeploymentRevision: deployment.Revision, ExpectedMaximumRisk: skill.RiskLevelWrite,
	}
	preview, err := f.installer.PreviewSharedAgentUpdate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]SharedAgentChange{}
	for _, change := range preview.Changes {
		changes[change.Kind+":"+change.Field+":"+change.Target] = change
	}
	if len(preview.Changes) != 6 {
		t.Fatalf("changes = %#v", preview.Changes)
	}
	instructions := changes["profile:instructions:"]
	if instructions.Before != "Plan the week from the calendar." || instructions.After != "Plan the week from the calendar and write notes." {
		t.Fatalf("instructions change = %#v", instructions)
	}
	if purpose := changes["profile:purpose:"]; purpose.Before != "" || purpose.After != "" || purpose.Label != "Purpose" {
		t.Fatalf("purpose change carries text: %#v", purpose)
	}
	if !changes["skill_added:skills:acme.notes"].RiskUp || changes["skill_added:skills:acme.drive"].RiskUp ||
		!changes["skill_changed:skills:acme.calendar"].RiskUp {
		t.Fatalf("risk marks = %#v", preview.Changes)
	}
	if _, ok := changes["skill_removed:skills:acme.search"]; !ok {
		t.Fatalf("search removal missing: %#v", preview.Changes)
	}
	if !reflect.DeepEqual(preview.ReplacesYourEdit, []string{"purpose"}) {
		t.Fatalf("replaces your edit = %v", preview.ReplacesYourEdit)
	}
	if preview.Risk != (SharedAgentRiskChange{From: skill.RiskLevelRead, To: skill.RiskLevelWrite}) || preview.DeploymentRevision != deployment.Revision {
		t.Fatalf("risk = %#v, revision %d", preview.Risk, preview.DeploymentRevision)
	}
	if len(preview.NewSlots) != 1 || preview.NewSlots[0].SkillID != "acme.drive" || preview.NewSlots[0].Slot != "drive" || len(preview.Unavailable) != 0 {
		t.Fatalf("new slots %#v, unavailable %#v", preview.NewSlots, preview.Unavailable)
	}

	stale := request
	stale.ExpectedDeploymentRevision = deployment.Revision - 1
	if _, err := f.installer.ApplySharedAgentUpdate(ctx, stale); !errors.Is(err, kernelagent.ErrRevisionConflict) {
		t.Fatalf("stale update = %v", err)
	}
	reviewedLow := request
	reviewedLow.ExpectedMaximumRisk = skill.RiskLevelRead
	if _, err := f.installer.ApplySharedAgentUpdate(ctx, reviewedLow); !errors.Is(err, ErrSharedAgentReviewChanged) {
		t.Fatalf("update reviewed at read = %v", err)
	}

	updated, err := f.installer.ApplySharedAgentUpdate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	deployment, _ = f.agents.GetDeployment(ctx, reference, installed.AgentID)
	definition, _ = f.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if updated.DefinitionVersion != deployment.ActiveVersion || definition.Purpose != "Plan the week and take notes." ||
		definition.SystemPrompt != "Plan the week from the calendar and write notes." || definition.Provenance.Reference != "sa1@2" ||
		definition.Provenance.Source != SharedAgentProvenanceSource || definition.Authority.MaximumRisk != capability.RiskLevelWrite {
		t.Fatalf("updated definition = %#v", definition)
	}
	if updated.Activation == nil || updated.Activation.ActorID != sharedAgentAdopter {
		t.Fatalf("activation = %#v", updated.Activation)
	}
	skillScope := skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
	binding := func(id string) *skill.Binding {
		t.Helper()
		value, err := f.catalog.GetBinding(ctx, skillScope, installed.AgentID, agentBuilderBindingID(installed.AgentID, id))
		if err != nil {
			t.Fatalf("binding %s: %v", id, err)
		}
		return value
	}
	if !binding("acme.search").Disabled || binding("acme.notes").Disabled {
		t.Fatal("search must be disabled and notes enabled")
	}
	calendar := binding("acme.calendar")
	if calendar.Credentials["google"].ID != "vault://adopter-google" || calendar.MaximumRisk != skill.RiskLevelWrite ||
		!reflect.DeepEqual(calendar.AllowedActions, []string{"create_event", "list_events"}) {
		t.Fatalf("calendar after the update = %#v", calendar)
	}
	if drive := binding("acme.drive"); drive.Credentials["drive"].ID != skill.PersonCredentialReferenceID {
		t.Fatalf("drive = %#v", drive)
	}
	if len(updated.Unconnected) != 1 || updated.Unconnected[0].SkillID != "acme.drive" {
		t.Fatalf("unconnected = %#v", updated.Unconnected)
	}

	// A replay converges on the same version without activating again.
	activations, _ := f.agents.ListActivations(ctx, reference, installed.AgentID)
	replay, err := f.installer.ApplySharedAgentUpdate(ctx, request)
	if err != nil || replay.DefinitionVersion != updated.DefinitionVersion {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	if again, _ := f.agents.ListActivations(ctx, reference, installed.AgentID); len(again) != len(activations) {
		t.Fatalf("replay activated again: %d -> %d", len(activations), len(again))
	}
	// Still a copy after the update.
	if _, err := f.installer.ExportSharedAgent(ctx, f.scope, installed.AgentID); !errors.Is(err, ErrSharedAgentIsCopy) {
		t.Fatalf("export of an updated copy = %v", err)
	}
}

func TestSharedAgentUpdateReplacesYourSkillEdit(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	installed := f.install(t, f.installRequest(sharedAgentSpecV1(), "adoption-1", 1, skill.RiskLevelRead))
	skillScope := skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
	actor := skill.BindingActor{Type: "user", ID: sharedAgentAdopter}
	calendarID := agentBuilderBindingID(installed.AgentID, "acme.calendar")
	request := func() SharedAgentUpdateRequest {
		deployment, _ := f.agents.GetDeployment(ctx, f.reference(), installed.AgentID)
		return SharedAgentUpdateRequest{
			Scope: f.scope, DeploymentID: installed.AgentID, ActingUserID: sharedAgentAdopter,
			Source: SharedAgentSource{ShareID: "sa1", Version: 2}, From: sharedAgentSpecV1(), To: sharedAgentSpecV2(),
			ExpectedDeploymentRevision: deployment.Revision,
		}
	}
	calendarChange := func(preview *SharedAgentUpdatePreview) SharedAgentChange {
		for _, change := range preview.Changes {
			if change.Target == "acme.calendar" {
				return change
			}
		}
		t.Fatalf("no calendar change: %#v", preview.Changes)
		return SharedAgentChange{}
	}

	// The adopter turns the calendar off; v2 changes it.
	if err := f.installer.disableAgentSkill(ctx, skillScope, installed.AgentID, calendarID, actor); err != nil {
		t.Fatal(err)
	}
	preview, err := f.installer.PreviewSharedAgentUpdate(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preview.ReplacesYourEdit, []string{SharedAgentFieldSkills}) {
		t.Fatalf("replaces your edit = %v", preview.ReplacesYourEdit)
	}
	if change := calendarChange(preview); change.Kind != SharedAgentChangeSkillChanged || change.Label != "Turns Calendar back on with changes" {
		t.Fatalf("calendar change = %#v", change)
	}

	// The adopter changed the calendar's actions instead.
	if _, err := f.installer.grantAgentSkill(ctx, f.scope, installed.AgentID, AgentSkillGrant{
		SkillID: "acme.calendar", SkillVersion: "1.0.0", AllowedActions: []string{"list_events", "create_event"}, MaximumRisk: skill.RiskLevelWrite,
	}, map[string]skill.CredentialReference{"google": {Kind: "oauth", ID: skill.PersonCredentialReferenceID}}, actor, "edit"); err != nil {
		t.Fatal(err)
	}
	preview, err = f.installer.PreviewSharedAgentUpdate(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preview.ReplacesYourEdit, []string{SharedAgentFieldSkills}) || calendarChange(preview).Label != "Changes Calendar" {
		t.Fatalf("replaces your edit = %v, calendar %#v", preview.ReplacesYourEdit, calendarChange(preview))
	}
}

func TestSharedAgentUpdateRefusals(t *testing.T) {
	ctx := context.Background()
	f := newSharedAgentFixture(t)
	installed := f.install(t, f.installRequest(sharedAgentSpecV1(), "adoption-1", 1, skill.RiskLevelRead))
	deployment, _ := f.agents.GetDeployment(ctx, f.reference(), installed.AgentID)
	request := SharedAgentUpdateRequest{
		Scope: f.scope, DeploymentID: "researcher", ActingUserID: sharedAgentAdopter, IdempotencyKey: "update-1",
		Source: SharedAgentSource{ShareID: "sa1", Version: 2}, From: sharedAgentSpecV1(), To: sharedAgentSpecV2(),
		ExpectedDeploymentRevision: 1, ExpectedMaximumRisk: skill.RiskLevelWrite,
	}
	if _, err := f.installer.PreviewSharedAgentUpdate(ctx, request); !errors.Is(err, ErrSharedAgentNotCopy) {
		t.Fatalf("update preview of a non-copy = %v", err)
	}
	request.DeploymentID = installed.AgentID
	request.ExpectedDeploymentRevision = deployment.Revision
	request.To.Grants = append(request.To.Grants, SharedAgentSkill{SkillID: "acme.unknown", SkillVersion: "1.0.0", AllowedActions: []string{"x"}})
	preview, err := f.installer.PreviewSharedAgentUpdate(ctx, request)
	if err != nil || len(preview.Unavailable) != 1 || preview.Unavailable[0].SkillID != "acme.unknown" {
		t.Fatalf("preview with an unavailable skill = %#v, %v", preview, err)
	}
	if _, err := f.installer.ApplySharedAgentUpdate(ctx, request); !errors.Is(err, ErrSharedAgentSkillUnavailable) {
		t.Fatalf("update with an unavailable skill = %v", err)
	}
	// A creator's rename to a name the adopter uses resolves the collision.
	request.To = sharedAgentSpecV1()
	request.To.Profile.DisplayName = "Researcher"
	request.ExpectedMaximumRisk = skill.RiskLevelRead
	if _, err := f.installer.ApplySharedAgentUpdate(ctx, request); err != nil {
		t.Fatal(err)
	}
	deployment, _ = f.agents.GetDeployment(ctx, f.reference(), installed.AgentID)
	definition, _ := f.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if definition.DisplayName != "Researcher 2" {
		t.Fatalf("renamed copy = %q", definition.DisplayName)
	}
	if !sharedAgentNameMatches("Planner 3", "Planner") || sharedAgentNameMatches("Planner Pro", "Planner") {
		t.Fatal("collision variants are not recognised")
	}
}
