package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestStandaloneContextResolvesScopedEnvironmentAndPrivateFileCredentials(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "token")
	if err := os.WriteFile(secretPath, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENSEAL_TEST_API_KEY", "environment-secret")
	contextPath := filepath.Join(dir, "context.yaml")
	data := `apiVersion: openseal.dev/v1alpha1
kind: StandaloneContext
authoring:
  baseURL: https://models.example/v1
  model: example-model
  credential:
    kind: api-key
    id: model
credentials:
  - scope: {kind: local, id: default}
    kind: api-key
    id: model
    displayName: Local model provider
    bindingKeys: [MODEL_PROVIDER]
    env: OPENSEAL_TEST_API_KEY
  - scope: {kind: local, id: default}
    kind: provider-token
    id: forum
    displayName: Forum account
    file: token
`
	if err := os.WriteFile(contextPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	configured, err := LoadStandaloneContext(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	scope := runtime.Scope{Kind: "local", ID: "default"}
	if got, err := configured.ResolveReference(scope, capability.CredentialReference{Kind: "api-key", ID: "model"}); err != nil || got != "environment-secret" {
		t.Fatalf("environment credential = %q, %v", got, err)
	}
	if got, err := configured.ResolveReference(scope, capability.CredentialReference{Kind: "provider-token", ID: "forum"}); err != nil || got != "file-secret" {
		t.Fatalf("file credential = %q, %v", got, err)
	}
	choices := configured.CredentialChoices(scope)
	if len(choices) != 2 || choices[0].Reference.ID != "model" || choices[0].DisplayName != "Local model provider" {
		t.Fatalf("credential choices = %#v", choices)
	}
	if _, err := configured.ResolveReference(runtime.Scope{Kind: "local", ID: "other"}, capability.CredentialReference{Kind: "api-key", ID: "model"}); err == nil {
		t.Fatal("cross-scope credential resolution succeeded")
	}
}

func TestStandaloneContextFailsClosedForInlineUnknownAndUnsafeFileSources(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"inline": `apiVersion: openseal.dev/v1alpha1
kind: StandaloneContext
credentials:
  - scope: {kind: local, id: default}
    kind: api-key
    id: model
    displayName: Model
    value: secret
`,
		"ambiguous": `apiVersion: openseal.dev/v1alpha1
kind: StandaloneContext
credentials:
  - scope: {kind: local, id: default}
    kind: api-key
    id: model
    displayName: Model
    env: MODEL_KEY
    file: key
`,
	} {
		path := filepath.Join(dir, name+".yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadStandaloneContext(path); err == nil {
			t.Fatalf("%s context succeeded", name)
		}
	}

	secretPath := filepath.Join(dir, "unsafe")
	if err := os.WriteFile(secretPath, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	configured := emptyStandaloneContext(dir)
	configured.Credentials = []StandaloneCredential{{
		Scope: runtime.Scope{Kind: "local", ID: "default"}, Kind: "api-key", ID: "unsafe", DisplayName: "Unsafe", File: secretPath,
	}}
	if err := configured.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := configured.ResolveReference(runtime.Scope{Kind: "local", ID: "default"}, capability.CredentialReference{Kind: "api-key", ID: "unsafe"}); err == nil {
		t.Fatal("world-readable credential file succeeded")
	}

	privatePath := filepath.Join(dir, "private")
	linkPath := filepath.Join(dir, "private-link")
	if err := os.WriteFile(privatePath, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(privatePath, linkPath); err != nil {
		t.Fatal(err)
	}
	configured.Credentials = []StandaloneCredential{{
		Scope: runtime.Scope{Kind: "local", ID: "default"}, Kind: "api-key", ID: "link", DisplayName: "Link", File: linkPath,
	}}
	if err := configured.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := configured.ResolveReference(runtime.Scope{Kind: "local", ID: "default"}, capability.CredentialReference{Kind: "api-key", ID: "link"}); err == nil {
		t.Fatal("symbolic-link credential file succeeded")
	}
}

func TestMissingStandaloneContextIsAnEmptyOptionalVault(t *testing.T) {
	configured, err := LoadStandaloneContext(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil || len(configured.CredentialChoices(runtime.Scope{Kind: "local", ID: "default"})) != 0 {
		t.Fatalf("missing context = %#v, %v", configured, err)
	}
}
