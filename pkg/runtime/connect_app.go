package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// SkillActionFindApps searches the host's shared catalog of connectable apps
// (remote MCP servers). It is read-only and available to every Agent, with
// or without connections.
const SkillActionFindApps = "find_apps"

// SkillActionRequestConnection asks the user, through a durable in-chat
// card, to connect one catalog app (or an MCP server address). It pauses the
// requesting Run until the connection is active or the card is dismissed.
const SkillActionRequestConnection = "request_connection"

// SkillSetupKindConnectApp is the setup request kind of a connect card. It
// is stored with the other in-chat setup requests; its target is always the
// openseal.mcp Skill and its resolution names the connection's host binding.
const SkillSetupKindConnectApp = "connect_app"

// ConnectAppWakeType parks the requesting Run until the requested app is
// connected for it or the card is dismissed.
const ConnectAppWakeType = "connect_app"

const (
	maximumFindAppsLimit = 8
	defaultFindAppsLimit = 5
	maximumAppQueryBytes = 200
	maximumAppAddress    = 2048
)

// App sign-in kinds as the catalog describes an app's first endpoint.
const (
	AppSignInOAuth   = "oauth"
	AppSignInAPIKey  = "api-key"
	AppSignInNone    = "none"
	AppSignInUnknown = "unknown"
)

// ConnectAppTarget is the app a connect_app request asks for: a catalog
// entry (CatalogSlug) or an MCP server address the user gave (Address), with
// what the card shows about it.
type ConnectAppTarget struct {
	CatalogSlug    string `json:"catalogSlug,omitempty"`
	Address        string `json:"address,omitempty"`
	Name           string `json:"name"`
	Official       bool   `json:"official,omitempty"`
	SignIn         string `json:"signIn,omitempty"`
	LogoSHA256     string `json:"logoSha256,omitempty"`
	LogoMonochrome bool   `json:"logoMonochrome,omitempty"`
}

func (t *ConnectAppTarget) validate() error {
	if t == nil {
		return invalidSkillSetupField("app", "is required for connect_app")
	}
	if (t.CatalogSlug == "") == (t.Address == "") {
		return invalidSkillSetupField("app", "must name exactly one catalog app or address")
	}
	if t.CatalogSlug != "" && !validAppSlug(t.CatalogSlug) {
		return invalidSkillSetupField("app", "has an invalid catalog slug")
	}
	if t.Address != "" {
		if normalized, err := NormalizeAppAddress(t.Address); err != nil || normalized != t.Address {
			return invalidSkillSetupField("app", "has an invalid address")
		}
	}
	if strings.TrimSpace(t.Name) == "" || len(t.Name) > 256 {
		return invalidSkillSetupField("app", "needs a display name of at most 256 bytes")
	}
	switch t.SignIn {
	case "", AppSignInOAuth, AppSignInAPIKey, AppSignInNone, AppSignInUnknown:
	default:
		return invalidSkillSetupField("app", "has an unknown sign-in kind")
	}
	if t.LogoSHA256 != "" && !validLogoDigest(t.LogoSHA256) {
		return invalidSkillSetupField("app", "has an invalid logo digest")
	}
	return nil
}

// appSlugPattern is a catalog slug, which may contain "/" (for example
// neondatabase/mcp-server-neon).
var appSlugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

func validAppSlug(value string) bool {
	return appSlugPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.Contains(value, "//")
}

func validLogoDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// NormalizeAppAddress returns an MCP server address as connections store it:
// an https URL without credentials, query or fragment.
func NormalizeAppAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || len(value) > maximumAppAddress || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("the address must be an https URL without credentials or a query, such as https://mcp.example.com/mcp")
	}
	parsed.Scheme, parsed.Host = "https", strings.ToLower(parsed.Host)
	return parsed.String(), nil
}

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
	FindApps(ctx context.Context, scope Scope, query string, limit int) ([]AppCatalogEntry, error)
	// GetApp returns nil, nil when no entry has the slug.
	GetApp(ctx context.Context, scope Scope, slug string) (*AppCatalogEntry, error)
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
func (d *SkillBindingActionDispatcher) UseAppCatalog(ref *AppCatalogRef) {
	if d != nil {
		d.apps = ref
	}
}

const connectAppGuidance = "Use this when the user wants to connect or use a service and no connected-app tools (mcp.<app>.*) cover it."

