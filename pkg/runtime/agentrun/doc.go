// Package agentrun holds the AgentRun lifecycle (creation, scheduling,
// commands, forks, retries, terminal close and wake) and the contract
// between a run and its loop: step and control contexts, final answers,
// StepHost and TurnRunnerBinding.
//
// The run loop, control tools and feature packages above it build on this
// contract, so it depends only on pkg/models and the activity and
// portfolio packages.
package agentrun
