package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestConversationRunsAreScopedFilteredBeforePaging(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store runtime.KernelStore = runtime.NewMemoryStore()
			if backend == "sqlite" {
				db, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "runs.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				store = db
			}
			scope := runtime.Scope{Kind: "local", ID: "default"}
			owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "research"}
			channels := runtime.NewConversationService(store.(runtime.ConversationStore))
			channel, _, err := channels.CreateConversation(t.Context(), runtime.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Evidence", IdempotencyKey: "evidence"})
			if err != nil {
				t.Fatal(err)
			}
			portfolio := runtime.NewPortfolioService(store)
			ids := []string{}
			for _, sample := range []struct {
				channel string
				owner   runtime.ObjectiveOwner
				kind    runtime.RunKind
				scope   runtime.Scope
			}{
				{channel.ID, owner, runtime.RunKindConversation, scope},
				{channel.ID, owner, runtime.RunKindConversation, scope},
				{"another-channel", owner, runtime.RunKindConversation, scope},
				{channel.ID, runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "other"}, runtime.RunKindConversation, scope},
				{channel.ID, owner, runtime.RunKindAgentWork, scope},
				{channel.ID, owner, runtime.RunKindConversation, runtime.Scope{Kind: "local", ID: "other"}},
			} {
				run, err := portfolio.CreateAgentRun(t.Context(), runtime.CreateAgentRunRequest{Scope: sample.scope, Owner: sample.owner, Kind: sample.kind, Goal: "Reply", Source: runtime.RunSourceChat, Context: map[string]interface{}{"conversationId": sample.channel}})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, run.ID)
				time.Sleep(time.Millisecond)
			}
			server := NewServer(store, zap.NewNop().Sugar())
			server.SetDesktopConversationScope(scope)
			path := "/api/v1/conversations/" + channel.ID + "/runs?scopeKind=local&scopeId=default"
			for offset, want := range []string{ids[1], ids[0], ""} {
				result := performAgentRunRequest(t, server.Handler(), http.MethodGet, fmt.Sprintf("%s&limit=1&offset=%d", path, offset), "", "")
				if result.Code != http.StatusOK {
					t.Fatal(result.Body.String())
				}
				var runs []*runtime.AgentRun
				if err := json.Unmarshal(result.Body.Bytes(), &runs); err != nil {
					t.Fatal(err)
				}
				if want == "" {
					if len(runs) != 0 {
						t.Fatalf("unexpected final page: %#v", runs)
					}
				} else if len(runs) != 1 || runs[0].ID != want {
					t.Fatalf("page %d: %#v", offset, runs)
				}
			}
			for _, sample := range []struct {
				url  string
				code int
			}{
				{path + "&limit=0", 400}, {path + "&offset=-1", 400},
				{"/api/v1/conversations/missing/runs?scopeKind=local&scopeId=default", 404},
				{"/api/v1/conversations/" + channel.ID + "/runs?scopeKind=local&scopeId=other", 403},
			} {
				result := performAgentRunRequest(t, server.Handler(), http.MethodGet, sample.url, "", "")
				if result.Code != sample.code {
					t.Fatalf("%s: %d %s", sample.url, result.Code, result.Body.String())
				}
			}
		})
	}
}
