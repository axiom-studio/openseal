// Package routine holds routines and runbooks: the routine actions an agent
// calls to create, change, pause and test scheduled and event routines, the
// grants a routine brief carries, routine scripts, and the activation
// service that starts each occurrence. RoutineEventMatcher and
// RoutineEventDispatcher match incoming events to event routines and start
// their batches, RunbookScheduler fires scheduled ones, and
// RunbookStepProducer runs a runbook turn by turn with its execution audit
// and trace.
package routine
