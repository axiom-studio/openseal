package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	opensealprocess "github.com/axiom-studio/openseal/pkg/process"
)

// HostCapabilityState is a non-secret declaration of the environment in which
// a deployment's skill snapshot will run. Environment contains names only;
// secret values are resolved later by a bounded action worker.
type HostCapabilityState struct {
	OperatingSystem string                 `json:"operatingSystem,omitempty"`
	Executables     map[string]bool        `json:"executables,omitempty"`
	Environment     map[string]bool        `json:"environment,omitempty"`
	Configuration   map[string]interface{} `json:"configuration,omitempty"`
	// ResourceRoots uses binding IDs so two source variants can be staged
	// independently without exposing provenance in the model-facing snapshot.
	// A declared skillId@version key remains supported for single-source hosts.
	ResourceRoots   map[string]string            `json:"resourceRoots,omitempty"`
	Adapters        map[string]AdapterCapability `json:"adapters,omitempty"`
	Revision        string                       `json:"revision,omitempty"`
	ResourceStager  ResourceStager               `json:"-"`
	Architecture    string                       `json:"architecture,omitempty"`
	RuntimePreparer RuntimePreparer              `json:"-"`
}

type AdapterState string

const (
	AdapterStateAvailable   AdapterState = "available"
	AdapterStateUnavailable AdapterState = "unavailable"
	AdapterStateDegraded    AdapterState = "degraded"

	AdapterLocal           = "local"
	AdapterGit             = "git"
	AdapterPlugin          = "plugin"
	AdapterInstaller       = "installer"
	AdapterWatcher         = "watcher"
	AdapterRemoteNode      = "remote-node"
	AdapterResourceStaging = "resource-staging"
	AdapterPreparedRuntime = "prepared-runtime"
	AdapterHTTPAction      = "openseal.http.request"
)

type AdapterCapability struct {
	State    AdapterState `json:"state"`
	Version  string       `json:"version,omitempty"`
	Features []string     `json:"features,omitempty"`
	Reason   string       `json:"reason,omitempty"`
}

type AvailabilityReason struct {
	Code        string `json:"code"`
	Requirement string `json:"requirement,omitempty"`
	Message     string `json:"message"`
}

type ActivatedSkill struct {
	BindingID        string                 `json:"bindingId"`
	BindingRevision  int64                  `json:"bindingRevision"`
	SkillID          string                 `json:"skillId"`
	SkillVersion     string                 `json:"skillVersion"`
	SourceIdentity   string                 `json:"sourceIdentity,omitempty"`
	Name             string                 `json:"name"`
	Description      string                 `json:"description,omitempty"`
	SourceDigest     string                 `json:"sourceDigest,omitempty"`
	ConfigurationKey string                 `json:"configurationKey,omitempty"`
	Configuration    map[string]interface{} `json:"configuration,omitempty"`
	ResourceRoot     string                 `json:"resourceRoot,omitempty"`
	ResourceRevision string                 `json:"resourceRevision,omitempty"`
	ResourceAdapter  string                 `json:"resourceAdapter,omitempty"`
	PreparedRuntime  *PreparedRuntime       `json:"preparedRuntime,omitempty"`
	Prompt           *PromptModule          `json:"prompt,omitempty"`
	Actions          []ModelAction          `json:"actions,omitempty"`
}

type UnavailableSkill struct {
	BindingID      string               `json:"bindingId"`
	SkillID        string               `json:"skillId"`
	SkillVersion   string               `json:"skillVersion"`
	SourceIdentity string               `json:"sourceIdentity,omitempty"`
	Reasons        []AvailabilityReason `json:"reasons"`
}

// ActivationSnapshot is the immutable model-visible skill surface for one
// deployment turn/session. SnapshotID excludes CreatedAt and is stable for
// equivalent bindings and declared host capabilities across process restarts.
type ActivationSnapshot struct {
	SnapshotID   string                       `json:"snapshotId"`
	Scope        ScopeReference               `json:"scope"`
	DeploymentID string                       `json:"deploymentId"`
	HostRevision string                       `json:"hostRevision,omitempty"`
	Adapters     map[string]AdapterCapability `json:"adapters,omitempty"`
	Skills       []ActivatedSkill             `json:"skills"`
	Unavailable  []UnavailableSkill           `json:"unavailable,omitempty"`
	CreatedAt    time.Time                    `json:"createdAt"`
}

