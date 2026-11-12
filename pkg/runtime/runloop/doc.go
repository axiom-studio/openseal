// Package runloop drives a claimed run: RunLoop.Drive takes a run step by
// step, preparing each step's context (steering, follow-ups, the catalog
// model request), producing it, and running its tool calls through the
// claim's tool executor in tool order: control tools, host tools and
// governed actions, which park the run on approvals, cards and
// maintenance. Around each step it enforces budgets, step durations, repeat
// guards and failure guards, resumes interrupted steps, delivers final
// answers, runs pinned routine scripts and cuts old runs over to the loop.
package runloop
