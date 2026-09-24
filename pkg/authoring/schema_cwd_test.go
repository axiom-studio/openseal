package authoring

import (
	"os"
	"path/filepath"
	"testing"

	validateschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// The desktop daemon runs from ~/Library/Application Support/<id>, and the
// schema validator resolves bare resource names against the working
// directory. A space in that path broke every authoring generation with
// "failing loading file:///.../Application%20Support/...json".
func TestAuthoringSchemasCompileFromCwdWithSpace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Application Support")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	for name, resource := range map[string]string{
		"intent": authoringIntentSchemaResource,
		"result": authoringResultSchemaResource,
	} {
		document := map[string]interface{}{
			"$schema":    "https://json-schema.org/draft/2020-12/schema",
			"type":       "object",
			"properties": map[string]interface{}{"role": map[string]interface{}{"$ref": "#/$defs/role"}},
			"$defs":      map[string]interface{}{"role": map[string]interface{}{"type": "string"}},
		}
		compiler := validateschema.NewCompiler()
		if err := compiler.AddResource(resource, document); err != nil {
			t.Fatalf("%s: register: %v", name, err)
		}
		if _, err := compiler.Compile(resource); err != nil {
			t.Fatalf("%s: compile from %q: %v", name, dir, err)
		}
	}
}