func (c *Catalog) Activate(ctx context.Context, scope ScopeReference, deploymentID string, host HostCapabilityState) (*ActivationSnapshot, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	adapters, err := normalizeAdapterCapabilities(host.Adapters)
	if err != nil {
		return nil, err
	}
	host.Adapters = adapters

	bindings, err := c.bindingsFor(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	definitions := make(map[string]*Definition)
	for _, binding := range bindings {
		if binding.Disabled {
			continue
		}
		definition, err := c.definitionFor(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
		if err != nil {
			return nil, err
		}
		if definition != nil {
			definitions[definitionKey(binding.SkillID, binding.SkillVersion, binding.SourceIdentity)] = definition
		}
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].ID < bindings[j].ID })
	activeBindings := bindings[:0]
	for _, binding := range bindings {
		if !binding.Disabled {
			activeBindings = append(activeBindings, binding)
		}
	}
	bindings = activeBindings

	snapshot := &ActivationSnapshot{
		Scope: scope, DeploymentID: deploymentID, HostRevision: strings.TrimSpace(host.Revision),
		Adapters: adapters, Skills: make([]ActivatedSkill, 0, len(bindings)), CreatedAt: time.Now().UTC(),
	}
	for _, binding := range bindings {
		definition := definitions[definitionKey(binding.SkillID, binding.SkillVersion, binding.SourceIdentity)]
		if definition == nil {
			snapshot.Unavailable = append(snapshot.Unavailable, UnavailableSkill{
				BindingID: binding.ID, SkillID: binding.SkillID, SkillVersion: binding.SkillVersion, SourceIdentity: binding.SourceIdentity,
				Reasons: []AvailabilityReason{{Code: "definition_missing", Message: "bound skill definition is unavailable"}},
			})
			continue
		}
		reasons := evaluateAvailability(definition, binding, host)
		resourcesRequired := bindingRequiresStagedResources(definition, binding)
		resourceRoot := strings.TrimSpace(host.ResourceRoots[binding.ID])
		if resourceRoot == "" {
			resourceRoot = strings.TrimSpace(host.ResourceRoots[definition.ID+"@"+definition.Version])
		}
		resourceRevision := ""
		resourceAdapter := ""
		var preparedRuntime *PreparedRuntime
		if resourceRoot == "" && len(reasons) == 0 && resourcesRequired && host.ResourceStager == nil {
			reasons = append(reasons, AvailabilityReason{Code: "resource_staging_unavailable", Requirement: AdapterResourceStaging, Message: "declared skill resources require a configured host staging adapter"})
		} else if resourceRoot == "" && len(reasons) == 0 && resourcesRequired && !adapterAvailable(adapters, AdapterResourceStaging) {
			reasons = append(reasons, AvailabilityReason{Code: "resource_staging_unavailable", Requirement: AdapterResourceStaging, Message: "the configured resource stager is not advertised as available by this host"})
		} else if resourceRoot == "" && len(reasons) == 0 && resourcesRequired {
			stage, stageErr := host.ResourceStager.StageResources(ctx, ResourceStageRequest{
				Scope: scope, DeploymentID: deploymentID, BindingID: binding.ID,
				SkillID: definition.ID, SkillVersion: definition.Version,
				SourceDigest: definitionSourceDigest(definition), Resources: append([]capability.Resource(nil), definition.Resources...),
			})
			if stageErr != nil || stage == nil || !validResourceRoot(host.OperatingSystem, strings.TrimSpace(stage.Root)) ||
				strings.TrimSpace(stage.Revision) == "" || strings.TrimSpace(stage.Adapter) == "" {
				reasons = append(reasons, AvailabilityReason{Code: "resource_staging_failed", Requirement: definition.ID, Message: "declared skill resources could not be materialized by the configured host adapter"})
			} else {
				resourceRoot = strings.TrimSpace(stage.Root)
				resourceRevision = strings.TrimSpace(stage.Revision)
				resourceAdapter = strings.TrimSpace(stage.Adapter)
			}
		}
		prompt := (*PromptModule)(nil)
		if binding.EnablePrompt && definition.Prompt != nil {
			prompt = cloneDefinition(definition).Prompt
			prompt.Credentials = nil
			if strings.Contains(prompt.Instructions, "{baseDir}") {
				if !validResourceRoot(host.OperatingSystem, resourceRoot) {
					reasons = append(reasons, AvailabilityReason{Code: "resource_root_missing", Requirement: "{baseDir}", Message: "skill instructions require a trusted absolute resource root"})
				} else {
					prompt.Instructions = strings.ReplaceAll(prompt.Instructions, "{baseDir}", resourceRoot)
				}
			}
		}
		if len(reasons) > 0 {
			snapshot.Unavailable = append(snapshot.Unavailable, UnavailableSkill{
				BindingID: binding.ID, SkillID: binding.SkillID, SkillVersion: binding.SkillVersion, SourceIdentity: binding.SourceIdentity, Reasons: reasons,
			})
			continue
		}
		if adapterAvailable(adapters, AdapterPreparedRuntime) {
			if host.RuntimePreparer == nil {
				reasons = append(reasons, AvailabilityReason{Code: "runtime_preparation_unavailable", Requirement: AdapterPreparedRuntime, Message: "the host advertises prepared runtimes without a configured preparation adapter"})
			} else {
				request, requestErr := runtimePreparationRequest(scope, deploymentID, binding, definition, host)
				if requestErr != nil {
					reasons = append(reasons, AvailabilityReason{Code: "runtime_preparation_invalid", Requirement: AdapterPreparedRuntime, Message: "the Skill runtime requirements cannot be prepared safely"})
				} else if request != nil {
					result, prepareErr := host.RuntimePreparer.PrepareRuntime(ctx, *request)
					switch {
					case prepareErr != nil || result == nil:
						reasons = append(reasons, AvailabilityReason{Code: "runtime_preparation_failed", Requirement: request.PreparationID, Message: "the governed host could not reconcile the Skill runtime"})
					case result.State == RuntimePreparationPreparing:
						reasons = append(reasons, AvailabilityReason{Code: "runtime_preparing", Requirement: request.PreparationID, Message: "the governed Skill runtime is still being prepared"})
					case result.State == RuntimePreparationUnavailable:
						reasons = append(reasons, AvailabilityReason{Code: "runtime_preparation_unavailable", Requirement: request.PreparationID, Message: "the governed Skill runtime is unavailable on this host"})
					case result.State != RuntimePreparationReady || validatePreparedRuntime(*request, result.Runtime) != nil:
						reasons = append(reasons, AvailabilityReason{Code: "runtime_preparation_invalid", Requirement: request.PreparationID, Message: "the host returned an invalid prepared runtime identity"})
					default:
						preparedRuntime = result.Runtime
					}
				}
			}
		}
		if len(reasons) > 0 {
			snapshot.Unavailable = append(snapshot.Unavailable, UnavailableSkill{
				BindingID: binding.ID, SkillID: binding.SkillID, SkillVersion: binding.SkillVersion, SourceIdentity: binding.SourceIdentity, Reasons: reasons,
			})
			continue
		}
		actions := make([]ModelAction, 0, len(binding.AllowedActions))
		for _, name := range binding.AllowedActions {
			action := definition.Actions[name]
			if action.Transport != nil && action.Transport.Endpoint == opensealprocess.TransportName && !processActionAvailable(definition, action, host) {
				continue
			}
			actions = append(actions, ModelAction{
				Name: definition.ID + "." + name, Description: action.Description,
				BindingID: binding.ID, BindingRevision: binding.Revision, SkillID: definition.ID,
				Version: definition.Version, Action: name, InputSchema: cloneMap(action.InputSchema),
				SemanticArguments: cloneStringMap(action.SemanticArguments), Risk: action.Risk, SideEffect: action.SideEffect,
			})
		}
		sort.Slice(actions, func(i, j int) bool { return actions[i].Name < actions[j].Name })
		digest := ""
		if definition.Source != nil {
			digest = definition.Source.Digest
		}
		snapshot.Skills = append(snapshot.Skills, ActivatedSkill{
			BindingID: binding.ID, BindingRevision: binding.Revision, SkillID: definition.ID, SkillVersion: definition.Version,
			SourceIdentity: binding.SourceIdentity,
			Name:           definition.Name, Description: definition.Description,
			SourceDigest: digest, ConfigurationKey: definition.ConfigurationKey, Configuration: cloneMap(binding.Config),
			ResourceRoot: resourceRoot, ResourceRevision: resourceRevision, ResourceAdapter: resourceAdapter,
			PreparedRuntime: preparedRuntime,
			Prompt:          prompt, Actions: actions,
		})
	}
	sort.Slice(snapshot.Unavailable, func(i, j int) bool { return snapshot.Unavailable[i].BindingID < snapshot.Unavailable[j].BindingID })
	snapshot.SnapshotID = activationSnapshotDigest(snapshot)
	return snapshot, nil
}

