//go:build darwin

package client

import "path/filepath"

// NewService is the bridge's background service on macOS: a LaunchAgent
// kept alive while store holds a connection.
func NewService(options ServiceOptions, store Store) ServiceManager {
	return launchdService{ServiceOptions: options, ConfigFile: filepath.Join(store.Dir, configFile)}
}
