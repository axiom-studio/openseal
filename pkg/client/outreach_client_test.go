package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestKernelClientUsesCanonicalOutreachRoutes(t *testing.T) {
	scope := runtime.Scope{Kind: "tenant", ID: "one"}
	requests := make([]*http.Request, 0, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Clone(request.Context()))
		w.Header().Set("Content-Type", "application/json")
		path := request.URL.EscapedPath()
		switch {
		case request.Method == http.MethodPost && path == "/api/v1/projects/project%2Fone/outreach":
			if request.Header.Get("Idempotency-Key") != "draft-key" {
				t.Errorf("draft idempotency = %q", request.Header.Get("Idempotency-Key"))
			}
			_ = json.NewEncoder(w).Encode(runtime.OutreachThread{ID: "thread-1", ProjectID: "project/one"})
		case request.Method == http.MethodGet && path == "/api/v1/projects/project%2Fone/outreach":
			_ = json.NewEncoder(w).Encode([]*runtime.OutreachThread{{ID: "thread-1", ProjectID: "project/one"}})
		case request.Method == http.MethodGet && path == "/api/v1/projects/project%2Fone/outreach/thread%2Fone":
			_ = json.NewEncoder(w).Encode(runtime.OutreachThread{ID: "thread/one", ProjectID: "project/one"})
		case request.Method == http.MethodPost && path == "/api/v1/projects/project%2Fone/outreach/thread%2Fone/messages/message%2Fone/deliveries":
			if request.Header.Get("Idempotency-Key") != "delivery-key" {
				t.Errorf("delivery idempotency = %q", request.Header.Get("Idempotency-Key"))
			}
			_ = json.NewEncoder(w).Encode(runtime.AgentRun{ID: "run-1", Scope: scope})
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	client := NewKernelHTTPClient(server.URL, server.Client())
	ctx := context.Background()
	created, err := client.CreateOutreachThread(ctx, kernelapi.CreateOutreachThreadRequest{Scope: scope, ProjectID: "project/one"}, "draft-key")
	if err != nil || created.ID != "thread-1" {
		t.Fatalf("created = %#v err=%v", created, err)
	}
	listed, err := client.ListOutreachThreads(ctx, runtime.OutreachThreadFilter{
		Scope: scope, ProjectID: "project/one", SourceObservationID: "observation/one", Statuses: []runtime.OutreachThreadStatus{runtime.OutreachThreadOpen}, Limit: 25, Offset: 2,
	})
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed = %#v err=%v", listed, err)
	}
	got, err := client.GetOutreachThread(ctx, scope, "project/one", "thread/one")
	if err != nil || got.ID != "thread/one" {
		t.Fatalf("got = %#v err=%v", got, err)
	}
	run, err := client.DeliverOutreachMessage(ctx, "project/one", "thread/one", "message/one", kernelapi.DeliverOutreachMessageRequest{Scope: scope}, "delivery-key")
	if err != nil || run.ID != "run-1" {
		t.Fatalf("run = %#v err=%v", run, err)
	}
	if len(requests) != 4 {
		t.Fatalf("requests = %d", len(requests))
	}
	query := requests[1].URL.Query()
	if query.Get("scopeKind") != scope.Kind || query.Get("scopeId") != scope.ID || query.Get("sourceObservationId") != "observation/one" ||
		query.Get("status") != string(runtime.OutreachThreadOpen) || query.Get("limit") != "25" || query.Get("offset") != "2" {
		t.Fatalf("list query = %s", requests[1].URL.RawQuery)
	}
	if !strings.Contains(requests[3].URL.EscapedPath(), "message%2Fone") {
		t.Fatalf("delivery path = %s", requests[3].URL.EscapedPath())
	}
}
