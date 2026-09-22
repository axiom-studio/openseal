package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The container configuration and the built-in default deliberately disagree
// about the listen address, and each half of that disagreement is load-bearing.
//
// In a container, a published port arrives on the container's own interface,
// never on its loopback. A daemon bound to 127.0.0.1 there is reachable only
// from inside its own container, so `-p 8080:8080` forwards to nothing and the
// image is unusable. Run directly on a host, the same loopback bind is the
// safe default: it keeps an unauthenticated API off the network until an
// operator deliberately exposes it.
//
// The two values are one line apart in effect but opposite in intent, so a
// well-meaning "consistency" edit to either one silently breaks something.
// These tests pin both.

func TestContainerDaemonConfigBindsAllInterfaces(t *testing.T) {
	path := filepath.Join("..", "..", "docker", "daemon.yaml")
	// LoadDaemonConfig writes a default config when the file is missing. Assert
	// it exists first so this test can only ever read the committed file, never
	// create one and then assert against its own output.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("container config must exist and be readable: %v", err)
	}
	cfg, err := LoadDaemonConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	host, port, found := strings.Cut(cfg.API.ListenAddr, ":")
	if !found {
		t.Fatalf("container listenAddr = %q, want host:port", cfg.API.ListenAddr)
	}
	if host != "0.0.0.0" {
		t.Fatalf("container listenAddr host = %q, want 0.0.0.0 — a loopback bind makes the published port unreachable", host)
	}
	if port != "8080" {
		t.Fatalf("container listenAddr port = %q, want 8080 to match the Dockerfile EXPOSE and health check", port)
	}
}

func TestDefaultDaemonConfigStaysOnLoopback(t *testing.T) {
	if got := DefaultDaemonConfig().API.ListenAddr; got != "127.0.0.1:8080" {
		t.Fatalf("default listenAddr = %q, want 127.0.0.1:8080 — the non-container default must not widen exposure", got)
	}
}
