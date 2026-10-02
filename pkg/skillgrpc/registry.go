// Package skillgrpc provides a gRPC client for external skills
package skillgrpc

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/axiom-studio/skills.sdk/executor"
)

// Registry manages connections to external skill services
type Registry struct {
	mu           sync.RWMutex
	connect      func(context.Context, string) (*Client, error)
	clients      map[string]*Client             // authority-scoped registration key -> client
	retired      map[*Client]struct{}           // draining clients retained for explicit shutdown
	typesBySkill map[string]map[string]struct{} // registration key -> node types
	skillsByType map[string]map[string]struct{} // node type -> registration keys
}

// NewRegistry creates a new skill registry
func NewRegistry() *Registry {
	return &Registry{
		connect:      connectSkillClient,
		clients:      make(map[string]*Client),
		retired:      make(map[*Client]struct{}),
		typesBySkill: make(map[string]map[string]struct{}),
		skillsByType: make(map[string]map[string]struct{}),
	}
}

// Register adds a skill to the registry
func (r *Registry) Register(ctx context.Context, address string) (*Client, error) {
	return r.register(ctx, "", address, nil)
}

// RegisterAs adds a Skill endpoint under an authority-scoped registration key.
// A trusted discovery boundary supplies the key independently of the identity
// reported by the remote Skill. Callers can therefore isolate equal Skill IDs
// by tenant, version, environment, or another host authority dimension.
func (r *Registry) RegisterAs(ctx context.Context, key, address string) (*Client, error) {
	if key == "" {
		return nil, fmt.Errorf("skill registration key is required")
	}
	return r.register(ctx, key, address, nil)
}

// RegisterAsVerified publishes an authority-scoped endpoint only after its
// health response confirms the exact advertised Skill identity and version.
// Failed verification leaves any existing registration and type mappings intact.
func (r *Registry) RegisterAsVerified(ctx context.Context, key, address, expectedSkillID, expectedVersion string) (*Client, error) {
	if key == "" || expectedSkillID == "" || expectedVersion == "" {
		return nil, fmt.Errorf("skill registration key, identity, and version are required")
	}
	return r.register(ctx, key, address, func(client *Client) error {
		health, err := client.Health(ctx)
		if err != nil {
			return fmt.Errorf("failed to verify skill health: %w", err)
		}
		if health == nil || !health.Healthy || health.SkillID != expectedSkillID ||
			health.Version != expectedVersion || client.SkillID() != expectedSkillID {
			return fmt.Errorf("skill health does not match the healthy advertised identity and version")
		}
		return nil
	})
}

func (r *Registry) register(ctx context.Context, key, address string, verify func(*Client) error) (*Client, error) {
	client, err := r.connect(ctx, address)
	if err != nil {
		return nil, err
	}

	types, err := client.GetNodeTypes(ctx)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to get node types: %w", err)
	}
	if verify != nil {
		if err := verify(client); err != nil {
			_ = client.Close()
			return nil, err
		}
	}

	if key == "" {
		key = client.SkillID()
	}
	r.mu.Lock()
	previous := r.clients[key]
	var retirement clientCloseAction
	if previous != nil && previous != client {
		retirement = r.prepareRetirementLocked(previous)
	}
	r.removeMappingsLocked(key)
	r.clients[key] = client
	r.typesBySkill[key] = make(map[string]struct{}, len(types))
	for _, t := range types {
		if t == "" {
			continue
		}
		r.typesBySkill[key][t] = struct{}{}
		owners := r.skillsByType[t]
		if owners == nil {
			owners = make(map[string]struct{})
			r.skillsByType[t] = owners
		}
		owners[key] = struct{}{}
	}
	r.mu.Unlock()
	_ = retirement.close()

	return client, nil
}

func connectSkillClient(ctx context.Context, address string) (*Client, error) {
	client := NewClient(address)
	if err := client.Connect(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

// Unregister removes a skill from the registry
func (r *Registry) Unregister(key string) error {
	r.mu.Lock()

	client, ok := r.clients[key]
	if !ok {
		r.mu.Unlock()
		return nil
	}

	retirement := r.prepareRetirementLocked(client)
	r.removeMappingsLocked(key)
	delete(r.clients, key)
	r.mu.Unlock()
	return retirement.close()
}

// prepareRetirementLocked prevents new acquisitions before a dispatch mapping
// changes. The close action and its drain callback run after locks are released.
func (r *Registry) prepareRetirementLocked(client *Client) clientCloseAction {
	if r.retired == nil {
		r.retired = make(map[*Client]struct{})
	}
	r.retired[client] = struct{}{}
	return client.prepareRetirement(func() {
		r.mu.Lock()
		delete(r.retired, client)
		r.mu.Unlock()
	})
}

// GetClient returns the client for a skill ID
func (r *Registry) GetClient(key string) *Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clients[key]
}

// GetClientForType returns the client that handles a node type
func (r *Registry) GetClientForType(nodeType string) *Client {
	r.mu.RLock()
	defer r.mu.RUnlock()

	owners := r.skillsByType[nodeType]
	if len(owners) != 1 {
		return nil
	}
	for key := range owners {
		return r.clients[key]
	}
	return nil
}

// GetClientForSkillType resolves a node type within one exact authority-scoped
// registration. This is the safe lookup when multiple Skills expose the same
// node type; ambiguous global type lookup deliberately fails closed.
func (r *Registry) GetClientForSkillType(key, nodeType string) *Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.typesBySkill[key][nodeType]; !ok {
		return nil
	}
	return r.clients[key]
}

// HasType checks if a node type is registered
func (r *Registry) HasType(nodeType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.skillsByType[nodeType]) > 0
}

func (r *Registry) GetSkillTypes(key string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]string, 0, len(r.typesBySkill[key]))
	for nodeType := range r.typesBySkill[key] {
		types = append(types, nodeType)
	}
	sort.Strings(types)
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
	sort.Strings(skills)
	return skills
}

// ListTypes returns all registered node types
func (r *Registry) ListTypes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]string, 0, len(r.skillsByType))
	for t := range r.skillsByType {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// Close closes all connections
func (r *Registry) Close() error {
	r.mu.Lock()
	clients := make(map[*Client]struct{}, len(r.clients)+len(r.retired))
	for _, client := range r.clients {
		clients[client] = struct{}{}
	}
	for client := range r.retired {
		clients[client] = struct{}{}
	}
	r.clients = make(map[string]*Client)
	r.retired = make(map[*Client]struct{})
	r.typesBySkill = make(map[string]map[string]struct{})
	r.skillsByType = make(map[string]map[string]struct{})
	r.mu.Unlock()

	var errs []error
	for client := range clients {
		if err := client.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors closing connections: %v", errs)
	}
	return nil
}

func (r *Registry) removeMappingsLocked(key string) {
	for nodeType := range r.typesBySkill[key] {
		owners := r.skillsByType[nodeType]
		delete(owners, key)
		if len(owners) == 0 {
			delete(r.skillsByType, nodeType)
		}
	}
	delete(r.typesBySkill, key)
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
