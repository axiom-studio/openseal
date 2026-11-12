// Package runtime is the top of the runtime packages. It checks that the
// stores satisfy the interfaces the feature packages own, and holds the
// scenario tests that drive several feature packages together.
//
// Code that uses the runtime imports the package that owns each concept:
// pkg/models for records and store interfaces, pkg/runtime/store/postgres
// and pkg/runtime/store/sqlite for the stores, and the feature packages
// (conversation, runloop, runworker, ...) for behavior.
package runtime
