package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// OpenKernelStore opens the standalone durable kernel store. Relative paths
// are anchored to the daemon configuration directory rather than the caller's
// current working directory.
func OpenKernelStore(config StorageConfig, configDir string) (*runtime.SQLiteStore, string, error) {
	if config.Driver != "sqlite" {
		return nil, "", fmt.Errorf("unsupported storage driver %q", config.Driver)
	}
	path := filepath.Clean(config.Path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDir, path)
	}
	if err := ensureParentDirectory(path); err != nil {
		return nil, "", err
	}
	store, err := runtime.NewSQLiteStore(path)
	if err != nil {
		return nil, "", err
	}
	return store, path, nil
}

func ensureParentDirectory(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o750)
}
