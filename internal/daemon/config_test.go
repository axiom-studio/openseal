package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDaemonConfigDefaultsToDurableSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	cfg, err := LoadDaemonConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Driver != "sqlite" || cfg.Storage.Path != "data/openseal.db" {
		t.Fatalf("storage = %#v", cfg.Storage)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("default config was not written")
	}
}

func TestDaemonConfigRejectsEphemeralOrUnknownStorage(t *testing.T) {
	cfg := DefaultDaemonConfig()
	cfg.Storage.Driver = "memory"
	if err := cfg.Validate(); err == nil {
		t.Fatal("memory storage should not be accepted by the durable daemon")
	}
}
