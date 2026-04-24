// Package skillgrpc provides a gRPC client for external skills
package skillgrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	skillpb "github.com/axiom-studio/skills.sdk/grpc/skillpb"
	"github.com/axiom-studio/skills.sdk/executor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client connects to external skill gRPC services
type Client struct {
	mu      sync.RWMutex
	conn    *grpc.ClientConn
	client  skillpb.SkillServiceClient
	skillID string
	address string
}

// NewClient creates a new gRPC skill client
func NewClient(address string) *Client {
	return &Client{
		address: address,
	}
}

// Connect establishes connection to the skill service
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return nil
	}

	conn, err := grpc.DialContext(ctx, c.address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to skill at %s: %w", c.address, err)
	}

	c.conn = conn
	c.client = skillpb.NewSkillServiceClient(conn)

	// Get skill info
	health, err := c.client.Health(ctx, &skillpb.HealthRequest{})
	if err != nil {
		conn.Close()
		c.conn = nil
		c.client = nil
		return fmt.Errorf("health check failed: %w", err)
	}

	c.skillID = health.SkillId
	return nil
}

// Close closes the connection
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Execute executes a node on the skill service
func (c *Client) Execute(ctx context.Context, step *executor.StepDefinition, resolver executor.TemplateResolver) (*executor.StepResult, error) {
	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()

	if client == nil {
		return nil, fmt.Errorf("not connected to skill service")
	}

	// Serialize config
	config := make(map[string][]byte)
	for k, v := range step.Config {
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize config %s: %w", k, err)
		}
		config[k] = data
	}

	// Get input from resolver (previous node output)
	input := make(map[string][]byte)
	if stepInput := resolver.GetStepOutput(step.Id); stepInput != nil {
		if m, ok := stepInput.(map[string]interface{}); ok {
			for k, v := range m {
				data, err := json.Marshal(v)
				if err != nil {
					return nil, fmt.Errorf("failed to serialize input %s: %w", k, err)
				}
				input[k] = data
			}
		}
	}

	// Get bindings from resolver
	// Bindings are resolved before workflow execution and contain things like
	// database connection strings, API keys, etc.
	bindings := make(map[string][]byte)
	if ctxProvider, ok := resolver.(interface{ GetContextData() map[string]interface{} }); ok {
		ctxData := ctxProvider.GetContextData()
		if bindingsData, ok := ctxData["bindings"]; ok {
			if bindingsMap, ok := bindingsData.(map[string]interface{}); ok {
				for k, v := range bindingsMap {
					data, err := json.Marshal(v)
					if err != nil {
						return nil, fmt.Errorf("failed to serialize binding %s: %w", k, err)
					}
					bindings[k] = data
				}
			}
		}
	}

	// Build context
	execCtx := &skillpb.ExecutionContext{
		RunId:     "", // TODO: get from resolver
		AgentId:   "", // TODO: get from resolver
		Namespace: "", // TODO: get from resolver
		Variables: make(map[string]string),
	}

	// Build request
	req := &skillpb.ExecuteRequest{
		NodeId:   step.Id,
		NodeType: step.Type,
		Config:   config,
		Input:    input,
		Context:  execCtx,
		Bindings: bindings,
	}

	// Execute
	resp, err := client.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("execute failed: %w", err)
	}

	// Check for error
	if resp.Error != nil {
		return nil, fmt.Errorf("%s: %s", resp.Error.Type, resp.Error.Message)
	}

	// Deserialize output
	output := make(map[string]interface{})
	for k, v := range resp.Output {
		var val interface{}
		if err := json.Unmarshal(v, &val); err != nil {
			output[k] = string(v)
		} else {
			output[k] = val
		}
	}

	return &executor.StepResult{
		Output:   output,
		NextStep: resp.NextStep,
	}, nil
}

// GetNodeTypes returns the node types this skill provides
func (c *Client) GetNodeTypes(ctx context.Context) ([]string, error) {
	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()

	if client == nil {
		return nil, fmt.Errorf("not connected to skill service")
	}

	resp, err := client.GetNodeTypes(ctx, &skillpb.GetNodeTypesRequest{})
	if err != nil {
		return nil, err
	}

	return resp.NodeTypes, nil
}

// GetNodeSchema returns the schema for a node type
func (c *Client) GetNodeSchema(ctx context.Context, nodeType string) ([]byte, error) {
	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()

	if client == nil {
		return nil, fmt.Errorf("not connected to skill service")
	}

	resp, err := client.GetNodeSchema(ctx, &skillpb.GetNodeSchemaRequest{NodeType: nodeType})
	if err != nil {
		return nil, err
	}

	return resp.Schema, nil
}

// Health checks if the skill is healthy
func (c *Client) Health(ctx context.Context) (*HealthInfo, error) {
	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()

	if client == nil {
		return nil, fmt.Errorf("not connected to skill service")
	}

	resp, err := client.Health(ctx, &skillpb.HealthRequest{})
	if err != nil {
		return nil, err
	}

	return &HealthInfo{
		Healthy: resp.Healthy,
		SkillID: resp.SkillId,
		Version: resp.Version,
	}, nil
}

// SkillID returns the skill ID
func (c *Client) SkillID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.skillID
}

// Address returns the address
func (c *Client) Address() string {
	return c.address
}

// HealthInfo contains health check information
type HealthInfo struct {
	Healthy bool
	SkillID string
	Version string
}
