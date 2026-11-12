package runtime

// legacyEventSourceTables are dropped by migration 79, dependents first.
var legacyEventSourceTables = []string{"event_source_health", "event_source_subscriptions", "event_source_checkpoints"}
