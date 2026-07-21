package httpaction

import "testing"

func TestDecodeInvocationPreservesFixedReadContract(t *testing.T) {
	input := map[string]interface{}{
		BaseURLKey: "https://api.example.test", MethodKey: "GET", PathKey: "/v1/search",
		ParametersKey:        map[string]interface{}{"keyword": "agents"},
		ParameterContractKey: []Parameter{{Name: "keyword", Location: "query", Required: true}, {Name: "after", Location: "query", Default: ""}},
		CredentialNameKey:    "API_TOKEN", CredentialParameterKey: "token",
	}
	invocation, err := DecodeInvocation(input)
	if err != nil {
		t.Fatal(err)
	}
	if invocation.BaseURL != "https://api.example.test" || invocation.Method != "GET" || invocation.Path != "/v1/search" ||
		invocation.Parameters["keyword"] != "agents" || invocation.CredentialName != "API_TOKEN" || invocation.CredentialParameter != "token" {
		t.Fatalf("decoded invocation = %#v", invocation)
	}
	input[ParametersKey].(map[string]interface{})["keyword"] = "mutated"
	if invocation.Parameters["keyword"] != "agents" {
		t.Fatalf("decoded parameters alias caller input: %#v", invocation.Parameters)
	}
}

func TestDecodeInvocationRejectsEndpointAndParameterEscalation(t *testing.T) {
	valid := map[string]interface{}{
		BaseURLKey: "https://api.example.test", MethodKey: "GET", PathKey: "/v1/search",
		ParametersKey:        map[string]interface{}{"keyword": "agents"},
		ParameterContractKey: []Parameter{{Name: "keyword", Location: "query", Required: true}},
		CredentialNameKey:    "API_TOKEN", CredentialParameterKey: "token",
	}
	for name, mutate := range map[string]func(map[string]interface{}){
		"scheme":      func(value map[string]interface{}) { value[BaseURLKey] = "http://api.example.test" },
		"origin path": func(value map[string]interface{}) { value[BaseURLKey] = "https://api.example.test/root" },
		"method":      func(value map[string]interface{}) { value[MethodKey] = "POST" },
		"path host":   func(value map[string]interface{}) { value[PathKey] = "//internal.test/secret" },
		"unknown": func(value map[string]interface{}) {
			value[ParametersKey] = map[string]interface{}{"keyword": "agents", "url": "https://internal.test"}
		},
		"credential collision": func(value map[string]interface{}) {
			value[ParameterContractKey] = []Parameter{{Name: "token", Location: "query", Required: true}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			copy := make(map[string]interface{}, len(valid))
			for key, value := range valid {
				copy[key] = value
			}
			mutate(copy)
			if _, err := DecodeInvocation(copy); err == nil {
				t.Fatalf("unsafe invocation was accepted: %#v", copy)
			}
		})
	}
}
