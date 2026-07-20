package outreach

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/source"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestWebhookInvokerPostsOnlyHostAuthorizedReviewedOutreach(t *testing.T) {
	now := time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC)
	decision := source.OutreachPolicyDecision{PolicyID: "research", PolicyVersion: "1", SourceHost: "hooks.example.com", PathPrefix: "/community", ApprovalPolicy: "human-review", MaximumBytes: 1000}
	var calls int
	invoker := &WebhookInvoker{
		authorizer: InvocationAuthorizerFunc(func(_ context.Context, invocation runtime.ToolInvocation) (*InvocationAuthorization, error) {
			if invocation.RunID != "run-1" || invocation.ActionCallID != "action-1" {
				t.Fatalf("authorization invocation = %#v", invocation)
			}
			return &InvocationAuthorization{Decision: decision, ApprovalPolicy: "human-review"}, nil
		}),
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.String() != "https://hooks.example.com/community/thread-1" || request.Header.Get("Idempotency-Key") != "action-1" || request.Header.Get("User-Agent") != "OpenSeal-Outreach/1.0" {
				t.Fatalf("request = %#v", request)
			}
			body, _ := io.ReadAll(request.Body)
			if string(body) != `{"body":"Reviewed disclosure and question"}` {
				t.Fatalf("body = %s", body)
			}
			return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{"X-Request-Id": {"provider-1"}}, Body: io.NopCloser(strings.NewReader("ignored")), Request: request}, nil
		})},
		validate: func(context.Context, *url.URL) error { return nil }, now: func() time.Time { return now },
	}
	result, err := invoker.InvokeTool(t.Context(), runtime.ToolInvocation{
		Name: SkillID, Scope: skill.ScopeReference{Kind: "local", ID: "research"}, DeploymentID: "researcher",
		SkillID: SkillID, SkillVersion: SkillVersion, Action: PostReply, ActionCallID: "action-1", RunID: "run-1",
		Arguments: map[string]interface{}{"targetUri": "https://hooks.example.com/community/thread-1", "body": "Reviewed disclosure and question"},
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ := result["outreachReceipt"].(map[string]interface{})
	if calls != 1 || receipt["externalId"] != "provider-1" || receipt["deliveredAt"] != now.Format(time.RFC3339Nano) || !strings.HasPrefix(receipt["digest"].(string), "sha256:") {
		t.Fatalf("calls=%d receipt=%#v", calls, receipt)
	}
}

func TestWebhookInvokerFailsClosedBeforeTransport(t *testing.T) {
	decision := source.OutreachPolicyDecision{PolicyID: "research", PolicyVersion: "1", SourceHost: "hooks.example.com", PathPrefix: "/community", ApprovalPolicy: "human-review", MaximumBytes: 1000}
	invoker := &WebhookInvoker{
		authorizer: InvocationAuthorizerFunc(func(context.Context, runtime.ToolInvocation) (*InvocationAuthorization, error) {
			return &InvocationAuthorization{Decision: decision, ApprovalPolicy: "human-review"}, nil
		}),
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unauthorized invocation reached transport")
			return nil, nil
		})}, validate: func(context.Context, *url.URL) error { return nil }, now: time.Now,
	}
	base := runtime.ToolInvocation{Name: SkillID, DeploymentID: "researcher", SkillID: SkillID, SkillVersion: SkillVersion, Action: PostReply, ActionCallID: "action-1", RunID: "run-1", Arguments: map[string]interface{}{"targetUri": "https://hooks.example.com/other", "body": "reviewed"}}
	if _, err := invoker.InvokeTool(t.Context(), base); err == nil {
		t.Fatal("out-of-policy target was accepted")
	}
	base.Arguments["targetUri"] = "https://hooks.example.com/community/thread"
	base.ActionCallID = "action\r\nevil"
	if _, err := invoker.InvokeTool(t.Context(), base); err == nil {
		t.Fatal("header injection was accepted")
	}
}
