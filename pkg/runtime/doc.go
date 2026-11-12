// Package runtime is the top of the runtime packages. It checks that the
// stores satisfy the interfaces the feature packages own. Its tests are the
// scenario tests that drive several feature packages together, the store
// tests that run against both backends through models' store interfaces,
// and single-feature tests that still lean on runtime's shared test
// helpers. Move a test to its feature package's external test package when
// it needs only that package and the *test fixture packages.
//
// Code that uses the runtime imports the package that owns each concept:
// pkg/models for records and store interfaces, pkg/runtime/store/postgres
// and pkg/runtime/store/sqlite for the stores, and the feature packages
// (conversation, runloop, runworker, ...) for behavior.
package runtime
