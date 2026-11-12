// Package callback handles skill-owned provider callbacks. The
// Registry records which skill adapter a callback registration is
// pinned to and moves it through its lifecycle; the IngressService
// receives public callback requests, verifies them through that adapter,
// stores a receipt and hands each event to its subscribed consumer; and the
// EventWorker claims receipts that are still pending, dispatches
// them again and backs off or fails them when dispatch keeps failing.
package callback
