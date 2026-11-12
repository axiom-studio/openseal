// Package models holds the runtime's records and the rules that belong to
// them: what a run, conversation, action, session or routine event is
// (Scope, AgentRun, Conversation, ActionCall, SessionEntry, …), how each is
// validated, cloned and compared, the sentinel errors they report, and the
// store interfaces a backend must provide to keep them. It has no SQL, no
// locks and no services, and it imports nothing from pkg/runtime, so the
// stores and every runtime feature can depend on it. Look here for what a
// record is; look in pkg/runtime and its packages for what is done with it.
package models