func bindingRequiresStagedResources(definition *Definition, binding *Binding) bool {
	if definition == nil || binding == nil || len(definition.Resources) == 0 {
		return false
	}
	search := make([]string, 0, len(definition.Actions)+1)
	if binding.EnablePrompt && definition.Prompt != nil {
		search = append(search, definition.Prompt.Instructions)
	}
	for _, actionName := range binding.AllowedActions {
		action, ok := definition.Actions[actionName]
		if !ok || action.Transport == nil {
			continue
		}
		search = append(search, action.Transport.Endpoint)
		for _, argument := range action.Transport.Arguments {
			literal, err := json.Marshal(argument.Literal)
			if err == nil {
				search = append(search, string(literal))
			}
		}
	}
	for _, value := range search {
		if strings.Contains(value, "{baseDir}") {
			return true
		}
		for _, resource := range definition.Resources {
			path := strings.TrimSpace(strings.ReplaceAll(resource.Path, "\\", "/"))
			if path != "" && strings.Contains(value, path) {
				return true
			}
		}
	}
	return false
}

func definitionSourceDigest(definition *Definition) string {
	if definition == nil || definition.Source == nil {
		return ""
	}
	return strings.TrimSpace(definition.Source.Digest)
}

func evaluateAvailability(definition *Definition, binding *Binding, host HostCapabilityState) []AvailabilityReason {
	reasons := make([]AvailabilityReason, 0)
	if binding.EnablePrompt && NeedsActionAdapter(definition) {
		return []AvailabilityReason{{
			Code: "needs_action_adapter", Requirement: definition.ID,
			Message: "this imported Skill requires external access but defines no governed action; install or configure an action adapter before activation",
		}}
	}
	if binding.EnablePrompt && definition.Prompt != nil {
		for _, requirement := range definition.Prompt.Credentials {
			ref, ok := binding.Credentials[requirement.Name]
			if requirement.Optional && !ok {
				continue
			}
			if !ok || strings.TrimSpace(ref.ID) == "" || ref.Kind != requirement.Kind {
				reasons = append(reasons, AvailabilityReason{Code: "credential_missing", Requirement: requirement.Kind, Message: "required prompt credential is unavailable"})
			}
		}
	}
	if definition.Requirements.AlwaysAvailable {
		return reasons
	}
	if binding.EnablePrompt && definitionHasProcessActions(definition) {
		if !bindingHasProcessAction(definition, binding) {
			reasons = append(reasons, AvailabilityReason{Code: "process_action_unbound", Requirement: opensealprocess.TransportName, Message: "skill instructions require an explicitly authorized governed process action"})
		} else if !bindingAllowsProcessAction(definition, binding, host) {
			reasons = append(reasons, AvailabilityReason{Code: "process_action_unavailable", Requirement: opensealprocess.TransportName, Message: "authorized process actions are unavailable on this host"})
		}
	}
	if binding.EnablePrompt && definitionHasTransport(definition, AdapterHTTPAction) && !bindingUsesTransport(definition, binding, AdapterHTTPAction) {
		reasons = append(reasons, AvailabilityReason{
			Code: "action_adapter_unbound", Requirement: AdapterHTTPAction,
			Message: "skill instructions require an explicitly authorized governed HTTP action",
		})
	} else if bindingUsesTransport(definition, binding, AdapterHTTPAction) && !adapterAvailable(host.Adapters, AdapterHTTPAction) {
		reasons = append(reasons, AvailabilityReason{
			Code: "action_adapter_unavailable", Requirement: AdapterHTTPAction,
			Message: "the governed HTTP action adapter is unavailable on this host",
		})
	}
	if len(definition.Requirements.OperatingSystems) > 0 && !containsOperatingSystem(definition.Requirements.OperatingSystems, host.OperatingSystem) {
		reasons = append(reasons, AvailabilityReason{Code: "operating_system_unavailable", Requirement: strings.TrimSpace(host.OperatingSystem), Message: "host operating system is not supported"})
	}
	for _, executable := range definition.Requirements.Executables {
		if !hostProvidesExecutable(definition, executable, host) {
			reasons = append(reasons, AvailabilityReason{Code: "executable_missing", Requirement: executable, Message: "required executable is unavailable"})
		}
	}
	if len(definition.Requirements.AnyExecutables) > 0 {
		available := false
		for _, executable := range definition.Requirements.AnyExecutables {
			available = available || hostProvidesExecutable(definition, executable, host)
		}
		if !available {
			reasons = append(reasons, AvailabilityReason{Code: "any_executable_missing", Requirement: strings.Join(definition.Requirements.AnyExecutables, ","), Message: "none of the alternative executables are available"})
		}
	}
	for _, environment := range definition.Requirements.Environment {
		credential := binding.Credentials[environment]
		if !host.Environment[environment] && strings.TrimSpace(credential.ID) == "" {
			reasons = append(reasons, AvailabilityReason{Code: "environment_missing", Requirement: environment, Message: "required environment credential is unavailable"})
		}
	}
	for _, key := range definition.Requirements.Configuration {
		if value, ok := lookupActivationConfiguration(host.Configuration, key); !ok || !activationTruthy(value) {
			reasons = append(reasons, AvailabilityReason{Code: "configuration_missing", Requirement: key, Message: "required configuration is missing or disabled"})
		}
	}
	return reasons
}

