package runbook

// Runbook schedules are portable artifacts, so their IANA timezone semantics
// must not depend on whether a host image happens to ship /usr/share/zoneinfo.
// Importing time/tzdata embeds the database in every binary that uses the
// runbook package and keeps validation and execution consistent.
import _ "time/tzdata"
