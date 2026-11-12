package runtime

// legacyEventWaitTables are dropped by migration 81, dependents first.
// run_event_retention_scopes only ever existed in SQLite.
var legacyEventWaitTables = []string{
	"run_event_notifications", "run_event_consumptions", "run_event_wait_scopes", "run_event_waits",
	"run_event_retention_cursor", "run_event_retention_scopes", "run_event_inbox",
}
