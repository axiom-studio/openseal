package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestActivityAPIIsCapabilityAdvertisedSelectorBoundedAndDetailedOnRequest(t *testing.T) {
	store := runtime.NewMemoryStore(100)
	scope := runtime.Scope{Kind: "tenant", ID: "one"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "researcher"}
	run, err := runtime.NewPortfolioService(store).CreateAgentRun(context.Background(), runtime.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Monitor governed sources", Source: runtime.RunSourceSchedule,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInitiative(context.Background(), &runtime.Initiative{
		ID: "initiative-1", Scope: scope, Owner: owner, Title: "Research", Purpose: "Collect evidence", Status: runtime.InitiativeStatusActive,
		ObjectiveRefs: []string{"objective-1"}, Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.NewRunActivityService(store, store).AppendActivity(context.Background(), &runtime.ActivityEvent{
		ID: "source-policy-call-1", Scope: scope, InitiativeID: "initiative-1", RunID: run.ID,
		EventType: "source_policy.authorized", Severity: runtime.ActivitySeverityInfo,
		Actor: runtime.ActivityActor{Type: "system", ID: "source-policy"}, Summary: "Source access authorized by policy",
		Payload:    map[string]interface{}{"monitorId": "monitor-1", "policyId": "public", "policyVersion": "1", "sourceHost": "www.reddit.com", "pathPrefix": "/r/kubernetes", "maximumItems": 5},
		Visibility: runtime.ActivityVisibilityScope, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewServer(nil, nil, store, zap.NewNop().Sugar())
	capabilities := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), `"id":"activity"`) {
		t.Fatalf("capabilities=%d %s", capabilities.Code, capabilities.Body.String())
	}
	missingSelector := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/activity?scopeKind=tenant&scopeId=one", "", "")
	if missingSelector.Code != http.StatusBadRequest {
		t.Fatalf("missing selector=%d %s", missingSelector.Code, missingSelector.Body.String())
	}
	compact := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/activity?scopeKind=tenant&scopeId=one&runId="+run.ID+"&eventType=source_policy.authorized", "", "")
	if compact.Code != http.StatusOK || !strings.Contains(compact.Body.String(), `"initiativeId":"initiative-1"`) || strings.Contains(compact.Body.String(), `"sourceHost"`) {
		t.Fatalf("compact=%d %s", compact.Code, compact.Body.String())
	}
	detailed := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/activity?scopeKind=tenant&scopeId=one&runId="+run.ID+"&eventType=source_policy.authorized&includeDetails=true", "", "")
	if detailed.Code != http.StatusOK || !strings.Contains(detailed.Body.String(), `"sourceHost":"www.reddit.com"`) || !strings.Contains(detailed.Body.String(), `"maximumItems":5`) {
		t.Fatalf("detailed=%d %s", detailed.Code, detailed.Body.String())
	}
	filtered := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/activity?scopeKind=tenant&scopeId=one&runId="+run.ID+"&severity=warning&visibility=scope", "", "")
	if filtered.Code != http.StatusOK || !strings.Contains(filtered.Body.String(), `"items":[]`) {
		t.Fatalf("filtered=%d %s", filtered.Code, filtered.Body.String())
	}
	invalidFilter := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/activity?scopeKind=tenant&scopeId=one&runId="+run.ID+"&severity=secret", "", "")
	if invalidFilter.Code != http.StatusBadRequest {
		t.Fatalf("invalid filter=%d %s", invalidFilter.Code, invalidFilter.Body.String())
	}
	crossScope := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/activity?scopeKind=tenant&scopeId=two&runId="+run.ID+"&includeDetails=true", "", "")
	if crossScope.Code != http.StatusOK || !strings.Contains(crossScope.Body.String(), `"items":[]`) {
		t.Fatalf("cross scope=%d %s", crossScope.Code, crossScope.Body.String())
	}
}
