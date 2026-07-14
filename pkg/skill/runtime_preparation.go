package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	opensealprocess "github.com/axiom-studio/openseal/pkg/process"
)

// RuntimePreparationState is the host-observed lifecycle of one immutable
// runtime preparation. Preparing is intentionally not treated as ready: an
// Agent may only see process actions after their exact runtime is usable.
type RuntimePreparationState string

const (
	RuntimePreparationPreparing   RuntimePreparationState = "preparing"
	RuntimePreparationReady       RuntimePreparationState = "ready"
	RuntimePreparationUnavailable RuntimePreparationState = "unavailable"
)

// RuntimePreparationRequest is a secret-free, deterministic request for the
// runtime needed by one activated Skill. Scope and ownership are authorization
// inputs; PreparationID is content identity and deliberately excludes them so
// a host may safely deduplicate immutable artifacts without sharing writable
// execution state.
type RuntimePreparationRequest struct {
	PreparationID     string                 `json:"preparationId"`
	Scope             ScopeReference         `json:"scope"`
	DeploymentID      string                 `json:"deploymentId"`
	BindingID         string                 `json:"bindingId"`
	SkillID           string                 `json:"skillId"`
	SkillVersion      string                 `json:"skillVersion"`
	SourceDigest      string                 `json:"sourceDigest"`
	SourceTrustDigest string                 `json:"sourceTrustDigest"`
	BuilderRevision   string                 `json:"builderRevision"`
	OperatingSystem   string                 `json:"operatingSystem"`
	Architecture      string                 `json:"architecture"`
	Executables       []string               `json:"executables"`
	Installers        []capability.Installer `json:"installers"`
}

// PreparedRuntime is a non-secret immutable execution reference. RuntimeID is
// host-defined (for example, an OCI digest or content-addressed filesystem
// artifact); Revision and Adapter make refresh and audit explicit.
type PreparedRuntime struct {
	PreparationID   string   `json:"preparationId"`
	RuntimeID       string   `json:"runtimeId"`
	Revision        string   `json:"revision"`
	Adapter         string   `json:"adapter"`
	OperatingSystem string   `json:"operatingSystem"`
	Architecture    string   `json:"architecture"`
	Executables     []string `json:"executables"`
}

type RuntimePreparationResult struct {
	State   RuntimePreparationState `json:"state"`
	Runtime *PreparedRuntime        `json:"runtime,omitempty"`
	Reason  string                  `json:"reason,omitempty"`
}

// RuntimePreparer is implemented by a governed host. Implementations should
// reconcile quickly and return Preparing while a durable asynchronous build
// continues rather than holding an Agent turn open for a cold installation.
type RuntimePreparer interface {
	PrepareRuntime(context.Context, RuntimePreparationRequest) (*RuntimePreparationResult, error)
}

