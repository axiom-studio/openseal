package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"
)

// guardedPath is a route behind the bearer gate. It deliberately is NOT
// /api/v1/health, which is exempt so container liveness probes can reach it: a
// test that probed the one unauthenticated route would assert nothing about
// authentication. No store is wired here, so a request that clears the gate
// reaches the mux and 404s — which is the signal that it got past the guard.
const guardedPath = "/api/v1/__not_a_route"

// passedGate is what an authorized request looks like when the route does not
// exist: anything other than 401 means the credential was accepted.
func passedGate(code int) bool { return code != http.StatusUnauthorized }

func TestBearerTokenProtectsAPI(t *testing.T) {
	api := NewServer(nil, zap.NewNop().Sugar())
	api.SetBearerToken("desktop-secret")
	for _, tc := range []struct {
		name, authorization string
		wantBlocked         bool
	}{
		{"missing", "", true},
		{"raw token", "desktop-secret", true},
		{"wrong scheme", "Basic desktop-secret", true},
		{"wrong token", "Bearer desktop-secrex", true},
		{"empty token", "Bearer ", true},
		{"extra credentials", "Bearer desktop-secret other", true},
		{"valid", "Bearer desktop-secret", false},
		{"case insensitive scheme", "bearer desktop-secret", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, guardedPath, nil)
			if tc.authorization != "" {
				request.Header.Set("Authorization", tc.authorization)
			}
			response := httptest.NewRecorder()
			api.Handler().ServeHTTP(response, request)
			if blocked := !passedGate(response.Code); blocked != tc.wantBlocked {
				t.Fatalf("status = %d, blocked = %v, want blocked = %v", response.Code, blocked, tc.wantBlocked)
			}
			if tc.wantBlocked && response.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing authentication challenge")
			}
		})
	}
	t.Run("duplicate credentials", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, guardedPath, nil)
		request.Header.Add("Authorization", "Bearer desktop-secret")
		request.Header.Add("Authorization", "Bearer another-secret")
		response := httptest.NewRecorder()
		api.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("duplicate credentials status = %d", response.Code)
		}
	})
}

// TestHealthProbeReachableWithoutCredential covers the exemption that lets a
// container HEALTHCHECK work against an authenticated daemon. Without it, an
// operator who follows the daemon's own "Set it" warning gets a container that
// never reports healthy.
func TestHealthProbeReachableWithoutCredential(t *testing.T) {
	for _, tc := range []struct {
		name, token, authorization string
	}{
		{"token armed, probe sends nothing", "desktop-secret", ""},
		{"token armed, probe sends the token", "desktop-secret", "Bearer desktop-secret"},
		{"token armed, probe sends a wrong token", "desktop-secret", "Bearer nonsense"},
		{"no token configured", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := NewServer(nil, zap.NewNop().Sugar())
			api.SetBearerToken(tc.token)
			request := httptest.NewRequest(http.MethodGet, healthProbePath, nil)
			if tc.authorization != "" {
				request.Header.Set("Authorization", tc.authorization)
			}
			response := httptest.NewRecorder()
			api.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("health status = %d, want %d — a liveness probe carries no credential", response.Code, http.StatusOK)
			}
		})
	}
}

// TestHealthExemptionIsNarrow pins the blast radius of the exemption. It must
// admit exactly one method on exactly one path; anything broader would turn a
// liveness allowance into an authentication hole.
func TestHealthExemptionIsNarrow(t *testing.T) {
	api := NewServer(nil, zap.NewNop().Sugar())
	api.SetBearerToken("desktop-secret")
	for _, tc := range []struct{ name, method, path string }{
		{"other method on the health path", http.MethodPost, healthProbePath},
		{"trailing slash", http.MethodGet, healthProbePath + "/"},
		{"prefix only", http.MethodGet, healthProbePath + "z"},
		{"traversal through the health path", http.MethodGet, healthProbePath + "/../capabilities"},
		{"health as a suffix", http.MethodGet, "/api/v1/agents/health"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, nil)
			response := httptest.NewRecorder()
			api.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s status = %d, want %d — the exemption must not widen beyond GET %s",
					tc.method, tc.path, response.Code, http.StatusUnauthorized, healthProbePath)
			}
		})
	}
}

func TestEmptyBearerTokenPreservesStandaloneAccess(t *testing.T) {
	api := NewServer(nil, zap.NewNop().Sugar())
	api.SetBearerToken(" \t ")
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestServeEphemeralListenerEnforcesAuthentication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	api := NewServer(nil, zap.NewNop().Sugar())
	api.SetBearerToken("desktop-secret")
	done := make(chan error, 1)
	go func() { done <- api.Serve(listener) }()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	for _, authorized := range []bool{false, true} {
		// guardedPath, not the health route: health is exempt so that a
		// container probe can reach it, so probing it here would assert
		// nothing about the listener enforcing authentication.
		request, err := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+guardedPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorized {
			request.Header.Set("Authorization", "Bearer desktop-secret")
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if blocked := !passedGate(response.StatusCode); blocked == authorized {
			t.Fatalf("authorized = %v but status = %d", authorized, response.StatusCode)
		}
	}
	client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := api.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve returned %v", err)
	}
}
