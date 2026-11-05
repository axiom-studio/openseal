package commands

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestParseScope(t *testing.T) {
	scope, err := parseScope("workspace:research")
	if err != nil {
		t.Fatal(err)
	}
	if scope.Kind != "workspace" || scope.ID != "research" {
		t.Fatalf("scope = %#v", scope)
	}
	for _, value := range []string{"", "local", ":default", "local:"} {
		if _, err := parseScope(value); err == nil {
			t.Fatalf("parseScope(%q) succeeded", value)
		}
	}
}

func TestRequestHeaderFlagsAcceptHostSelectorsAndRejectProtocolHeaders(t *testing.T) {
	headers := make(requestHeaderFlags)
	if err := headers.Set("X-Workspace-Scope=tenant:7"); err != nil {
		t.Fatal(err)
	}
	if err := headers.Set("X-Workspace-Scope=tenant:8"); err != nil {
		t.Fatal(err)
	}
	values := http.Header(headers).Values("X-Workspace-Scope")
	if len(values) != 2 || values[0] != "tenant:7" || values[1] != "tenant:8" {
		t.Fatalf("headers = %#v", headers)
	}
	for _, value := range []string{"", "missing-value=", "bad header=value", "X-Test=bad\nvalue", "Content-Type=text/plain", "Idempotency-Key=override"} {
		if err := headers.Set(value); err == nil {
			t.Fatalf("Set(%q) succeeded", value)
		}
	}
}

func TestParseOwnerAcceptsOnlyAgentOrTeam(t *testing.T) {
	for _, value := range []string{"agent:operator", "team:platform"} {
		owner, err := parseOwner(value)
		if err != nil {
			t.Fatalf("parseOwner(%q): %v", value, err)
		}
		if owner.ID == "" || (owner.Type != runtime.OwnerTypeAgent && owner.Type != runtime.OwnerTypeTeam) {
			t.Fatalf("owner = %#v", owner)
		}
	}
	for _, value := range []string{"", "agent", "department:ops", "team:"} {
		if _, err := parseOwner(value); err == nil {
			t.Fatalf("parseOwner(%q) succeeded", value)
		}
	}
}

func TestTUIRequestHeadersSendDaemonTokenFromEnvironment(t *testing.T) {
	for _, test := range []struct {
		name, token, flag, want string
	}{
		{name: "token", token: " daemon-token ", want: "Bearer daemon-token"},
		{name: "no token", want: ""},
		{name: "explicit header wins", token: "daemon-token", flag: "Authorization=Bearer explicit", want: "Bearer explicit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var observed []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed = r.Header.Values("Authorization")
				if len(observed) != 1 || observed[0] != "Bearer daemon-token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			flags := make(requestHeaderFlags)
			if test.flag != "" {
				if err := flags.Set(test.flag); err != nil {
					t.Fatal(err)
				}
			}
			_, err := client.NewKernelHTTPClient(server.URL, server.Client(), client.WithRequestHeaders(tuiRequestHeaders(flags, test.token))).Capabilities(t.Context())
			if got := strings.Join(observed, ","); got != test.want {
				t.Fatalf("Authorization = %q, want %q", got, test.want)
			}
			if (test.want == "Bearer daemon-token") != (err == nil) {
				t.Fatalf("token-protected request error = %v", err)
			}
		})
	}
	flags := make(requestHeaderFlags)
	tuiRequestHeaders(flags, "daemon-token")
	if len(flags) != 0 {
		t.Fatal("token was written into the caller's header flags")
	}
}
