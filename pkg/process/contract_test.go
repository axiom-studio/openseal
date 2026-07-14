package process

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestDecodeInvocationAcceptsArgumentSafeInstallerEnvelope(t *testing.T) {
	invocation, err := DecodeInvocation(map[string]interface{}{
		ExecutableKey: "summarize", ArgumentsKey: []interface{}{"https://example.com", "--extract-only"},
		InstallersKey:       []interface{}{map[string]interface{}{"id": "brew", "kind": "brew", "formula": "owner/tap/summarize", "executables": []interface{}{"summarize"}}},
		EnvironmentNamesKey: []interface{}{"OPENAI_API_KEY"}, TimeoutSecondsKey: float64(120),
	})
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Executable != "summarize" || len(invocation.Arguments) != 2 || invocation.Arguments[1] != "--extract-only" ||
		len(invocation.Installers) != 1 || invocation.Installers[0].Formula != "owner/tap/summarize" || invocation.TimeoutSeconds != 120 {
		t.Fatalf("invocation = %#v", invocation)
	}
}

func TestDecodeInvocationRejectsShellAndInstallerConfusion(t *testing.T) {
	valid := map[string]interface{}{
		ExecutableKey: "summarize", ArgumentsKey: []string{}, TimeoutSecondsKey: 120,
		InstallersKey: []capability.Installer{{ID: "brew", Kind: "brew", Formula: "owner/tap/summarize", Executables: []string{"summarize"}}},
	}
	for name, mutate := range map[string]func(map[string]interface{}){
		"shell executable": func(value map[string]interface{}) { value[ExecutableKey] = "sh -c" },
		"nul argument":     func(value map[string]interface{}) { value[ArgumentsKey] = []string{"safe", "bad\x00value"} },
		"wrong installer": func(value map[string]interface{}) {
			value[InstallersKey] = []capability.Installer{{ID: "brew", Kind: "brew", Executables: []string{"other"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			copy := make(map[string]interface{}, len(valid))
			for key, value := range valid {
				copy[key] = value
			}
			mutate(copy)
			if _, err := DecodeInvocation(copy); err == nil || strings.Contains(err.Error(), "bad\x00value") {
				t.Fatalf("unsafe invocation error = %v", err)
			}
		})
	}
}
