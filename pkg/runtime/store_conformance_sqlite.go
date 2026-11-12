package runtime

// The SQLite store satisfies these runtime-owned interfaces. The checks
// live in runtime because the store package cannot import it.
var _ channelActivationStore = (*SQLiteStore)(nil)
