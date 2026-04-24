package module

import (
	"context"
	"fmt"
	"sync"
)

type StepDefinition struct {
	Name   string                 `json:"name"`
	Type   string                 `json:"type"`
	Config map[string]interface{} `json:"config"`
}

type StepResult struct {
	Output   map[string]interface{}
	NextStep string
}

type TemplateResolver interface {
	ResolveString(template string) string
	ResolveMap(input map[string]interface{}) map[string]interface{}
	EvaluateCondition(condition string) bool
	SetVariable(name string, value interface{})
	GetStepOutput(stepName string) interface{}
	SetStepOutput(stepName string, output interface{})
}

type ContextAwareResolver interface {
	TemplateResolver
	GetBindings() map[string]interface{}
	GetTriggerData() map[string]interface{}
	GetPrevOutput() interface{}
	GetAllNodeOutputs() map[string]interface{}
	GetVariables() map[string]interface{}
	GetRunContext() map[string]interface{}
}

type StepExecutor interface {
	Type() string
	Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error)
}

type Registry struct {
	mu        sync.RWMutex
	executors map[string]StepExecutor
}

func NewRegistry() *Registry {
	return &Registry{
		executors: make(map[string]StepExecutor),
	}
}

func (r *Registry) Register(executor StepExecutor) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	stepType := executor.Type()
	if _, exists := r.executors[stepType]; exists {
		return fmt.Errorf("executor already registered: %s", stepType)
	}

	r.executors[stepType] = executor
	return nil
}

func (r *Registry) Get(stepType string) (StepExecutor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	exec, ok := r.executors[stepType]
	if !ok {
		return nil, fmt.Errorf("no executor for step type: %s", stepType)
	}
	return exec, nil
}

func (r *Registry) Has(stepType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.executors[stepType]
	return ok
}

func (r *Registry) ListTypes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]string, 0, len(r.executors))
	for t := range r.executors {
		types = append(types, t)
	}
	return types
}

type InitializableExecutor interface {
	StepExecutor
	Init(config map[string]interface{}) error
}

type ShutdownableExecutor interface {
	StepExecutor
	Shutdown(ctx context.Context) error
}

type CompositeExecutor interface {
	StepExecutor
	Steps() []StepExecutor
}

func (r *Registry) Shutdown(ctx context.Context) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var lastErr error
	for _, exec := range r.executors {
		if shutdownable, ok := exec.(ShutdownableExecutor); ok {
			if err := shutdownable.Shutdown(ctx); err != nil {
				lastErr = err
			}
		}
	}
	return lastErr
}

type FuncExecutor struct {
	stepType string
	fn       func(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error)
}

func NewFuncExecutor(stepType string, fn func(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error)) *FuncExecutor {
	return &FuncExecutor{stepType: stepType, fn: fn}
}

func (e *FuncExecutor) Type() string { return e.stepType }

func (e *FuncExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	return e.fn(ctx, step, resolver)
}
