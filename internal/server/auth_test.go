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

func TestBearerTokenProtectsAPI(t *testing.T) {
	api := NewServer(nil, zap.NewNop().Sugar())
	api.SetBearerToken("desktop-secret")
	for _, tc := range []struct {
		name, authorization string
		want                int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"raw token", "desktop-secret", http.StatusUnauthorized},
		{"wrong scheme", "Basic desktop-secret", http.StatusUnauthorized},
		{"wrong token", "Bearer desktop-secrex", http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"extra credentials", "Bearer desktop-secret other", http.StatusUnauthorized},
		{"valid", "Bearer desktop-secret", http.StatusOK},
		{"case insensitive scheme", "bearer desktop-secret", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
			if tc.authorization != "" {
				request.Header.Set("Authorization", tc.authorization)
			}
			response := httptest.NewRecorder()
			api.Handler().ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d", response.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing authentication challenge")
			}
		})
	}
	t.Run("duplicate credentials", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		request.Header.Add("Authorization", "Bearer desktop-secret")
		request.Header.Add("Authorization", "Bearer another-secret")
		response := httptest.NewRecorder()
		api.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("duplicate credentials status = %d", response.Code)
		}
	})
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
		request, err := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+"/api/v1/health", nil)
		if err != nil {
			t.Fatal(err)
		}
		want := http.StatusUnauthorized
		if authorized {
			request.Header.Set("Authorization", "Bearer desktop-secret")
			want = http.StatusOK
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("status = %d, want %d", response.StatusCode, want)
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
