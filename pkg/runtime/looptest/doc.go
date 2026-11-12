// Package looptest is the run-loop test kit built on the exported runtime
// feature APIs: a scripted model, fake host tools, a store matrix, a fake
// clock, a crash store, and Fixture, which drives the real RunLoop with the
// production bindings. Runtime's loop scenario tests and runworker's pool
// scenario tests use it. The kit never reads run-level engine state
// (runtime's TestLoopTestkitHasNoCheckpointAccess checks these files too).
package looptest
