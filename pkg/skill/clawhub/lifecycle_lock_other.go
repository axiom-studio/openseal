//go:build !unix

package clawhub

// The in-process mutex remains authoritative on platforms without Unix file
// locks. Multi-replica shared-volume hosting is supported on Unix runtimes.
func (m *InstallManager) lockWorkspace() (func(), error) { return func() {}, nil }