func findAppsAction() skill.Action {
	return skill.Action{Name: SkillActionFindApps,
		Description: "Search what can be connected for every Agent: catalog apps (remote MCP servers) and Skills. " + connectAppGuidance + " Query with the service name, for example Linear. Apps list slug, name, whether the vendor maintains it (official), how it signs in, and whether it is already connected for this chat (then its tools are listed); call request_connection with the slug. Prefer an app that covers the service. When no app does, use a matching Skill: call request_setup with its skillId, skillVersion and sourceIdentity, and its setupKind as kind.",
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
	return skill.Action{Name: SkillActionRequestConnection,
		Description: "Ask the user to connect an app through a Connect card in this chat. " + connectAppGuidance + " Pass app as the exact slug from find_apps, or as {\"address\": \"https://...\"} when the user gave an MCP server address. The user signs in on the card; never ask for keys or passwords in chat. A pending result pauses this work until the app is connected (you then continue with its new tools) or the card is dismissed. If the app is already connected, the result lists its tools instead: use them. If the result is refused, explain the reason and do not repeat the request in this reply.",
		Risk:        skill.RiskLevelRead, SideEffect: skill.SideEffectNone, Idempotency: skill.IdempotencySupported, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
			"app": map[string]interface{}{"anyOf": []interface{}{
				map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128, "description": "Catalog slug copied from find_apps."},
				map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
					"address": map[string]interface{}{"type": "string", "minLength": 9, "maxLength": maximumAppAddress, "description": "The MCP server's https address."},
				}, "required": []interface{}{"address"}},
			}},
			"reason": map[string]interface{}{"type": "string", "maxLength": 1000, "description": "Optional; shown on the card, for example \"Connect Linear so I can list your open issues\"."},
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

