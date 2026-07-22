package hostadapter

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	opensealsource "github.com/axiom-studio/openseal/pkg/source"
)

type feedRoundTripper func(*http.Request) (*http.Response, error)

func (f feedRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestSourceFeedExecutorReturnsCanonicalObservations(t *testing.T) {
	body := `<rss><channel><title>Public forum</title><item><guid>thread-1</guid><link>https://forum.example/thread/1</link><title>Agent recovery</title><description>Runs must survive restarts.</description><pubDate>Sun, 13 Jul 2026 02:00:00 +0000</pubDate></item></channel></rss>`
	executor := &SourceFeedExecutor{
		validate: func(context.Context, *url.URL) error { return nil },
		client: &http.Client{Transport: feedRoundTripper(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("Accept") == "" || request.Header.Get("User-Agent") == "" {
				t.Fatal("governed feed headers were not set")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/rss+xml"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
	decision := map[string]interface{}{"policyId": "public-forum", "policyVersion": "1", "sourceHost": "forum.example", "pathPrefix": "/feed", "maximumItems": 1}
	result, err := executor.Execute(context.Background(), &StepDefinition{Config: map[string]interface{}{"url": "https://forum.example/feed", "maxItems": 1, "_opensealSourcePolicyDecision": decision}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	observations, ok := result.Output["sourceObservations"].([]interface{})
	if !ok || len(observations) != 1 || result.Output["nextCursor"] != "thread-1" {
		t.Fatalf("canonical output mismatch: %#v", result.Output)
	}
	observation := observations[0].(map[string]interface{})
	if observation["sourceUri"] != "https://forum.example/thread/1" || !strings.HasPrefix(observation["contentDigest"].(string), "sha256:") {
		t.Fatalf("observation mismatch: %#v", observation)
	}
}

func TestSourceFeedExecutorFailsClosed(t *testing.T) {
	executor := NewSourceFeedExecutor()
	decision := map[string]interface{}{"policyId": "public-web", "policyVersion": "1", "sourceHost": "example.com", "pathPrefix": "/feed", "maximumItems": 5}
	for _, raw := range []string{"http://example.com/feed", "https://127.0.0.1/feed", "https://[::1]/feed", "https://user:pass@example.com/feed", "https://example.com:8443/feed"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := executor.Execute(context.Background(), &StepDefinition{Config: map[string]interface{}{"url": raw, "maxItems": 5, "_opensealSourcePolicyDecision": decision}}, nil); err == nil {
				t.Fatal("unsafe feed URL was accepted")
			}
		})
	}
	if _, err := executor.Execute(context.Background(), &StepDefinition{Config: map[string]interface{}{"url": "https://example.com/feed", "maxItems": 1}}, nil); err == nil {
		t.Fatal("source request without trusted policy decision was accepted")
	}
	if supportedFeedMediaType("text/html") || !supportedFeedMediaType("application/atom+xml; charset=utf-8") {
		t.Fatal("feed media-type policy mismatch")
	}
	if _, err := feedMaximumItems(101); err == nil {
		t.Fatal("oversized item request was accepted")
	}
}

func TestSourceFeedExecutorReturnsEmptyDeltaAtDurableCursor(t *testing.T) {
	body := `<feed xmlns="http://www.w3.org/2005/Atom"><title>Forum</title><entry><id>thread-1</id><title>Finding</title><updated>2026-07-13T02:00:00Z</updated><link href="https://forum.example/feed/thread-1"/></entry></feed>`
	executor := &SourceFeedExecutor{
		validate: func(context.Context, *url.URL) error { return nil },
		client: &http.Client{Transport: feedRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/atom+xml"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
	decision := map[string]interface{}{"policyId": "public-forum", "policyVersion": "1", "sourceHost": "forum.example", "pathPrefix": "/feed", "maximumItems": 5}
	result, err := executor.Execute(context.Background(), &StepDefinition{Config: map[string]interface{}{
		"url": "https://forum.example/feed", "maxItems": 5, opensealsource.PolicyDecisionTransportKey: decision, opensealsource.CursorTransportKey: "thread-1",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	observations, ok := result.Output["sourceObservations"].([]interface{})
	if !ok || len(observations) != 0 || result.Output["nextCursor"] != "thread-1" {
		t.Fatalf("empty delta mismatch: %#v", result.Output)
	}
}
