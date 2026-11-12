// Package sqlstore holds the SQL the Postgres and SQLite stores share: row
// decoding and scanning, queries and column lists both run, and the bodies
// of migrations both apply. Domain rules live in pkg/models; anything one
// store alone runs stays with that store.
package sqlstore
