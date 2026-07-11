//go:build unix

package clawhub

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func (m *InstallManager) lockWorkspace() (func(), error) {
	metadata := filepath.Join(m.workspace, ".clawhub")
	if err := os.MkdirAll(metadata, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(metadata, "lifecycle.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}
