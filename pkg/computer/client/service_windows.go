//go:build windows

package client

// NewService is the bridge's background service on Windows: a logon task.
func NewService(options ServiceOptions, _ Store) ServiceManager {
	return schtasksService{options}
}