// RuntimePreparationID derives stable content identity from source,
// installers, required executables, and target platform.
func RuntimePreparationID(request RuntimePreparationRequest) (string, error) {
	normalized, err := normalizeRuntimePreparationRequest(request)
	if err != nil {
		return "", err
	}
	payload := struct {
		SkillID           string                 `json:"skillId"`
		SkillVersion      string                 `json:"skillVersion"`
		SourceDigest      string                 `json:"sourceDigest"`
		SourceTrustDigest string                 `json:"sourceTrustDigest"`
		BuilderRevision   string                 `json:"builderRevision"`
		OperatingSystem   string                 `json:"operatingSystem"`
		Architecture      string                 `json:"architecture"`
		Executables       []string               `json:"executables"`
		Installers        []capability.Installer `json:"installers"`
	}{
		SkillID: normalized.SkillID, SkillVersion: normalized.SkillVersion, SourceDigest: normalized.SourceDigest,
		SourceTrustDigest: normalized.SourceTrustDigest, BuilderRevision: normalized.BuilderRevision,
		OperatingSystem: normalized.OperatingSystem, Architecture: normalized.Architecture,
		Executables: normalized.Executables, Installers: normalized.Installers,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", errors.New("runtime preparation identity could not be encoded")
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// ValidateRuntimePreparationRequest lets host adapters reject malformed or
// drifted requests before creating build resources.
func ValidateRuntimePreparationRequest(request RuntimePreparationRequest) error {
	normalized, err := normalizeRuntimePreparationRequest(request)
	if err != nil {
		return err
	}
	id, err := RuntimePreparationID(normalized)
	if err != nil {
		return err
	}
	if request.PreparationID != id {
		return errors.New("runtime preparation identity does not match its content")
	}
	return nil
}

func normalizeRuntimePreparationRequest(request RuntimePreparationRequest) (RuntimePreparationRequest, error) {
	request.Scope.Kind = strings.TrimSpace(request.Scope.Kind)
	request.Scope.ID = strings.TrimSpace(request.Scope.ID)
	request.DeploymentID = strings.TrimSpace(request.DeploymentID)
	request.BindingID = strings.TrimSpace(request.BindingID)
	request.SkillID = strings.TrimSpace(request.SkillID)
	request.SkillVersion = strings.TrimSpace(request.SkillVersion)
	request.SourceDigest = strings.ToLower(strings.TrimSpace(request.SourceDigest))
	request.SourceTrustDigest = strings.ToLower(strings.TrimSpace(request.SourceTrustDigest))
	request.BuilderRevision = strings.TrimSpace(request.BuilderRevision)
	request.OperatingSystem = normalizeOperatingSystem(request.OperatingSystem)
	request.Architecture = normalizeRuntimeArchitecture(request.Architecture)
	if request.Scope.Kind == "" || request.Scope.ID == "" || request.DeploymentID == "" || request.BindingID == "" ||
		request.SkillID == "" || request.SkillVersion == "" || request.BuilderRevision == "" || request.OperatingSystem == "" || request.Architecture == "" {
		return RuntimePreparationRequest{}, errors.New("runtime preparation scope, ownership, skill, and platform are required")
	}
	if len(request.BuilderRevision) > 512 || strings.ContainsAny(request.BuilderRevision, "\x00\r\n") {
		return RuntimePreparationRequest{}, errors.New("runtime preparation builder revision is invalid")
	}
	for _, value := range []string{request.SourceDigest, request.SourceTrustDigest} {
		digest, digestErr := hex.DecodeString(value)
		if digestErr != nil || len(digest) != sha256.Size {
			return RuntimePreparationRequest{}, errors.New("runtime preparation requires SHA-256 source and trust digests")
		}
	}
	var err error
	request.Executables, err = normalizedRuntimeStrings(request.Executables, opensealprocess.ValidExecutableName)
	if err != nil || len(request.Executables) == 0 {
		return RuntimePreparationRequest{}, errors.New("runtime preparation requires portable executable names")
	}
	if len(request.Installers) == 0 || len(request.Installers) > 16 {
		return RuntimePreparationRequest{}, errors.New("runtime preparation requires bounded installers")
	}
	for index := range request.Installers {
		installer := &request.Installers[index]
		installer.ID = strings.TrimSpace(installer.ID)
		installer.Kind = strings.ToLower(strings.TrimSpace(installer.Kind))
		installer.Label = strings.TrimSpace(installer.Label)
		installer.Package = strings.TrimSpace(installer.Package)
		installer.Module = strings.TrimSpace(installer.Module)
		installer.Formula = strings.TrimSpace(installer.Formula)
		installer.URL = strings.TrimSpace(installer.URL)
		installer.Archive = strings.TrimSpace(installer.Archive)
		installer.TargetDirectory = strings.TrimSpace(installer.TargetDirectory)
		if installer.Kind == "" {
			return RuntimePreparationRequest{}, errors.New("runtime preparation installer kind is required")
		}
		for osIndex := range installer.OperatingSystems {
			installer.OperatingSystems[osIndex] = normalizeOperatingSystem(installer.OperatingSystems[osIndex])
		}
		installer.OperatingSystems, err = normalizedRuntimeStrings(installer.OperatingSystems, func(value string) bool { return value != "" })
		if err != nil {
			return RuntimePreparationRequest{}, errors.New("runtime preparation installer operating systems are invalid")
		}
		installer.Executables, err = normalizedRuntimeStrings(installer.Executables, opensealprocess.ValidExecutableName)
		if err != nil || len(installer.Executables) == 0 {
			return RuntimePreparationRequest{}, errors.New("runtime preparation installer executables are invalid")
		}
	}
	sort.Slice(request.Installers, func(i, j int) bool {
		left, _ := json.Marshal(request.Installers[i])
		right, _ := json.Marshal(request.Installers[j])
		return string(left) < string(right)
	})
	deduplicated := request.Installers[:0]
	previous := ""
	for _, installer := range request.Installers {
		encoded, _ := json.Marshal(installer)
		if string(encoded) != previous {
			deduplicated = append(deduplicated, installer)
			previous = string(encoded)
		}
	}
	request.Installers = deduplicated
	for _, executable := range request.Executables {
		provided := false
		for _, installer := range request.Installers {
			if containsString(installer.Executables, executable) && installerSupportsOperatingSystem(installer, request.OperatingSystem) {
				provided = true
				break
			}
		}
		if !provided {
			return RuntimePreparationRequest{}, fmt.Errorf("runtime preparation has no compatible installer for executable %q", executable)
		}
	}
	return request, nil
}

func normalizeRuntimeArchitecture(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "x86_64", "x86-64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func normalizedRuntimeStrings(values []string, valid func(string) bool) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !valid(value) || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("invalid runtime preparation value")
		}
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	sort.Strings(result)
	return result, nil
}

func runtimePreparationRequest(scope ScopeReference, deploymentID string, binding *Binding, definition *Definition, host HostCapabilityState) (*RuntimePreparationRequest, error) {
	if binding == nil || definition == nil {
		return nil, nil
	}
	executables := make([]string, 0)
	installers := make([]capability.Installer, 0)
	for _, actionName := range binding.AllowedActions {
		action, ok := definition.Actions[actionName]
		if !ok || action.Transport == nil || action.Transport.Kind != "tool" || action.Transport.Endpoint != opensealprocess.TransportName {
			continue
		}
		executable, _ := action.Transport.Arguments[opensealprocess.ExecutableKey].Literal.(string)
		executables = append(executables, executable)
		encoded, err := json.Marshal(action.Transport.Arguments[opensealprocess.InstallersKey].Literal)
		var actionInstallers []capability.Installer
		if err != nil || json.Unmarshal(encoded, &actionInstallers) != nil {
			return nil, errors.New("process action installers cannot be prepared")
		}
		installers = append(installers, actionInstallers...)
	}
	if len(executables) == 0 {
		return nil, nil
	}
	request := RuntimePreparationRequest{
		Scope: scope, DeploymentID: deploymentID, BindingID: binding.ID, SkillID: definition.ID, SkillVersion: definition.Version,
		SourceDigest: definitionSourceDigest(definition), SourceTrustDigest: definitionSourceTrustDigest(definition),
		BuilderRevision: host.Adapters[AdapterPreparedRuntime].Version, OperatingSystem: host.OperatingSystem, Architecture: host.Architecture,
		Executables: executables, Installers: installers,
	}
	normalized, err := normalizeRuntimePreparationRequest(request)
	if err != nil {
		return nil, err
	}
	normalized.PreparationID, err = RuntimePreparationID(normalized)
	if err != nil {
		return nil, err
	}
	return &normalized, nil
}

func definitionSourceTrustDigest(definition *Definition) string {
	trust := map[string]interface{}{}
	if definition != nil && definition.Source != nil && definition.Source.Trust != nil {
		trust = definition.Source.Trust
	}
	encoded, _ := json.Marshal(trust)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func validatePreparedRuntime(request RuntimePreparationRequest, runtime *PreparedRuntime) error {
	if runtime == nil || runtime.PreparationID != request.PreparationID || strings.TrimSpace(runtime.RuntimeID) == "" ||
		strings.TrimSpace(runtime.Revision) == "" || strings.TrimSpace(runtime.Adapter) == "" ||
		normalizeOperatingSystem(runtime.OperatingSystem) != request.OperatingSystem || normalizeRuntimeArchitecture(runtime.Architecture) != request.Architecture {
		return errors.New("prepared runtime identity is incomplete or does not match the request")
	}
	if len(runtime.RuntimeID) > 2048 || len(runtime.Revision) > 512 || len(runtime.Adapter) > 256 || strings.ContainsAny(runtime.RuntimeID+runtime.Revision+runtime.Adapter, "\x00\r\n") {
		return errors.New("prepared runtime identity is invalid")
	}
	executables, err := normalizedRuntimeStrings(runtime.Executables, opensealprocess.ValidExecutableName)
	if err != nil {
		return err
	}
	for _, required := range request.Executables {
		if !containsString(executables, required) {
			return fmt.Errorf("prepared runtime does not provide executable %q", required)
		}
	}
	runtime.OperatingSystem = request.OperatingSystem
	runtime.Architecture = request.Architecture
	runtime.RuntimeID = strings.TrimSpace(runtime.RuntimeID)
	runtime.Revision = strings.TrimSpace(runtime.Revision)
	runtime.Adapter = strings.TrimSpace(runtime.Adapter)
	runtime.Executables = executables
	return nil
}

// ValidatePreparedRuntimeReference validates the canonical, secret-free
// identity carried from an activation snapshot into a durable action. It does
// not prove that a host artifact exists; execution hosts must additionally
// resolve RuntimeID and verify its published preparation and revision before
// mounting it.
func ValidatePreparedRuntimeReference(runtime *PreparedRuntime) error {
	if runtime == nil {
		return errors.New("prepared runtime reference is required")
	}
	preparationID := strings.TrimSpace(runtime.PreparationID)
	digest, err := hex.DecodeString(strings.TrimPrefix(preparationID, "sha256:"))
	if !strings.HasPrefix(preparationID, "sha256:") || err != nil || len(digest) != sha256.Size {
		return errors.New("prepared runtime preparation identity is invalid")
	}
	if strings.TrimSpace(runtime.RuntimeID) == "" || strings.TrimSpace(runtime.Revision) == "" || strings.TrimSpace(runtime.Adapter) == "" ||
		len(runtime.RuntimeID) > 2048 || len(runtime.Revision) > 512 || len(runtime.Adapter) > 256 ||
		strings.ContainsAny(runtime.RuntimeID+runtime.Revision+runtime.Adapter, "\x00\r\n") {
		return errors.New("prepared runtime artifact identity is invalid")
	}
	operatingSystem := normalizeOperatingSystem(runtime.OperatingSystem)
	architecture := normalizeRuntimeArchitecture(runtime.Architecture)
	if operatingSystem == "" || architecture == "" || operatingSystem != runtime.OperatingSystem || architecture != runtime.Architecture {
		return errors.New("prepared runtime platform identity is invalid")
	}
	executables, err := normalizedRuntimeStrings(runtime.Executables, opensealprocess.ValidExecutableName)
	if err != nil || len(executables) == 0 || len(executables) != len(runtime.Executables) {
		return errors.New("prepared runtime executable identity is invalid")
	}
	for index := range executables {
		if executables[index] != runtime.Executables[index] {
			return errors.New("prepared runtime executables must be canonical")
		}
	}
	return nil
}
