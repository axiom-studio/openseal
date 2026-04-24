// Package executor contains step executors for the agent runtime
package executor

import (
	"context"
	"fmt"

	skillgrpc "github.com/axiom-studio/openseal/pkg/skillgrpc"
)

// GRPCSkillExecutor wraps a gRPC skill client as a StepExecutor
// This allows external skills to be used seamlessly in workflows
type GRPCSkillExecutor struct {
	client   *skillgrpc.Client
	nodeType string
}

// NewGRPCSkillExecutor creates a new gRPC skill executor
func NewGRPCSkillExecutor(client *skillgrpc.Client, nodeType string) *GRPCSkillExecutor {
	return &GRPCSkillExecutor{
		client:   client,
		nodeType: nodeType,
	}
}

// Type returns the node type this executor handles
func (e *GRPCSkillExecutor) Type() string {
	return e.nodeType
}

// Execute executes the step via gRPC
func (e *GRPCSkillExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	// Convert to SDK StepDefinition for gRPC call
	sdkStep := &StepDefinition{
		Id:     step.Id,
		Name:   step.Name,
		Type:   step.Type,
		Config: step.Config,
	}

	// Call the gRPC skill
	result, err := e.client.Execute(ctx, sdkStep, resolver)
	if err != nil {
		return nil, fmt.Errorf("gRPC skill execution failed: %w", err)
	}

	// Convert result back
	return &StepResult{
		Output:   result.Output,
		NextStep: result.NextStep,
	}, nil
}

// GRPCSkillRegistry manages gRPC skill executors
type GRPCSkillRegistry struct {
	grpcRegistry *skillgrpc.Registry
	execRegistry *Registry
}

// NewGRPCSkillRegistry creates a new gRPC skill registry
func NewGRPCSkillRegistry(execRegistry *Registry) *GRPCSkillRegistry {
	return &GRPCSkillRegistry{
		grpcRegistry: skillgrpc.NewRegistry(),
		execRegistry: execRegistry,
	}
}

// RegisterSkill connects to a gRPC skill and registers its executors
func (r *GRPCSkillRegistry) RegisterSkill(ctx context.Context, address string) error {
	client, err := r.grpcRegistry.Register(ctx, address)
	if err != nil {
		return fmt.Errorf("failed to register gRPC skill: %w", err)
	}

	// Get node types from the skill
	types, err := client.GetNodeTypes(ctx)
	if err != nil {
		r.grpcRegistry.Unregister(client.SkillID())
		return fmt.Errorf("failed to get node types: %w", err)
	}

	// Register each node type as an executor
	for _, nodeType := range types {
		executor := NewGRPCSkillExecutor(client, nodeType)
		r.execRegistry.Register(executor)
	}

	return nil
}

// UnregisterSkill removes a skill and its executors
func (r *GRPCSkillRegistry) UnregisterSkill(skillID string) error {
	types := r.grpcRegistry.GetSkillTypes(skillID)
	for _, t := range types {
		r.execRegistry.Unregister(t)
	}
	return r.grpcRegistry.Unregister(skillID)
}

// GetGRPCRegistry returns the underlying gRPC registry
func (r *GRPCSkillRegistry) GetGRPCRegistry() *skillgrpc.Registry {
	return r.grpcRegistry
}

// Close closes all gRPC connections
func (r *GRPCSkillRegistry) Close() error {
	return r.grpcRegistry.Close()
}

// ListSkills returns all registered skill IDs
func (r *GRPCSkillRegistry) ListSkills() []string {
	return r.grpcRegistry.ListSkills()
}

// ListTypes returns all registered node types from gRPC skills
func (r *GRPCSkillRegistry) ListTypes() []string {
	return r.grpcRegistry.ListTypes()
}