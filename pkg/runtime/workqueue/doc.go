// Package workqueue holds the work queue and its signals, outbox relay,
// worker scope, limits and scope pools: how work items are queued and
// claimed.
//
// Actions, conversation runs, external conversations and run workers all
// schedule their passes through it, so it sits below those features and
// depends only on pkg/models.
package workqueue
