package skillbinding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// SkillActionFindApps searches the host's shared catalog of connectable apps
// (remote MCP servers). It is read-only and available to every Agent, with
// or without connections.
const SkillActionFindApps = "find_apps"

// ConnectAppWakeType parks the requesting Run until the requested app is
// connected for it or the card is dismissed.
const ConnectAppWakeType = "connect_app"

const (
	maximumFindAppsLimit = 8
	defaultFindAppsLimit = 5
	maximumAppQueryBytes = 200
)

// Result types of find_apps. Each result names its type and the one action
// that sets it up: an app (a remote MCP server) is connected with
// request_connection, a Skill is installed or configured with request_setup.
// Neither action accepts the other type.
const (
	FindAppsTypeApp    = "mcp-server"
	FindAppsTypeSkill  = "skill"
	FindAppsSetupApp   = agentrun.SkillActionRequestConnection
	FindAppsSetupSkill = agentrun.SkillActionRequestSetup
)

// AppCatalogEntry is one connectable app as the host's catalog lists it.
type AppCatalogEntry struct {
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Summary        string `json:"summary,omitempty"`
	Official       bool   `json:"official"`
	SignIn         string `json:"signIn"`
	LogoSHA256     string `json:"logoSha256,omitempty"`
	LogoMonochrome bool   `json:"logoMonochrome,omitempty"`
}

// AppCatalog is the host's shared catalog of connectable apps. Entries with
// no connectable endpoint are left out.
type AppCatalog interface {
	FindApps(ctx context.Context, scope models.Scope, query string, limit int) ([]AppCatalogEntry, error)
	// GetApp returns nil, nil when no entry has the slug.
	GetApp(ctx context.Context, scope models.Scope, slug string) (*AppCatalogEntry, error)
}

// isCatalogApp reports whether id is the slug of an app in the host's
// catalog, so a Skill setup naming an app can say how to connect it.
func (d *ActionDispatcher) isCatalogApp(ctx context.Context, scope models.Scope, id string) bool {
	catalog := d.apps.get()
	if catalog == nil || !models.IsValidAppSlug(id) {
		return false
	}
	entry, err := catalog.GetApp(ctx, scope, id)
	return err == nil && entry != nil
}

// AppCatalogRef holds the host's app catalog, which hosts install after the
// engine (and its action dispatchers) exist.
type AppCatalogRef struct {
	mu      sync.RWMutex
	catalog AppCatalog
}

func (r *AppCatalogRef) Set(catalog AppCatalog) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.catalog = catalog
	r.mu.Unlock()
}

func (r *AppCatalogRef) get() AppCatalog {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.catalog
}

// UseAppCatalog gives the dispatcher the host's app catalog for find_apps
// and request_connection.
func (d *ActionDispatcher) UseAppCatalog(ref *AppCatalogRef) {
	if d != nil {
		d.apps = ref
	}
}

const connectAppGuidance = "Use this when the user wants to connect or use a service and no connected-app tools (mcp.<app>.*) cover it."

func findAppsAction() skill.Action {
	return skill.Action{Name: SkillActionFindApps,
		Description: "Search what can be connected for every Agent: catalog apps (remote MCP servers) and Skills. " + connectAppGuidance + " Query with the service name, for example Linear. Every result has a type and the one action that sets it up (setup). Apps (type mcp-server, setup request_connection) are connected, never installed: a remote server the user signs in to. They list slug, name, whether the vendor maintains it (official), how it signs in, and whether it is already connected for the person asking (then its tools are listed); call request_connection with the slug. Skills (type skill, setup request_setup) are installed and configured for this Agent: call request_setup with its skillId, skillVersion and sourceIdentity, and its setupKind as kind. Never pass an app to request_setup or a Skill to request_connection. Prefer an app that covers the service; when no app does, use a matching Skill.",
		Risk:        skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
			"query": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maximumAppQueryBytes},
			"limit": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": maximumFindAppsLimit},
		}, "required": []interface{}{"query"}},
		OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
			"apps":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}},
			"skills": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}},
		}, "required": []interface{}{"apps", "skills"}}}
}

