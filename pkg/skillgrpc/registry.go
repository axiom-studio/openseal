// Package skillgrpc provides a gRPC client for external skills
package skillgrpc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/axiom-studio/skills.sdk/executor"
)

// Registry manages connections to external skill services
type Registry struct {
	mu      sync.RWMutex
	clients map[string]*Client // skillID -> client
	byType  map[string]string // nodeType -> skillID
}

// NewRegistry creates a new skill registry
func NewRegistry() *Registry {
	return &Registry{
		clients: make(map[string]*Client),
		byType:  make(map[string]string),
	}
}

// Register adds a skill to the registry
func (r *Registry) Register(ctx context.Context, address string) (*Client, error) {
	client := NewClient(address)
	if err := client.Connect(ctx); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Get node types
	types, err := client.GetNodeTypes(ctx)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to get node types: %w", err)
	}

	// Register
	skillID := client.SkillID()
	r.clients[skillID] = client

	// Map node types to skill
	for _, t := range types {
		r.byType[t] = skillID
	}

	return client, nil
}

// Unregister removes a skill from the registry
func (r *Registry) Unregister(skillID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	client, ok := r.clients[skillID]
	if !ok {
		return nil
	}

	// Remove type mappings
	for t, sid := range r.byType {
		if sid == skillID {
			delete(r.byType, t)
		}
	}

	delete(r.clients, skillID)
	return client.Close()
}

// GetClient returns the client for a skill ID
func (r *Registry) GetClient(skillID string) *Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clients[skillID]
}

// GetClientForType returns the client that handles a node type
func (r *Registry) GetClientForType(nodeType string) *Client {
	r.mu.RLock()
	defer r.mu.RUnlock()

	skillID, ok := r.byType[nodeType]
	if !ok {
		return nil
	}
	return r.clients[skillID]
}

// HasType checks if a node type is registered
func (r *Registry) HasType(nodeType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byType[nodeType]
	return ok
}

func (r *Registry) GetSkillTypes(skillID string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var types []string
	for t, sid := range r.byType {
		if sid == skillID {
			types = append(types, t)
		}
	}
	return types
}

// ListSkills returns all registered skill IDs
func (r *Registry) ListSkills() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	skills := make([]string, 0, len(r.clients))
	for id := range r.clients {
		skills = append(skills, id)
	}
	return skills
}

// ListTypes returns all registered node types
func (r *Registry) ListTypes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]string, 0, len(r.byType))
	for t := range r.byType {
		types = append(types, t)
	}
	return types
}

// Close closes all connections
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var errs []error
	for _, client := range r.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	r.clients = make(map[string]*Client)
	r.byType = make(map[string]string)

	if len(errs) > 0 {
		return fmt.Errorf("errors closing connections: %v", errs)
	}
	return nil
}

// GRPCExecutor wraps a gRPC client as an executor
type GRPCExecutor struct {
	client *Client
}

// NewGRPCExecutor creates a new gRPC executor
func NewGRPCExecutor(client *Client) *GRPCExecutor {
	return &GRPCExecutor{client: client}
}

// Execute implements executor.StepExecutor
func (e *GRPCExecutor) Execute(ctx context.Context, step *executor.StepDefinition, resolver executor.TemplateResolver) (*executor.StepResult, error) {
	return e.client.Execute(ctx, step, resolver)
}

// Type returns the executor type
func (e *GRPCExecutor) Type() string {
	return "grpc"
}

// HealthCheck periodically checks skill health
func (r *Registry) HealthCheck(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.checkHealth(ctx)
		}
	}
}

func (r *Registry) checkHealth(ctx context.Context) {
	r.mu.RLock()
	clients := make([]*Client, 0, len(r.clients))
	for _, c := range r.clients {
		clients = append(clients, c)
	}
	r.mu.RUnlock()

	for _, client := range clients {
		_, err := client.Health(ctx)
		if err != nil {
			// TODO: log warning, maybe reconnect
		}
	}
}