func bindingUsesTransport(definition *Definition, binding *Binding, endpoint string) bool {
	if definition == nil || binding == nil {
		return false
	}
	for _, name := range binding.AllowedActions {
		action, ok := definition.Actions[name]
		if ok && action.Transport != nil && action.Transport.Kind == "tool" && action.Transport.Endpoint == endpoint {
			return true
		}
	}
	return false
}

func definitionHasTransport(definition *Definition, endpoint string) bool {
	if definition == nil {
		return false
	}
	for _, action := range definition.Actions {
		if action.Transport != nil && action.Transport.Kind == "tool" && action.Transport.Endpoint == endpoint {
			return true
		}
	}
	return false
}

func definitionHasProcessActions(definition *Definition) bool {
	if definition == nil {
		return false
	}
	for _, action := range definition.Actions {
		if action.Transport != nil && action.Transport.Kind == "tool" && action.Transport.Endpoint == opensealprocess.TransportName {
			return true
		}
	}
	return false
}

func bindingHasProcessAction(definition *Definition, binding *Binding) bool {
	if definition == nil || binding == nil {
		return false
	}
	for _, name := range binding.AllowedActions {
		action, ok := definition.Actions[name]
		if ok && action.Transport != nil && action.Transport.Kind == "tool" && action.Transport.Endpoint == opensealprocess.TransportName {
			return true
		}
	}
	return false
}

