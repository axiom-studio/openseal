package commands

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/internal/daemon"
)

// The bearer-token gate in internal/server is covered by auth_test.go, but that
// test arms it by calling SetBearerToken directly. Nothing there establishes
// that the daemon arms it — delete the SetBearerToken call in daemonCmd and
// every one of those tests still passes while every route silently becomes
// unauthenticated.
//
// These tests close that gap by running the daemon's own construction path in a
// subprocess and driving it over HTTP, so the assertion is about the daemon's
// observable behaviour rather than about a function being reachable.
//
// They are deliberately NOT behind the `integration` build tag: a guard that
// `make test` does not run would not have caught the deletion it exists to
// catch.

// TestDaemonAuthHelperProcess is the subprocess entry point. It returns
// immediately unless the parent marked it, so it is inert in a normal run.
func TestDaemonAuthHelperProcess(t *testing.T) {
	if os.Getenv("OPENSEAL_AUTH_HELPER") != "1" {
		return
	}
	daemonCmd([]string{"--config", os.Getenv("OPENSEAL_AUTH_CONFIG")})
}

func TestDaemonEnforcesBearerTokenFromEnvironment(t *testing.T) {
	base, stop := startDaemonForAuth(t, "test-secret-123")
	defer stop()

	for _, testCase := range []struct {
		name        string
		header      string
		wantBlocked bool
	}{
		{name: "no authorization header", header: "", wantBlocked: true},
		{name: "wrong token", header: "Bearer wrong-token", wantBlocked: true},
		{name: "correct token", header: "Bearer test-secret-123", wantBlocked: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, headers := getGuarded(t, base, testCase.header)
			blocked := status == http.StatusUnauthorized
			if blocked != testCase.wantBlocked {
				t.Fatalf("GET %s with %q = %d, blocked = %v, want blocked = %v — "+
					"the daemon must apply OPENSEAL_API_TOKEN to the API server",
					guardedRoute, testCase.header, status, blocked, testCase.wantBlocked)
			}
			if testCase.wantBlocked {
				if challenge := headers.Get("WWW-Authenticate"); challenge == "" {
					t.Fatalf("401 carried no WWW-Authenticate header")
				}
			}
		})
	}
}

// TestDaemonHealthProbeWorksWithTokenArmed is the regression this pair of tests
// was missing: the daemon can have authentication correctly armed and still be
// unusable, because a container's HEALTHCHECK cannot send a credential and the
// orchestrator marks the container unhealthy.
func TestDaemonHealthProbeWorksWithTokenArmed(t *testing.T) {
	base, stop := startDaemonForAuth(t, "test-secret-123")
	defer stop()

	if status, _ := getHealth(t, base, ""); status != http.StatusOK {
		t.Fatalf("GET /api/v1/health with no credential = %d, want %d — "+
			"HEALTHCHECK sends no Authorization header, so an armed token "+
			"would leave every container permanently unhealthy",
			status, http.StatusOK)
	}
}

// The unset case is asserted too, so the backward-compatible default cannot
// change silently: every existing container and CLI deployment relies on the
// daemon serving without a token.
func TestDaemonWithoutTokenLeavesRoutesOpen(t *testing.T) {
	base, stop := startDaemonForAuth(t, "")
	defer stop()

	if status, _ := getHealth(t, base, ""); status != http.StatusOK {
		t.Fatalf("GET /api/v1/health without OPENSEAL_API_TOKEN = %d, want %d — "+
			"an unset token must leave routes open for existing deployments",
			status, http.StatusOK)
	}
}

// startDaemonForAuth runs daemonCmd in a subprocess with the given token and
// returns its base URL plus a stop function.
func startDaemonForAuth(t *testing.T, token string) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	port := freeLoopbackPort(t)
	configPath := filepath.Join(dir, "daemon.yaml")

	config := daemon.DefaultDaemonConfig()
	config.API.ListenAddr = fmt.Sprintf("127.0.0.1:%d", port)
	config.Storage.Path = filepath.Join(dir, "kernel.db")
	config.Storage.ArtifactsPath = filepath.Join(dir, "artifacts")
	if err := daemon.WriteDaemonConfig(configPath, config); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(os.Args[0], "-test.run=TestDaemonAuthHelperProcess")
	command.Env = append(os.Environ(),
		"OPENSEAL_AUTH_HELPER=1",
		"OPENSEAL_AUTH_CONFIG="+configPath,
		"OPENSEAL_API_TOKEN="+token,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	}

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForDaemon(t, base, stop)
	return base, stop
}

// waitForDaemon blocks until the port answers. Any HTTP response means the
// server is up — including a 401, which is the expected answer in the armed
// case and must not be mistaken for the daemon not having started.
func waitForDaemon(t *testing.T, base string, stop func()) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(base + "/api/v1/health")
		if err == nil {
			_ = response.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	t.Fatalf("daemon did not start listening on %s", base)
}

// guardedRoute is a route behind the bearer gate, used to assert that the
// daemon armed authentication. It is deliberately NOT /api/v1/health: that
// route is exempt so a container's HEALTHCHECK can reach an authenticated
// daemon, so probing it would assert nothing about the token being applied.
// No such route exists, which is the point — clearing the gate reaches the mux
// and 404s, and only a 401 means the credential was rejected.
const guardedRoute = "/api/v1/__not_a_route"

// getGuarded returns the status of a request to a token-protected route.
func getGuarded(t *testing.T, base, authorization string) (int, http.Header) {
	t.Helper()
	return get(t, base+guardedRoute, authorization)
}

// getHealth probes the liveness route, which is exempt from the bearer gate.
func getHealth(t *testing.T, base, authorization string) (int, http.Header) {
	t.Helper()
	return get(t, base+"/api/v1/health", authorization)
}

func get(t *testing.T, url, authorization string) (int, http.Header) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode, response.Header
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
