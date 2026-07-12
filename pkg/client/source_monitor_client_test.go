package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestKernelHTTPClientEncodesSourceMonitorRoutes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.EscapedPath() {
		case "/api/v1/initiatives/initiative%2Fone/source-monitors/reddit%2Fnew/checkpoint":
			if request.URL.Query().Get("scopeKind") != "tenant" || request.URL.Query().Get("scopeId") != "one" {
				t.Fatalf("checkpoint query=%s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(runtime.SourceMonitorCheckpoint{InitiativeID: "initiative/one", MonitorID: "reddit/new", Revision: 3})
		case "/api/v1/initiatives/initiative%2Fone/source-monitors/reddit%2Fnew/observations":
			if request.URL.Query().Get("limit") != "20" || request.URL.Query().Get("offset") != "4" {
				t.Fatalf("observation query=%s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode([]*runtime.SourceObservation{{ID: "observation-1"}})
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	client := NewKernelHTTPClient(server.URL, server.Client())
	scope := runtime.Scope{Kind: "tenant", ID: "one"}
	checkpoint, err := client.GetSourceMonitorCheckpoint(context.Background(), scope, "initiative/one", "reddit/new")
	if err != nil || checkpoint.Revision != 3 {
		t.Fatalf("checkpoint=%#v err=%v", checkpoint, err)
	}
	observations, err := client.ListSourceObservations(context.Background(), runtime.SourceObservationFilter{Scope: scope, InitiativeID: "initiative/one", MonitorID: "reddit/new", Limit: 20, Offset: 4})
	if err != nil || len(observations) != 1 || observations[0].ID != "observation-1" {
		t.Fatalf("observations=%#v err=%v", observations, err)
	}
}