func bindingAllowsProcessAction(definition *Definition, binding *Binding, host HostCapabilityState) bool {
	if definition == nil || binding == nil {
		return false
	}
	for _, name := range binding.AllowedActions {
		action, ok := definition.Actions[name]
		if ok && processActionAvailable(definition, action, host) {
			return true
		}
	}
	return false
}

func processActionAvailable(definition *Definition, action Action, host HostCapabilityState) bool {
	if action.Transport == nil || action.Transport.Kind != "tool" || action.Transport.Endpoint != opensealprocess.TransportName {
		return false
	}
	executable, _ := action.Transport.Arguments[opensealprocess.ExecutableKey].Literal.(string)
	return hostProvidesExecutable(definition, executable, host)
}

func hostProvidesExecutable(definition *Definition, executable string, host HostCapabilityState) bool {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return false
	}
	if host.Executables[executable] {
		return true
	}
	adapter, ok := host.Adapters[AdapterInstaller]
	if !ok || adapter.State != AdapterStateAvailable {
		return false
	}
	for _, installer := range definition.Installers {
		if !containsString(installer.Executables, executable) || !installerSupportsOperatingSystem(installer, host.OperatingSystem) ||
			!containsString(adapter.Features, strings.ToLower(strings.TrimSpace(installer.Kind))) {
			continue
		}
		return true
	}
	return false
}

func installerSupportsOperatingSystem(installer capability.Installer, operatingSystem string) bool {
	if len(installer.OperatingSystems) == 0 {
		return true
	}
	return containsOperatingSystem(installer.OperatingSystems, operatingSystem)
}

