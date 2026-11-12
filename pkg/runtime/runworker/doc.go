// Package runworker runs agent work: AgentRunWorkerPool claims queued Runs
// of one scope, resolves each claim's turn binding (ResolveLoopBinding: the
// Agent's definition, activated Skills, pinned runbook and the run-loop
// seams) and drives it through runloop until the Run is at rest, renewing
// its lease, recovering accepted runs and parking for skill runtime
// maintenance. Between claims it repairs fork children and finalizes
// terminal Runs. AgentRunWorkerSupervisor keeps one pool per active scope.
package runworker
