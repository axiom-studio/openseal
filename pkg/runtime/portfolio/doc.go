// Package portfolio holds objectives, spaces and run dependencies:
// Service creates and updates objectives and the agent runs
// under them, SpaceService manages spaces, their members and access, and
// DependencyCoordinator records which runs wait on which and resolves
// those waits. Look here for how work is grouped and ordered; the records
// themselves live in pkg/models.
package portfolio
