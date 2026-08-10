package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"gopkg.in/yaml.v3"
)

const (
	StandaloneContextAPIVersion = "openseal.dev/v1alpha1"
	StandaloneContextKind       = "StandaloneContext"
)

// StandaloneContext is the local counterpart to an embedding host's Vault and
// model-provider grants. It contains references to secret sources, never the
// secret values themselves. Authoring credentials are resolved into the local
// provider process; Action credentials are resolved only for the exact
// ActionCall carrying a governed opaque CredentialReference.
type StandaloneContext struct {
	APIVersion  string                      `yaml:"apiVersion"`
	Kind        string                      `yaml:"kind"`
	Authoring   *StandaloneAuthoringContext `yaml:"authoring,omitempty"`
	Credentials []StandaloneCredential      `yaml:"credentials,omitempty"`

	baseDir string
	entries map[string]StandaloneCredential
}

type StandaloneAuthoringContext struct {
	BaseURL    string                         `yaml:"baseURL"`
	Model      string                         `yaml:"model"`
	Credential capability.CredentialReference `yaml:"credential"`
}

type StandaloneCredential struct {
	Scope       runtime.Scope `yaml:"scope"`
	Kind        string        `yaml:"kind"`
	ID          string        `yaml:"id"`
	DisplayName string        `yaml:"displayName"`
	BindingKeys []string      `yaml:"bindingKeys,omitempty"`
	Env         string        `yaml:"env,omitempty"`
	File        string        `yaml:"file,omitempty"`
}

// LoadStandaloneContext loads an optional local context file. A missing file
// means no local Vault has been configured; malformed or unsafe configuration
// fails closed.
func LoadStandaloneContext(path string) (*StandaloneContext, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return emptyStandaloneContext(""), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyStandaloneContext(filepath.Dir(path)), nil
		}
		return nil, fmt.Errorf("read standalone context: %w", err)
	}
	var result StandaloneContext
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("parse standalone context: %w", err)
	}
	result.baseDir = filepath.Dir(path)
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return &result, nil
}

func emptyStandaloneContext(baseDir string) *StandaloneContext {
	return &StandaloneContext{
		APIVersion: StandaloneContextAPIVersion,
		Kind:       StandaloneContextKind,
		baseDir:    baseDir,
		entries:    map[string]StandaloneCredential{},
	}
}

func (c *StandaloneContext) Validate() error {
	if c == nil {
		return errors.New("standalone context is required")
	}
	if c.APIVersion != StandaloneContextAPIVersion || c.Kind != StandaloneContextKind {
		return fmt.Errorf("standalone context must use apiVersion %s and kind %s", StandaloneContextAPIVersion, StandaloneContextKind)
	}
	c.entries = make(map[string]StandaloneCredential, len(c.Credentials))
	for index := range c.Credentials {
		entry := &c.Credentials[index]
		entry.Kind, entry.ID = strings.TrimSpace(entry.Kind), strings.TrimSpace(entry.ID)
		entry.DisplayName, entry.Env, entry.File = strings.TrimSpace(entry.DisplayName), strings.TrimSpace(entry.Env), strings.TrimSpace(entry.File)
		if err := entry.Scope.Validate(); err != nil {
			return fmt.Errorf("standalone credential %d scope: %w", index, err)
		}
		if entry.Kind == "" || entry.ID == "" || entry.DisplayName == "" {
			return fmt.Errorf("standalone credential %d requires kind, id, and displayName", index)
		}
		if (entry.Env == "") == (entry.File == "") {
			return fmt.Errorf("standalone credential %s/%s must configure exactly one of env or file", entry.Kind, entry.ID)
		}
		if entry.File != "" && !filepath.IsAbs(entry.File) {
			entry.File = filepath.Clean(filepath.Join(c.baseDir, entry.File))
		}
		entry.BindingKeys = normalizedContextStrings(entry.BindingKeys)
		key := standaloneCredentialKey(entry.Scope, capability.CredentialReference{Kind: entry.Kind, ID: entry.ID})
		if _, exists := c.entries[key]; exists {
			return fmt.Errorf("standalone credential %s/%s is duplicated in scope %s:%s", entry.Kind, entry.ID, entry.Scope.Kind, entry.Scope.ID)
		}
		c.entries[key] = *entry
	}
	if c.Authoring != nil {
		c.Authoring.BaseURL = strings.TrimSpace(c.Authoring.BaseURL)
		c.Authoring.Model = strings.TrimSpace(c.Authoring.Model)
		c.Authoring.Credential.Kind = strings.TrimSpace(c.Authoring.Credential.Kind)
		c.Authoring.Credential.ID = strings.TrimSpace(c.Authoring.Credential.ID)
		if c.Authoring.BaseURL == "" || c.Authoring.Model == "" || c.Authoring.Credential.Kind == "" || c.Authoring.Credential.ID == "" {
			return errors.New("standalone authoring requires baseURL, model, and an opaque credential reference")
		}
	}
	return nil
}

