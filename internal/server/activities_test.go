package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"go.uber.org/zap"
)

func TestActivityAPIIsCapabilityAdvertisedSelectorBoundedAndDetailedOnRequest(t *testing.T) {
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "one"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}
	run, err := portfolio.NewService(store).CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Monitor governed sources", Source: models.RunSourceSchedule,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = activity.NewRunActivityService(store, store).AppendActivity(context.Background(), &models.ActivityEvent{
		ID: "source-policy-call-1", Scope: scope, RunID: run.ID,
		EventType: "source_policy.authorized", Severity: models.ActivitySeverityInfo,
		Actor: models.ActivityActor{Type: "system", ID: "source-policy"}, Summary: "Source access authorized by policy",
		Payload:    map[string]interface{}{"monitorId": "monitor-1", "policyId": "public", "policyVersion": "1", "sourceHost": "www.reddit.com", "pathPrefix": "/r/kubernetes", "maximumItems": 5},
		Visibility: models.ActivityVisibilityScope, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewServer(store, zap.NewNop().Sugar())
	capabilities := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), `"id":"activity"`) {
		t.Fatalf("capabilities=%d %s", capabilities.Code, capabilities.Body.String())
	}
	missingSelector := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/activity?scopeKind=tenant&scopeId=one", "", "")
	if missingSelector.Code != http.StatusBadRequest {
		t.Fatalf("missing selector=%d %s", missingSelector.Code, missingSelector.Body.String())
	}
	compact := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/activity?scopeKind=tenant&scopeId=one&runId="+run.ID+"&eventType=source_policy.authorized", "", "")
	if compact.Code != http.StatusOK || !strings.Contains(compact.Body.String(), `"id":"source-policy-call-1"`) || strings.Contains(compact.Body.String(), `"sourceHost"`) {
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
