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
	if cfg.Storage.Driver != "sqlite" || cfg.Storage.Path != "data/openseal.db" || cfg.Storage.ArtifactsPath != "data/artifacts" {
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

func TestLoadDaemonConfigRejectsRemovedWorkflowRuntimeKeys(t *testing.T) {
	for _, legacy := range []string{
		"workflowsDir: workflows\n",
		"triggers: {}\n",
		"webhook:\n  listenAddr: ':9090'\n",
	} {
		path := filepath.Join(t.TempDir(), "daemon.yaml")
		config := legacy + "api:\n  listenAddr: ':8080'\nstorage:\n  driver: sqlite\n  path: data/openseal.db\n  artifactsPath: data/artifacts\n"
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDaemonConfig(path); err == nil {
			t.Fatalf("removed daemon configuration was silently accepted:\n%s", legacy)
		}
	}
}
