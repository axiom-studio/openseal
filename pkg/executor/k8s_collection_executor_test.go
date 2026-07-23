package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type nilCollectionK8sClient struct {
	K8sClient
}

func (nilCollectionK8sClient) ListResources(context.Context, int, string, string, string, string) ([]map[string]interface{}, error) {
	return nil, nil
}

func (nilCollectionK8sClient) ListEvents(context.Context, int, string, string, string) ([]map[string]interface{}, error) {
	return nil, nil
}

func TestK8sCollectionExecutorsReturnArraysForEmptyResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		executor StepExecutor
		config   map[string]interface{}
		property string
	}{
		{
			name:     "resources",
			executor: NewK8sListExecutor(nilCollectionK8sClient{}),
			config:   map[string]interface{}{"kind": "Pod", "namespace": "default"},
			property: "list",
		},
		{
			name:     "events",
			executor: NewK8sEventsExecutor(nilCollectionK8sClient{}),
			config:   map[string]interface{}{"namespace": "default"},
			property: "items",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			result, err := test.executor.Execute(t.Context(), &StepDefinition{Config: test.config}, &k8sTestResolver{})
			if err != nil {
				t.Fatalf("execute empty %s collection: %v", test.name, err)
			}
			items, ok := result.Output[test.property].([]map[string]interface{})
			if !ok {
				t.Fatalf("%s output has type %T, want []map[string]interface{}", test.property, result.Output[test.property])
			}
			if items == nil || len(items) != 0 {
				t.Fatalf("%s output = %#v, want a non-nil empty collection", test.property, items)
			}
			if result.Output["count"] != 0 {
				t.Fatalf("count = %#v, want 0", result.Output["count"])
			}

			encoded, err := json.Marshal(result.Output)
			if err != nil {
				t.Fatalf("marshal output: %v", err)
			}
			if strings.Contains(string(encoded), `"`+test.property+`":null`) {
				t.Fatalf("output violates array schema: %s", encoded)
			}
		})
	}
}
