package executor

import (
	sdkResolver "github.com/axiom-studio/skills.sdk/resolver"
)

// Resolver wraps the SDK's Resolver and adds graph access for AI executors.
type Resolver struct {
	*sdkResolver.Resolver
	graph *ExecutionGraph
}

// NewResolver creates a new resolver with the given config
func NewResolver(config sdkResolver.Config) *Resolver {
	return &Resolver{
		Resolver: sdkResolver.New(config),
	}
}

// SetGraph sets the execution graph for this resolver
func (r *Resolver) SetGraph(graph *ExecutionGraph) {
	r.graph = graph
}

// GetGraph returns the execution graph (implements GraphProvider)
func (r *Resolver) GetGraph() *ExecutionGraph {
	return r.graph
}

// simpleResolver provides backward compatibility for tests
// It uses the same field names as the old implementation
type simpleResolver struct {
	input       map[string]interface{}
	nodeOutputs map[string]interface{}
	variables   map[string]interface{}
	runMetadata map[string]interface{}
	bindings    map[string]interface{}
	graph       *ExecutionGraph
}

// newSimpleResolver creates a simpleResolver from field values
func newSimpleResolver(input, nodeOutputs, variables, runMetadata, bindings map[string]interface{}, graph *ExecutionGraph) *simpleResolver {
	return &simpleResolver{
		input:       input,
		nodeOutputs: nodeOutputs,
		variables:   variables,
		runMetadata: runMetadata,
		bindings:    bindings,
		graph:       graph,
	}
}

// GetContextData implements ContextProvider
func (r *simpleResolver) GetContextData() map[string]interface{} {
	// Get bindings from either the bindings field or input["bindings"]
	bindings := r.bindings
	if bindings == nil {
		bindings = getMap(r.input, "bindings")
	}

	return map[string]interface{}{
		"bindings":   bindings,
		"trigger":    getMap(r.input, "trigger"),
		"prev":       getMap(r.input, "prev"),
		"nodes":      r.nodeOutputs,
		"vars":       r.variables,
		"run":        r.runMetadata,
		"self":       getMap(r.input, "self"),
		"elapsed_ms": r.input["elapsed_ms"],
	}
}

// GetGraph implements GraphProvider
func (r *simpleResolver) GetGraph() *ExecutionGraph {
	return r.graph
}

// getMap helper for type assertions
func getMap(m map[string]interface{}, key string) map[string]interface{} {
	if m == nil {
		return nil
	}
	if v, ok := m[key]; ok {
		if m, ok := v.(map[string]interface{}); ok {
			return m
		}
	}
	return nil
}

// getElapsedMs helper for type assertions
func getElapsedMs(m map[string]interface{}) int64 {
	if m == nil {
		return 0
	}
	if v, ok := m["elapsed_ms"]; ok {
		switch val := v.(type) {
		case int64:
			return val
		case int:
			return int64(val)
		case float64:
			return int64(val)
		}
	}
	return 0
}

// ResolveString resolves {{}} templates
func (r *simpleResolver) ResolveString(template string) string {
	// Get bindings from either the bindings field or input["bindings"]
	bindings := r.bindings
	if bindings == nil {
		bindings = getMap(r.input, "bindings")
	}

	res := sdkResolver.New(sdkResolver.Config{
		Trigger:   getMap(r.input, "trigger"),
		Bindings:  bindings,
		Prev:      getMap(r.input, "prev"),
		Nodes:     r.nodeOutputs,
		Variables: r.variables,
		Run:       r.runMetadata,
		Self:      getMap(r.input, "self"),
	})
	return res.ResolveString(template)
}

// ResolveMap resolves all string values in a map
func (r *simpleResolver) ResolveMap(input map[string]interface{}) map[string]interface{} {
	// Get bindings from either the bindings field or input["bindings"]
	bindings := r.bindings
	if bindings == nil {
		bindings = getMap(r.input, "bindings")
	}

	res := sdkResolver.New(sdkResolver.Config{
		Trigger:   getMap(r.input, "trigger"),
		Bindings:  bindings,
		Prev:      getMap(r.input, "prev"),
		Nodes:     r.nodeOutputs,
		Variables: r.variables,
		Run:       r.runMetadata,
		Self:      getMap(r.input, "self"),
	})
	return res.ResolveMap(input)
}

// EvaluateCondition evaluates a condition string
func (r *simpleResolver) EvaluateCondition(condition string) bool {
	// Get bindings from either the bindings field or input["bindings"]
	bindings := r.bindings
	if bindings == nil {
		bindings = getMap(r.input, "bindings")
	}

	res := sdkResolver.New(sdkResolver.Config{
		Trigger:   getMap(r.input, "trigger"),
		Bindings:  bindings,
		Prev:      getMap(r.input, "prev"),
		Nodes:     r.nodeOutputs,
		Variables: r.variables,
	})
	return res.EvaluateCondition(r.ResolveString(condition))
}

// SetVariable sets a variable
func (r *simpleResolver) SetVariable(name string, value interface{}) {
	if r.variables == nil {
		r.variables = make(map[string]interface{})
	}
	r.variables[name] = value
}

// GetStepOutput returns the output of a step
func (r *simpleResolver) GetStepOutput(stepName string) interface{} {
	if r.nodeOutputs == nil {
		return nil
	}
	return r.nodeOutputs[stepName]
}

// SetStepOutput sets the output of a step
func (r *simpleResolver) SetStepOutput(stepName string, output interface{}) {
	if r.nodeOutputs == nil {
		r.nodeOutputs = make(map[string]interface{})
	}
	r.nodeOutputs[stepName] = output
}

// Ensure Resolver implements required interfaces
var (
	_ TemplateResolver = (*Resolver)(nil)
	_ ContextProvider  = (*Resolver)(nil)

	_ TemplateResolver = (*simpleResolver)(nil)
	_ ContextProvider  = (*simpleResolver)(nil)
)
