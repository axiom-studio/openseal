//go:build linux

package client

// NewService is the bridge's background service on Linux: a systemd --user
// unit.
func NewService(options ServiceOptions, _ Store) ServiceManager {
	return systemdService{options}
}