func requestConnectionAction() skill.Action {
	return skill.Action{Name: agentrun.SkillActionRequestConnection,
		Description: "Ask the person who sent the message to connect the app through a Connect card in this chat. If the app is shared with the agent or team, ask someone who manages it. " + connectAppGuidance + " Pass app as the exact slug from find_apps, or as {\"address\": \"https://...\"} when the user gave an MCP server address. The user signs in on the card; never ask for keys or passwords in chat. A pending result pauses this work until the app is connected (you then continue with its new tools) or the card is dismissed. If the app is already connected, the result lists its tools instead: use them. If the result is refused, explain the reason and do not repeat the request in this reply. With targetAgentId, the card still appears in this chat and the app is connected for that Agent.",
		Risk:        skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
			"app": map[string]interface{}{"anyOf": []interface{}{
				map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128, "description": "Catalog slug copied from find_apps."},
				map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
					"address": map[string]interface{}{"type": "string", "minLength": 9, "maxLength": models.MaximumAppAddress, "description": "The MCP server's https address."},
				}, "required": []interface{}{"address"}},
			}},
			"reason":                       map[string]interface{}{"type": "string", "maxLength": 1000, "description": "Optional; shown on the card, for example \"Connect Linear so I can list your open issues\"."},
			SkillActionTargetAgentArgument: skillActionTargetAgentProperty(),
		}, "required": []interface{}{"app"}},
		OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
			"setupRequest": map[string]interface{}{"type": "object"},
			"connected":    map[string]interface{}{"type": "object"},
			"refused":      map[string]interface{}{"type": "object"},
		}}}
}

type findAppsArguments struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

func (d *ActionDispatcher) findApps(ctx context.Context, input agentrun.ActionDispatchInput, run *models.AgentRun, umbrella models.ObjectiveOwner) (map[string]interface{}, error) {
	var a findAppsArguments
	if err := decodeSkillBindingArguments(input.Arguments, &a); err != nil {
		return nil, err
	}
	a.Query = strings.TrimSpace(a.Query)
	if a.Query == "" || len(a.Query) > maximumAppQueryBytes {
		return nil, errors.New("query must contain between 1 and 200 bytes")
	}
	if a.Limit <= 0 {
		a.Limit = defaultFindAppsLimit
	}
	a.Limit = min(a.Limit, maximumFindAppsLimit)
	skills, err := d.findSetupSkills(ctx, run, umbrella.ID, a.Query, a.Limit)
	if err != nil {
		return nil, err
	}
	var entries []AppCatalogEntry
	if catalog := d.apps.get(); catalog != nil {
		if entries, err = catalog.FindApps(ctx, run.Scope, a.Query, a.Limit); err != nil {
			return nil, err
		}
	}
	if len(entries) > a.Limit {
		entries = entries[:a.Limit]
	}
	set, err := d.runHostBindings(ctx, run, umbrella)
	if err != nil {
		return nil, err
	}
	apps := make([]interface{}, 0, len(entries))
	for _, entry := range entries {
		item := map[string]interface{}{"type": FindAppsTypeApp, "setup": FindAppsSetupApp, "slug": entry.Slug, "name": entry.Name,
			"official": entry.Official, "signIn": appSignIn(entry.SignIn), "connected": false}
		if summary := strings.TrimSpace(entry.Summary); summary != "" {
			item["summary"] = models.TruncateRunes(summary, 240)
		}
		if binding := ConnectedAppBinding(set, models.ConnectAppTarget{CatalogSlug: entry.Slug}); binding != nil {
			tools, toolErr := ConnectedAppToolNames(ctx, d.catalog, binding)
			if toolErr != nil {
				return nil, toolErr
			}
			item["connected"] = true
			item["tools"] = listedToolNames(tools)
		}
		apps = append(apps, item)
	}
	return map[string]interface{}{"apps": apps, "skills": skills}, nil
}

