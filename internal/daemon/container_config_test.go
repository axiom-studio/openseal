package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
// The compose publish spec is the third value in the same arrangement, and it
// is the one that decides network exposure. Because the container now really
// does listen, an unqualified "8080:8080" would bind the HOST side to 0.0.0.0
// and put an API that authenticates nothing on every interface. Docker's
// forward is a DNAT rule traversed before the host INPUT chain, so a host
// firewall does not contain it.
//
// So the intended arrangement is three values that do not agree, on purpose:
//
//	docker/daemon.yaml        0.0.0.0:8080       (inside the container)
//	docker-compose.yml        127.0.0.1:8080:... (on the host)
//	internal/daemon/config.go 127.0.0.1:8080     (built-in default)
//
// Any "consistency" edit to one of them silently breaks something. These tests
// pin all three.

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

func TestComposePublishesTheAPIOnLoopbackOnly(t *testing.T) {
	path := filepath.Join("..", "..", "docker-compose.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var compose struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &compose); err != nil {
		t.Fatal(err)
	}

	service, ok := compose.Services["openseal"]
	if !ok {
		t.Fatalf("docker-compose.yml has no openseal service; services = %v", compose.Services)
	}
	if len(service.Ports) != 1 {
		t.Fatalf("openseal publishes %d ports, want exactly 1", len(service.Ports))
	}

	// A published port is host[:hostPort]:containerPort. Without a host address
	// Docker binds 0.0.0.0, which is the case this test exists to reject.
	published := service.Ports[0]
	if !strings.HasPrefix(published, "127.0.0.1:") {
		t.Fatalf("openseal publishes %q, want a 127.0.0.1-qualified spec — "+
			"an unqualified host port binds 0.0.0.0 and exposes an API that "+
			"authenticates nothing on every interface", published)
	}
	if !strings.HasSuffix(published, ":8080") {
		t.Fatalf("openseal publishes %q, want it to reach container port 8080", published)
	}
}