func (d *SkillBindingActionDispatcher) findApps(ctx context.Context, input ActionDispatchInput, run *AgentRun, deploymentID string) (map[string]interface{}, error) {
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
	skills, err := d.findSetupSkills(ctx, run, deploymentID, a.Query, a.Limit)
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
	set, err := d.runHostBindings(ctx, run, deploymentID)
	if err != nil {
		return nil, err
	}
	apps := make([]interface{}, 0, len(entries))
	for _, entry := range entries {
		item := map[string]interface{}{"slug": entry.Slug, "name": entry.Name, "official": entry.Official, "signIn": appSignIn(entry.SignIn), "connected": false}
		if summary := strings.TrimSpace(entry.Summary); summary != "" {
			item["summary"] = truncateRunes(summary, 240)
		}
		if binding := ConnectedAppBinding(set, ConnectAppTarget{CatalogSlug: entry.Slug}); binding != nil {
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
func (d *SkillBindingActionDispatcher) findSetupSkills(ctx context.Context, run *AgentRun, deploymentID, query string, limit int) ([]interface{}, error) {
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
		item := map[string]interface{}{"skillId": candidate.ID, "skillVersion": "latest", "name": candidate.Name, "setupKind": kind}
		if candidate.SourceIdentity != "" {
			item["sourceIdentity"] = candidate.SourceIdentity
		}
		if summary := strings.TrimSpace(candidate.Description); summary != "" {
			item["summary"] = truncateRunes(summary, 240)
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
	App    interface{} `json:"app"`
	Reason string      `json:"reason,omitempty"`
}

func decodeRequestConnectionArguments(arguments map[string]interface{}) (ConnectAppTarget, string, error) {
	var a requestConnectionArguments
	if err := decodeSkillBindingArguments(arguments, &a); err != nil {
		return ConnectAppTarget{}, "", refuseInteraction("the connection request arguments do not match the action schema")
	}
	reason := strings.TrimSpace(a.Reason)
	if utf8.RuneCountInString(reason) > 1000 {
		return ConnectAppTarget{}, "", refuseInteraction("reason must contain at most 1000 characters")
	}
	switch value := a.App.(type) {
	case string:
		slug := strings.TrimSpace(value)
		if strings.HasPrefix(strings.ToLower(slug), "https://") {
			return decodeAppAddress(slug, reason)
		}
		if !validAppSlug(slug) {
			return ConnectAppTarget{}, "", refuseInteraction("app must be a slug copied from find_apps")
		}
		return ConnectAppTarget{CatalogSlug: slug}, reason, nil
	case map[string]interface{}:
		address, _ := value["address"].(string)
		if len(value) != 1 || address == "" {
			return ConnectAppTarget{}, "", refuseInteraction("app must be a catalog slug or {\"address\": \"https://...\"}")
		}
		return decodeAppAddress(address, reason)
	}
	return ConnectAppTarget{}, "", refuseInteraction("app must be a catalog slug or {\"address\": \"https://...\"}")
}

func decodeAppAddress(value, reason string) (ConnectAppTarget, string, error) {
	address, err := NormalizeAppAddress(value)
	if err != nil {
		return ConnectAppTarget{}, "", refuseInteraction(err.Error())
	}
	parsed, _ := url.Parse(address)
	return ConnectAppTarget{Address: address, Name: parsed.Hostname(), SignIn: AppSignInUnknown}, reason, nil
}

func (d *SkillBindingActionDispatcher) requestConnection(ctx context.Context, input ActionDispatchInput, run *AgentRun, deploymentID string) (map[string]interface{}, error) {
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
			return nil, refuseInteraction("no catalog app has the slug " + target.CatalogSlug + "; call find_apps and copy a returned slug")
		}
		target = ConnectAppTarget{CatalogSlug: entry.Slug, Name: strings.TrimSpace(entry.Name), Official: entry.Official, SignIn: appSignIn(entry.SignIn), LogoSHA256: entry.LogoSHA256, LogoMonochrome: entry.LogoMonochrome}
		if target.Name == "" {
			target.Name = entry.Slug
		}
		if len(target.Name) > 256 {
			target.Name = truncateRunes(target.Name, 64)
		}
		if !validLogoDigest(target.LogoSHA256) {
			target.LogoSHA256, target.LogoMonochrome = "", false
		}
	}
	// An app this run can already use needs no card: the model gets its tools.
	set, err := d.runHostBindings(ctx, run, deploymentID)
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
	conversationID, messageID, err := d.skillSetupConversationOrigin(ctx, run, deploymentID)
	if err != nil {
		return nil, refuseInteraction("an app can only be connected from a conversation with the user; ask them to connect " + target.Name + " in Settings, Connected apps")
	}
	id := "connect-app:" + input.Call.ID
	if existing, getErr := d.store.GetSkillSetupRequest(ctx, run.Scope, id); getErr != nil {
		return nil, getErr
	} else if existing != nil {
		return skillSetupResult(existing)
	}
	// One card per app in a conversation: an unanswered one is reused.
	current, err := d.store.ListSkillSetupRequests(ctx, run.Scope, deploymentID, conversationID)
	if err != nil {
		return nil, err
	}
	for _, r := range current {
		if r.Kind != SkillSetupKindConnectApp || r.App == nil || !sameConnectAppTarget(*r.App, target) {
			continue
		}
		if r.Status == "pending" {
			return skillSetupResult(r)
		}
		if r.Status == "dismissed" && r.RunID == run.ID {
			return nil, refuseInteraction("the user chose not to connect " + target.Name + " in this reply; do not ask again now")
		}
	}
	if reason == "" {
		reason = "Connect " + target.Name + " to continue in this chat."
	}
	now := time.Now().UTC()
	r := &SkillSetupRequest{ID: id, Scope: run.Scope, DeploymentID: deploymentID, ConversationID: conversationID, TriggerMessageID: messageID,
		RunID: run.ID, ActionCallID: input.Call.ID, Kind: SkillSetupKindConnectApp, SkillID: mcp.SkillID, SkillVersion: mcp.SkillVersion,
		SkillName: target.Name, App: &target, Reason: reason, Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := d.store.SaveSkillSetupRequest(ctx, r, 0); err != nil {
		if errors.Is(err, ErrSkillSetupConflict) {
			if replay, readErr := d.store.GetSkillSetupRequest(ctx, run.Scope, id); readErr == nil && replay != nil {
				return skillSetupResult(replay)
			}
		}
		return nil, err
	}
	return skillSetupResult(r)
}

func (d *SkillBindingActionDispatcher) runHostBindings(ctx context.Context, run *AgentRun, deploymentID string) (*skill.HostBindingSet, error) {
	if run == nil || run.AssignedAgentID != deploymentID {
		return &skill.HostBindingSet{}, nil
	}
	return d.catalog.HostBindings(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, deploymentID, skill.RunActor{RunID: run.ID, AgentID: deploymentID})
}

func appSignIn(value string) string {
	switch value {
	case AppSignInOAuth, AppSignInAPIKey, AppSignInNone:
		return value
	}
	return AppSignInUnknown
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit-1]) + "…"
}

func sameConnectAppTarget(left, right ConnectAppTarget) bool {
	if left.CatalogSlug != "" || right.CatalogSlug != "" {
		return strings.EqualFold(left.CatalogSlug, right.CatalogSlug)
	}
	return left.Address == right.Address
}

// ConnectedAppBinding returns the host binding (a connected app) in set that
// connects target: the connection made from that catalog entry, or to that
// address. Nil when none does.
func ConnectedAppBinding(set *skill.HostBindingSet, target ConnectAppTarget) *skill.Binding {
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
		if normalized, err := NormalizeAppAddress(endpoint); err == nil && target.Address != "" && normalized == target.Address {
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

// pendingConnectAppRequestFromAction returns the request a succeeded
// request_connection call created for its own Run when it still waits for
// the user.
func pendingConnectAppRequestFromAction(ctx context.Context, store SkillSetupRequestStore, call *ActionCall) *SkillSetupRequest {
	if call == nil || store == nil || call.Status != ActionCallStatusSucceeded || call.SkillID != SkillManagementSkillID || call.Action != SkillActionRequestConnection {
		return nil
	}
	id := interactionRequestID(call, "setupRequest")
	if id == "" {
		return nil
	}
	// Read the durable record: the user may have connected the app between
	// dispatch and this commit, and a settled request must not park the Run.
	current, err := store.GetSkillSetupRequest(ctx, call.Scope, id)
	if err != nil || current == nil || current.Kind != SkillSetupKindConnectApp || current.RunID != call.RunID || current.Status != "pending" {
		return nil
	}
	return current
}

// IsExactConnectAppWait reports whether the Run waits for this request.
func IsExactConnectAppWait(run *AgentRun, requestID string) bool {
	return run != nil && run.Status == AgentRunStatusWaitingForEvent && run.WakeCondition != nil &&
		run.WakeCondition.Type == ConnectAppWakeType && run.WakeCondition.Reference == requestID
}

// ConnectAppResolutionInstruction is the durable guidance given to a Run
// woken by its connect card. tools are the app's model tool names.
func ConnectAppResolutionInstruction(r *SkillSetupRequest, tools []string) string {
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
