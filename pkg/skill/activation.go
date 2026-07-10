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
)

// HostCapabilityState is a non-secret declaration of the environment in which
// a deployment's skill snapshot will run. Environment contains names only;
// secret values are resolved later by a bounded action worker.
type HostCapabilityState struct {
	OperatingSystem string                 `json:"operatingSystem,omitempty"`
	Executables     map[string]bool        `json:"executables,omitempty"`
	Environment     map[string]bool        `json:"environment,omitempty"`
	Configuration   map[string]interface{} `json:"configuration,omitempty"`
	ResourceRoots   map[string]string      `json:"resourceRoots,omitempty"`
	Adapters        map[string]bool        `json:"adapters,omitempty"`
	Revision        string                 `json:"revision,omitempty"`
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
	SourceDigest     string                 `json:"sourceDigest,omitempty"`
	ConfigurationKey string                 `json:"configurationKey,omitempty"`
	Configuration    map[string]interface{} `json:"configuration,omitempty"`
	ResourceRoot     string                 `json:"resourceRoot,omitempty"`
	Prompt           *PromptModule          `json:"prompt,omitempty"`
	Actions          []ModelAction          `json:"actions,omitempty"`
}

type UnavailableSkill struct {
	BindingID    string               `json:"bindingId"`
	SkillID      string               `json:"skillId"`
	SkillVersion string               `json:"skillVersion"`
	Reasons      []AvailabilityReason `json:"reasons"`
}

// ActivationSnapshot is the immutable model-visible skill surface for one
// deployment turn/session. SnapshotID excludes CreatedAt and is stable for
// equivalent bindings and declared host capabilities across process restarts.
type ActivationSnapshot struct {
	SnapshotID   string             `json:"snapshotId"`
	Scope        ScopeReference     `json:"scope"`
	DeploymentID string             `json:"deploymentId"`
	HostRevision string             `json:"hostRevision,omitempty"`
	Adapters     map[string]bool    `json:"adapters,omitempty"`
	Skills       []ActivatedSkill   `json:"skills"`
	Unavailable  []UnavailableSkill `json:"unavailable,omitempty"`
	CreatedAt    time.Time          `json:"createdAt"`
}

func (c *Catalog) Activate(_ context.Context, scope ScopeReference, deploymentID string, host HostCapabilityState) (*ActivationSnapshot, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}

	c.mu.RLock()
	bindings := make([]*Binding, 0)
	definitions := make(map[string]*Definition)
	for _, binding := range c.bindings {
		if binding.Scope != scope || binding.DeploymentID != deploymentID {
			continue
		}
		copy := cloneBinding(binding)
		bindings = append(bindings, copy)
		if definition := c.skills[definitionKey(binding.SkillID, binding.SkillVersion)]; definition != nil {
			definitions[definitionKey(binding.SkillID, binding.SkillVersion)] = cloneDefinition(definition)
		}
	}
	c.mu.RUnlock()
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].ID < bindings[j].ID })

	snapshot := &ActivationSnapshot{
		Scope: scope, DeploymentID: deploymentID, HostRevision: strings.TrimSpace(host.Revision),
		Adapters: cloneBoolMap(host.Adapters), Skills: make([]ActivatedSkill, 0, len(bindings)), CreatedAt: time.Now().UTC(),
	}
	for _, binding := range bindings {
		definition := definitions[definitionKey(binding.SkillID, binding.SkillVersion)]
		if definition == nil {
			snapshot.Unavailable = append(snapshot.Unavailable, UnavailableSkill{
				BindingID: binding.ID, SkillID: binding.SkillID, SkillVersion: binding.SkillVersion,
				Reasons: []AvailabilityReason{{Code: "definition_missing", Message: "bound skill definition is unavailable"}},
			})
			continue
		}
		reasons := evaluateAvailability(definition, binding, host)
		resourceRoot := strings.TrimSpace(host.ResourceRoots[definitionKey(definition.ID, definition.Version)])
		prompt := (*PromptModule)(nil)
		if binding.EnablePrompt && definition.Prompt != nil {
			prompt = cloneDefinition(definition).Prompt
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
				BindingID: binding.ID, SkillID: binding.SkillID, SkillVersion: binding.SkillVersion, Reasons: reasons,
			})
			continue
		}
		actions := make([]ModelAction, 0, len(binding.AllowedActions))
		for _, name := range binding.AllowedActions {
			action := definition.Actions[name]
			actions = append(actions, ModelAction{
				Name: definition.ID + "." + name, Description: action.Description, SkillID: definition.ID,
				Version: definition.Version, Action: name, InputSchema: cloneMap(action.InputSchema), Risk: action.Risk, SideEffect: action.SideEffect,
			})
		}
		sort.Slice(actions, func(i, j int) bool { return actions[i].Name < actions[j].Name })
		digest := ""
		if definition.Source != nil {
			digest = definition.Source.Digest
		}
		snapshot.Skills = append(snapshot.Skills, ActivatedSkill{
			BindingID: binding.ID, BindingRevision: binding.Revision, SkillID: definition.ID, SkillVersion: definition.Version,
			SourceDigest: digest, ConfigurationKey: definition.ConfigurationKey, Configuration: cloneMap(binding.Config),
			ResourceRoot: resourceRoot, Prompt: prompt, Actions: actions,
		})
	}
	sort.Slice(snapshot.Unavailable, func(i, j int) bool { return snapshot.Unavailable[i].BindingID < snapshot.Unavailable[j].BindingID })
	snapshot.SnapshotID = activationSnapshotDigest(snapshot)
	return snapshot, nil
}

func evaluateAvailability(definition *Definition, binding *Binding, host HostCapabilityState) []AvailabilityReason {
	if definition.Requirements.AlwaysAvailable {
		return nil
	}
	reasons := make([]AvailabilityReason, 0)
	if len(definition.Requirements.OperatingSystems) > 0 && !containsOperatingSystem(definition.Requirements.OperatingSystems, host.OperatingSystem) {
		reasons = append(reasons, AvailabilityReason{Code: "operating_system_unavailable", Requirement: strings.TrimSpace(host.OperatingSystem), Message: "host operating system is not supported"})
	}
	for _, executable := range definition.Requirements.Executables {
		if !host.Executables[executable] {
			reasons = append(reasons, AvailabilityReason{Code: "executable_missing", Requirement: executable, Message: "required executable is unavailable"})
		}
	}
	if len(definition.Requirements.AnyExecutables) > 0 {
		available := false
		for _, executable := range definition.Requirements.AnyExecutables {
			available = available || host.Executables[executable]
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
		Scope        ScopeReference     `json:"scope"`
		DeploymentID string             `json:"deploymentId"`
		HostRevision string             `json:"hostRevision,omitempty"`
		Adapters     map[string]bool    `json:"adapters,omitempty"`
		Skills       []ActivatedSkill   `json:"skills"`
		Unavailable  []UnavailableSkill `json:"unavailable,omitempty"`
	}{snapshot.Scope, snapshot.DeploymentID, snapshot.HostRevision, snapshot.Adapters, snapshot.Skills, snapshot.Unavailable}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func cloneBoolMap(value map[string]bool) map[string]bool {
	if value == nil {
		return nil
	}
	result := make(map[string]bool, len(value))
	for key, child := range value {
		result[key] = child
	}
	return result
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