func containsOperatingSystem(supported []string, current string) bool {
	current = normalizeOperatingSystem(current)
	for _, candidate := range supported {
		if normalizeOperatingSystem(candidate) == current && current != "" {
			return true
		}
	}
	return false
}

func normalizeOperatingSystem(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "win32", "windows":
		return "windows"
	case "macos", "osx", "darwin":
		return "darwin"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func validResourceRoot(operatingSystem, value string) bool {
	if value == "" || strings.ContainsRune(value, '\x00') {
		return false
	}
	if normalizeOperatingSystem(operatingSystem) == "windows" {
		return len(value) >= 3 && ((value[1] == ':' && (value[2] == '\\' || value[2] == '/')) || strings.HasPrefix(value, `\\`))
	}
	return filepath.IsAbs(value) && filepath.Clean(value) != string(filepath.Separator)
}

func lookupActivationConfiguration(configuration map[string]interface{}, key string) (interface{}, bool) {
	var current interface{} = configuration
	for _, part := range strings.Split(key, ".") {
		values, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = values[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func activationTruthy(value interface{}) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return strings.TrimSpace(typed) != ""
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	default:
		return true
	}
}

func activationSnapshotDigest(snapshot *ActivationSnapshot) string {
	payload := struct {
		Scope        ScopeReference               `json:"scope"`
		DeploymentID string                       `json:"deploymentId"`
		HostRevision string                       `json:"hostRevision,omitempty"`
		Adapters     map[string]AdapterCapability `json:"adapters,omitempty"`
		Skills       []ActivatedSkill             `json:"skills"`
		Unavailable  []UnavailableSkill           `json:"unavailable,omitempty"`
	}{snapshot.Scope, snapshot.DeploymentID, snapshot.HostRevision, snapshot.Adapters, snapshot.Skills, snapshot.Unavailable}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func normalizeAdapterCapabilities(value map[string]AdapterCapability) (map[string]AdapterCapability, error) {
	if value == nil {
		return nil, nil
	}
	result := make(map[string]AdapterCapability, len(value))
	for key, capability := range value {
		key = strings.TrimSpace(key)
		capability.Version = strings.TrimSpace(capability.Version)
		capability.Reason = strings.TrimSpace(capability.Reason)
		features := make([]string, 0, len(capability.Features))
		seenFeatures := make(map[string]bool, len(capability.Features))
		for _, feature := range capability.Features {
			feature = strings.ToLower(strings.TrimSpace(feature))
			if feature == "" || strings.ContainsAny(feature, "\x00\r\n") {
				return nil, fmt.Errorf("host adapter %q has an invalid feature", key)
			}
			if !seenFeatures[feature] {
				features = append(features, feature)
				seenFeatures[feature] = true
			}
		}
		sort.Strings(features)
		capability.Features = features
		if key == "" {
			return nil, errors.New("host adapter capability id is required")
		}
		switch capability.State {
		case AdapterStateAvailable:
		case AdapterStateUnavailable, AdapterStateDegraded:
			if capability.Reason == "" {
				return nil, fmt.Errorf("host adapter %q in state %q requires a reason", key, capability.State)
			}
		default:
			return nil, fmt.Errorf("host adapter %q has invalid state %q", key, capability.State)
		}
		result[key] = capability
	}
	return result, nil
}

func adapterAvailable(adapters map[string]AdapterCapability, id string) bool {
	return adapters[id].State == AdapterStateAvailable
}

func validateNonSecretConfiguration(value interface{}, path string) error {
	switch values := value.(type) {
	case map[string]interface{}:
		for key, child := range values {
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(key), "_", ""), "-", ""))
			if normalized == "token" || strings.HasSuffix(normalized, "apikey") || strings.HasSuffix(normalized, "password") || strings.HasSuffix(normalized, "secret") || strings.HasSuffix(normalized, "credential") || strings.HasSuffix(normalized, "credentialid") || strings.HasSuffix(normalized, "accesstoken") || strings.HasSuffix(normalized, "refreshtoken") {
				return fmt.Errorf("binding config %s%s must use an opaque credential reference", path, key)
			}
			if err := validateNonSecretConfiguration(child, path+key+"."); err != nil {
				return err
			}
		}
	case []interface{}:
		for index, child := range values {
			if err := validateNonSecretConfiguration(child, fmt.Sprintf("%s%d.", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}