func (c *StandaloneContext) ResolveCredentials(_ context.Context, request runtime.CredentialResolutionRequest) (map[string]string, error) {
	result := make(map[string]string, len(request.References))
	for name, reference := range request.References {
		value, err := c.ResolveReference(request.Scope, reference)
		if err != nil {
			return nil, fmt.Errorf("resolve credential %s: %w", name, err)
		}
		result[name] = value
	}
	return result, nil
}

func (c *StandaloneContext) ResolveReference(scope runtime.Scope, reference capability.CredentialReference) (string, error) {
	if c == nil {
		return "", errors.New("standalone context is unavailable")
	}
	entry, ok := c.entries[standaloneCredentialKey(scope, reference)]
	if !ok {
		return "", fmt.Errorf("credential reference %s/%s is not configured in scope %s:%s", reference.Kind, reference.ID, scope.Kind, scope.ID)
	}
	if entry.Env != "" {
		value := os.Getenv(entry.Env)
		if value == "" {
			return "", fmt.Errorf("environment source %s is empty", entry.Env)
		}
		return value, nil
	}
	pathInfo, err := os.Lstat(entry.File)
	if err != nil {
		return "", fmt.Errorf("inspect credential file: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("credential file must not be a symbolic link")
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm()&0o077 != 0 {
		return "", errors.New("credential file must be regular and accessible only by its owner (mode 0600 or stricter)")
	}
	data, err := os.ReadFile(entry.File)
	if err != nil {
		return "", fmt.Errorf("read credential file: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("credential file is empty")
	}
	return value, nil
}

func (c *StandaloneContext) CredentialChoices(scope runtime.Scope) []capability.CredentialBindingChoice {
	if c == nil {
		return nil
	}
	result := make([]capability.CredentialBindingChoice, 0)
	for _, entry := range c.entries {
		if entry.Scope != scope {
			continue
		}
		result = append(result, capability.CredentialBindingChoice{
			Reference:   capability.CredentialReference{Kind: entry.Kind, ID: entry.ID},
			DisplayName: entry.DisplayName, BindingKeys: append([]string(nil), entry.BindingKeys...),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i].Reference, result[j].Reference
		return left.Kind+"\x00"+left.ID < right.Kind+"\x00"+right.ID
	})
	return result
}

func (c *StandaloneContext) ListWorkerScopes(context.Context) ([]runtime.Scope, error) {
	seen := map[string]runtime.Scope{}
	for _, entry := range c.entries {
		seen[workerScopeKey(entry.Scope)] = entry.Scope
	}
	result := make([]runtime.Scope, 0, len(seen))
	for _, scope := range seen {
		result = append(result, scope)
	}
	sort.Slice(result, func(i, j int) bool {
		return workerScopeKey(result[i]) < workerScopeKey(result[j])
	})
	return result, nil
}

func standaloneCredentialKey(scope runtime.Scope, reference capability.CredentialReference) string {
	return workerScopeKey(scope) + "\x00" + strings.TrimSpace(reference.Kind) + "\x00" + strings.TrimSpace(reference.ID)
}

func normalizedContextStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

var _ runtime.CredentialResolver = (*StandaloneContext)(nil)
var _ runtime.WorkerScopeSource = (*StandaloneContext)(nil)
