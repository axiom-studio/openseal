// Package storetest holds test support shared by the SQL stores: fixtures
// built through the store interfaces in pkg/models, the bodies of suites each
// backend runs against itself, and migration fixtures that lay out or read
// legacy rows with raw SQL on a connection the test owns (PostgresSchema, or
// a *sql.DB the caller passes in). It never opens a store and never touches a
// store's own connection; the caller passes the store in.
package storetest
