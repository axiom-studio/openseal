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

// NodeDefinition is an alias for the SDK graph node shared by the engine and skills.
type NodeDefinition = sdk.GraphNode

// StepExecutor interface from SDK
type StepExecutor = sdk.StepExecutor

// StreamingExecutor interface from SDK
type StreamingExecutor = sdk.StreamingExecutor

// GraphProvider is implemented by Resolver for AI executors to discover connected tools.