// findSetupSkills lists the Skills that discovery offers for the query and
// that the user can set up from chat, so one search covers apps and Skills.
func (d *ActionDispatcher) findSetupSkills(ctx context.Context, run *models.AgentRun, deploymentID, query string, limit int) ([]interface{}, error) {
	skills := []interface{}{}
	if d.discovery == nil {
		return skills, nil
	}
	request, err := skill.NormalizeDiscoveryRequest(skill.DiscoveryRequest{
		Scope: skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, DeploymentID: deploymentID, Query: query, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	page, err := d.discovery.DiscoverSkills(ctx, request)
	if err != nil {
		return nil, err
	}
	if page, err = skill.NormalizeDiscoveryPage(request, page); err != nil {
		return nil, err
	}
	for index := range page.Items {
		candidate := &page.Items[index]
		kind := skillSetupKindFor(candidate)
		if kind == "" {
			continue
		}
		item := map[string]interface{}{"type": FindAppsTypeSkill, "setup": FindAppsSetupSkill, "skillId": candidate.ID, "skillVersion": "latest",
			"name": candidate.Name, "setupKind": kind}
		if candidate.SourceIdentity != "" {
			item["sourceIdentity"] = candidate.SourceIdentity
		}
		if summary := strings.TrimSpace(candidate.Description); summary != "" {
			item["summary"] = models.TruncateRunes(summary, 240)
		}
		skills = append(skills, item)
	}
	return skills, nil
}

// skillSetupKindFor is the request_setup kind that sets a discovered Skill
// up for this Agent, or "" when the user cannot set it up from chat.
func skillSetupKindFor(candidate *skill.DiscoveryCandidate) string {
	switch {
	case candidate.Readiness == skill.DiscoveryReadinessNeedsInstallation:
		return "install"
	case candidate.Readiness == skill.DiscoveryReadinessBindable, skillCandidateNeedsConfigurationSetup(candidate):
		return "configure"
	}
	return ""
}

type requestConnectionArguments struct {
	App           interface{} `json:"app"`
	Reason        string      `json:"reason,omitempty"`
	TargetAgentID string      `json:"targetAgentId,omitempty"`
}

func decodeRequestConnectionArguments(arguments map[string]interface{}) (models.ConnectAppTarget, string, error) {
	var a requestConnectionArguments
	if err := decodeSkillBindingArguments(arguments, &a); err != nil {
		return models.ConnectAppTarget{}, "", refuseInteraction("the connection request arguments do not match the action schema")
	}
	reason := strings.TrimSpace(a.Reason)
	if utf8.RuneCountInString(reason) > 1000 {
		return models.ConnectAppTarget{}, "", refuseInteraction("reason must contain at most 1000 characters")
	}
	switch value := a.App.(type) {
	case string:
		slug := strings.TrimSpace(value)
		if strings.HasPrefix(strings.ToLower(slug), "https://") {
			return decodeAppAddress(slug, reason)
		}
		if !models.IsValidAppSlug(slug) {
			return models.ConnectAppTarget{}, "", refuseInteraction("app must be a slug copied from find_apps")
		}
		return models.ConnectAppTarget{CatalogSlug: slug}, reason, nil
	case map[string]interface{}:
		address, _ := value["address"].(string)
		if len(value) != 1 || address == "" {
			return models.ConnectAppTarget{}, "", refuseInteraction("app must be a catalog slug or {\"address\": \"https://...\"}")
		}
		return decodeAppAddress(address, reason)
	}
	return models.ConnectAppTarget{}, "", refuseInteraction("app must be a catalog slug or {\"address\": \"https://...\"}")
}

func decodeAppAddress(value, reason string) (models.ConnectAppTarget, string, error) {
	address, err := models.NormalizeAppAddress(value)
	if err != nil {
		return models.ConnectAppTarget{}, "", refuseInteraction(err.Error())
	}
	parsed, _ := url.Parse(address)
	return models.ConnectAppTarget{Address: address, Name: parsed.Hostname(), SignIn: models.AppSignInUnknown}, reason, nil
}

// requestConnection saves a connect card in the run's chat (under umbrella)
// for owner, the Agent or Team the app is connected for: the umbrella, or
// the Agent a builder names with targetAgentId.
func (d *ActionDispatcher) requestConnection(ctx context.Context, input agentrun.ActionDispatchInput, run *models.AgentRun, member string, umbrella, owner models.ObjectiveOwner) (map[string]interface{}, error) {
	target, reason, err := decodeRequestConnectionArguments(input.Arguments)
	if err != nil {
		return nil, err
	}
	if target.CatalogSlug != "" {
		catalog := d.apps.get()
		if catalog == nil {
			return nil, refuseInteraction("connecting apps from chat is unavailable on this host")
		}
		entry, getErr := catalog.GetApp(ctx, run.Scope, target.CatalogSlug)
		if getErr != nil {
			return nil, getErr
		}
		if entry == nil {
			return nil, refuseInteraction("no catalog app has the slug " + target.CatalogSlug + "; call find_apps and copy the slug of a result with type mcp-server. A Skill (type skill) is not connected: set it up with request_setup")
		}
		target = models.ConnectAppTarget{CatalogSlug: entry.Slug, Name: strings.TrimSpace(entry.Name), Official: entry.Official, SignIn: appSignIn(entry.SignIn), LogoSHA256: entry.LogoSHA256, LogoMonochrome: entry.LogoMonochrome}
		if target.Name == "" {
			target.Name = entry.Slug
		}
		if len(target.Name) > 256 {
			target.Name = models.TruncateRunes(target.Name, 64)
		}
		if !models.IsValidLogoDigest(target.LogoSHA256) {
			target.LogoSHA256, target.LogoMonochrome = "", false
		}
	}
	// An app the owner can already use needs no card: the model gets its tools.
	set, err := d.runHostBindings(ctx, run, owner)
	if err != nil {
		return nil, err
	}
	if binding := ConnectedAppBinding(set, target); binding != nil {
		tools, toolErr := ConnectedAppToolNames(ctx, d.catalog, binding)
		if toolErr != nil {
			return nil, toolErr
		}
		return map[string]interface{}{"connected": map[string]interface{}{
			"app": target.Name, "tools": listedToolNames(tools),
			"instruction": target.Name + " is already connected. Use its tools for this request; do not ask the user to connect it again.",
		}}, nil
	}
	// An app the umbrella uses per person is connected by the person asking;
	// a run with no person cannot ask anyone to.
	mode := ""
	if _, personal := set.PersonalApps[ConnectAppKey(target)]; personal {
		mode = models.SkillSetupModePerPerson
		if run.ActingUser == "" {
			return nil, refuseInteraction(target.Name + " uses each person's own account, and no person asked for this run. Ask someone who manages " + umbrellaLabel(owner) + " to share an account with it")
		}
	}
	conversationID, messageID, err := d.skillSetupConversationOrigin(ctx, run, member, umbrella)
	if err != nil {
		return nil, refuseInteraction("an app can only be connected from a conversation with the user; ask them to connect " + target.Name + " in Settings, Connected apps")
	}
	id := "connect-app:" + input.Call.ID
	if existing, getErr := d.store.GetSkillSetupRequest(ctx, run.Scope, id); getErr != nil {
		return nil, getErr
	} else if existing != nil {
		return SkillSetupRequestResult(existing)
	}
	// An unanswered card for the app is reused only within this run and for
	// the same person: a card resumes only the run that made it.
	current, err := d.store.ListSkillSetupRequests(ctx, run.Scope, owner.ID, conversationID)
	if err != nil {
		return nil, err
	}
	for _, r := range current {
		if r.Kind != models.SkillSetupKindConnectApp || r.App == nil || !sameConnectAppTarget(*r.App, target) || r.RunID != run.ID || r.RequestedFor != run.ActingUser {
			continue
		}
		if r.Status == "pending" {
			return SkillSetupRequestResult(r)
		}
		if r.Status == "dismissed" && r.RunID == run.ID {
			return nil, refuseInteraction("the user chose not to connect " + target.Name + " in this reply; do not ask again now")
		}
	}
	if reason == "" {
		reason = "Connect " + target.Name + " to continue in this chat."
	}
	now := time.Now().UTC()
	r := &models.SkillSetupRequest{ID: id, Scope: run.Scope, Owner: owner, DeploymentID: owner.ID, AgentID: member, ConversationID: conversationID, TriggerMessageID: messageID,
		RunID: run.ID, ActionCallID: input.Call.ID, Kind: models.SkillSetupKindConnectApp, SkillID: mcp.SkillID, SkillVersion: mcp.SkillVersion,
		SkillName: target.Name, App: &target, RequestedFor: run.ActingUser, Mode: mode, Reason: reason, Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := d.store.SaveSkillSetupRequest(ctx, r, 0); err != nil {
		if errors.Is(err, models.ErrSkillSetupConflict) {
			if replay, readErr := d.store.GetSkillSetupRequest(ctx, run.Scope, id); readErr == nil && replay != nil {
				return SkillSetupRequestResult(replay)
			}
		}
		return nil, err
	}
	return SkillSetupRequestResult(r)
}

// runHostBindings are the host bindings (connected apps) of owner as the
// run uses them: the run's umbrella, or the Agent a builder in its own chat
// acts for (skillActionTarget checked it), read as that Agent for the
// person the run acts for.
func (d *ActionDispatcher) runHostBindings(ctx context.Context, run *models.AgentRun, owner models.ObjectiveOwner) (*skill.HostBindingSet, error) {
	if run == nil {
		return &skill.HostBindingSet{}, nil
	}
	actor := models.RunSkillActor(run)
	if models.OwnerOfSkillUmbrella(actor.Umbrella) != owner {
		if owner.Type != models.OwnerTypeAgent || run.Owner != (models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: run.AssignedAgentID}) {
			return &skill.HostBindingSet{}, nil
		}
		actor = skill.RunActor{RunID: run.ID, AgentID: owner.ID, Umbrella: models.SkillUmbrella(owner), ActingUser: run.ActingUser}
	}
	return d.catalog.HostBindings(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, owner.ID, actor)
}

func appSignIn(value string) string {
	switch value {
	case models.AppSignInOAuth, models.AppSignInAPIKey, models.AppSignInNone:
		return value
	}
	return models.AppSignInUnknown
}

// ConnectAppKey is the host's key of an app: its catalog slug, or for an
// address "custom:" and the first 24 hex digits of the address's SHA-256.
func ConnectAppKey(target models.ConnectAppTarget) string {
	if target.CatalogSlug != "" {
		return target.CatalogSlug
	}
	sum := sha256.Sum256([]byte(target.Address))
	return "custom:" + hex.EncodeToString(sum[:])[:24]
}

func sameConnectAppTarget(left, right models.ConnectAppTarget) bool {
	if left.CatalogSlug != "" || right.CatalogSlug != "" {
		return strings.EqualFold(left.CatalogSlug, right.CatalogSlug)
	}
	return left.Address == right.Address
}

// ConnectedAppBinding returns the host binding (a connected app) in set that
// connects target: the connection made from that catalog entry, or to that
// address. Nil when none does.
func ConnectedAppBinding(set *skill.HostBindingSet, target models.ConnectAppTarget) *skill.Binding {
	if set == nil {
		return nil
	}
	for _, binding := range set.Bindings {
		if binding == nil || binding.SkillID != mcp.SkillID || binding.Disabled {
			continue
		}
		if target.CatalogSlug != "" {
			for _, service := range set.Services[binding.ID] {
				if strings.EqualFold(strings.TrimSpace(service), target.CatalogSlug) {
					return binding
				}
			}
			continue
		}
		endpoint, _ := binding.Config["endpoint"].(string)
		if normalized, err := models.NormalizeAppAddress(endpoint); err == nil && target.Address != "" && normalized == target.Address {
			return binding
		}
	}
	return nil
}

// ConnectedAppToolNames lists a connected app's model tool names.
func ConnectedAppToolNames(ctx context.Context, catalog *skill.Catalog, binding *skill.Binding) ([]string, error) {
	var definition *skill.Definition
	var err error
	if strings.TrimSpace(binding.SourceIdentity) == "" {
		definition, err = catalog.GetDefinition(ctx, binding.SkillID, binding.SkillVersion)
	} else {
		definition, err = catalog.GetDefinitionVariant(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
	}
	if err != nil {
		return nil, err
	}
	tools := make([]string, 0, len(binding.Tools))
	for _, tool := range binding.Tools {
		tools = append(tools, skill.BindingToolModelName(definition, binding, tool.Name))
	}
	sort.Strings(tools)
	return tools, nil
}

// listedToolNames bounds a tool list for a model-facing result.
func listedToolNames(tools []string) []interface{} {
	listed := make([]interface{}, 0, min(len(tools), maximumConnectedAppToolsNamed)+1)
	for index, tool := range tools {
		if index == maximumConnectedAppToolsNamed {
			listed = append(listed, fmt.Sprintf("and %d more", len(tools)-index))
			break
		}
		listed = append(listed, tool)
	}
	return listed
}

// PendingConnectAppRequestFromAction returns the request a succeeded
// request_connection call created for its own Run when it still waits for
// the user.
func PendingConnectAppRequestFromAction(ctx context.Context, store models.SkillSetupRequestStore, call *models.ActionCall) *models.SkillSetupRequest {
	if call == nil || store == nil || call.Status != models.ActionCallStatusSucceeded || call.SkillID != models.SkillManagementSkillID || call.Action != agentrun.SkillActionRequestConnection {
		return nil
	}
	id := agentrun.InteractionRequestID(call, "setupRequest")
	if id == "" {
		return nil
	}
	// Read the durable record: the user may have connected the app between
	// dispatch and this commit, and a settled request must not park the Run.
	current, err := store.GetSkillSetupRequest(ctx, call.Scope, id)
	if err != nil || current == nil || current.Kind != models.SkillSetupKindConnectApp || current.RunID != call.RunID || current.Status != "pending" {
		return nil
	}
	return current
}

// ConnectAppResolutionInstruction is the durable guidance given to a Run
// woken by its connect card. tools are the app's model tool names.
func ConnectAppResolutionInstruction(r *models.SkillSetupRequest, tools []string) string {
	name := r.SkillName
	if r.Status == "dismissed" {
		return "The user chose not to connect " + name + " now. Do not ask again in this reply; explain what remains blocked and offer an alternative."
	}
	message := "The user connected " + name + "."
	if len(tools) > 0 {
		named := make([]string, 0, len(tools))
		for _, tool := range listedToolNames(tools) {
			named = append(named, tool.(string))
		}
		message += " Its tools are now available: " + strings.Join(named, ", ") + "."
	}
	return message + " Continue the user's original request with them."
}
