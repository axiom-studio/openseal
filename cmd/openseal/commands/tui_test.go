package commands

import (
	"net/http"
	"testing"

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
