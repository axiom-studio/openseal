package executor

import (
	sdk "github.com/axiom-studio/skills.sdk/executor"
)

// Re-export SDK types for compatibility
type StepDefinition = sdk.StepDefinition
type StepResult = sdk.StepResult
type StreamUpdate = sdk.StreamUpdate
type StreamCallback = sdk.StreamCallback
type TemplateResolver = sdk.TemplateResolver
type ContextProvider = sdk.ContextProvider
type NodePosition = sdk.NodePosition

// NodeDefinition is an alias for SDK's GraphNode
// This allows cortex to use the same type for node definitions
type NodeDefinition = sdk.GraphNode

// StepExecutor interface from SDK
type StepExecutor = sdk.StepExecutor

// StreamingExecutor interface from SDK
type StreamingExecutor = sdk.StreamingExecutor

// GraphProvider is defined locally because cortex uses its own ExecutionGraph type
// This interface is implemented by Resolver for AI executors to discover connected tools