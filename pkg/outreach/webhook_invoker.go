package outreach

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/source"
)

const maximumWebhookResponseBytes = 64 << 10

type InvocationAuthorization struct {
	Decision       source.OutreachPolicyDecision
	ApprovalPolicy string
}

type InvocationAuthorizer interface {
	AuthorizeOutreachInvocation(context.Context, runtime.ToolInvocation) (*InvocationAuthorization, error)
}

type InvocationAuthorizerFunc func(context.Context, runtime.ToolInvocation) (*InvocationAuthorization, error)

func (f InvocationAuthorizerFunc) AuthorizeOutreachInvocation(ctx context.Context, invocation runtime.ToolInvocation) (*InvocationAuthorization, error) {
	return f(ctx, invocation)
}

// WebhookInvoker is OpenSeal's portable, SSRF-safe transport for the canonical
// governed outreach Skill. A host authorizer supplies a trusted source-policy
// decision after model-visible input validation; redirects and private network
// destinations are never followed.
type WebhookInvoker struct {
	authorizer InvocationAuthorizer
	client     *http.Client
	validate   func(context.Context, *url.URL) error
	now        func() time.Time
}

func NewWebhookInvoker(authorizer InvocationAuthorizer) (*WebhookInvoker, error) {
	if authorizer == nil {
		return nil, errors.New("outreach invocation authorizer is required")
	}
	resolver := net.DefaultResolver
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("invalid outreach address: %w", err)
			}
			addresses, err := publicAddresses(ctx, resolver, host)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
		},
		MaxIdleConns: 20, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
	}
	return &WebhookInvoker{
		authorizer: authorizer, client: &http.Client{Transport: transport, Timeout: 30 * time.Second}, now: time.Now,
		validate: func(ctx context.Context, target *url.URL) error {
			if target == nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || target.Fragment != "" || (target.Port() != "" && target.Port() != "443") {
				return errors.New("outreach target must be an absolute credential-free HTTPS URL on port 443")
			}
			_, err := publicAddresses(ctx, resolver, target.Hostname())
			return err
		},
	}, nil
}

func (w *WebhookInvoker) InvokeTool(ctx context.Context, invocation runtime.ToolInvocation) (map[string]interface{}, error) {
	if w == nil || w.authorizer == nil || w.client == nil || w.validate == nil || w.now == nil {
		return nil, errors.New("outreach webhook invoker is not configured")
	}
	if invocation.Name != SkillID || invocation.SkillID != SkillID || invocation.SkillVersion != SkillVersion || invocation.Action != PostReply ||
		!safeHeaderValue(invocation.ActionCallID) || strings.TrimSpace(invocation.RunID) == "" || strings.TrimSpace(invocation.DeploymentID) == "" {
		return nil, errors.New("governed outreach invocation identity is incomplete")
	}
	targetURI, _ := invocation.Arguments["targetUri"].(string)
	body, _ := invocation.Arguments["body"].(string)
	if strings.TrimSpace(targetURI) == "" || strings.TrimSpace(body) == "" {
		return nil, errors.New("outreach target and reviewed body are required")
	}
	authorization, err := w.authorizer.AuthorizeOutreachInvocation(ctx, invocation)
	if err != nil {
		return nil, err
	}
	if authorization == nil {
		return nil, errors.New("trusted outreach authorization is incomplete")
	}
	if err := authorization.Decision.Authorize(targetURI, len([]byte(body)), authorization.ApprovalPolicy); err != nil {
		return nil, err
	}
	target, err := url.Parse(targetURI)
	if err != nil {
		return nil, errors.New("outreach target is invalid")
	}
	if err := w.validate(ctx, target); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", invocation.ActionCallID)
	request.Header.Set("User-Agent", "OpenSeal-Outreach/1.0")
	client := *w.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("outreach webhook redirects are not allowed")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("post governed outreach: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumWebhookResponseBytes))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("outreach provider returned HTTP %d", response.StatusCode)
	}
	externalID := safeProviderID(response.Header.Get("X-Request-Id"))
	if externalID == "" {
		externalID = "http:" + invocation.ActionCallID
	}
	digest := sha256.Sum256([]byte(body))
	return map[string]interface{}{"outreachReceipt": map[string]interface{}{
		"provider": "http-webhook", "externalId": externalID, "externalUri": target.String(),
		"deliveredAt": w.now().UTC().Format(time.RFC3339Nano), "digest": "sha256:" + hex.EncodeToString(digest[:]),
	}}, nil
}

func publicAddresses(ctx context.Context, resolver *net.Resolver, host string) ([]net.IP, error) {
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve outreach host: %w", err)
	}
	if len(addresses) == 0 {
		return nil, errors.New("outreach host has no addresses")
	}
	result := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		ip := address.IP
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return nil, errors.New("outreach host resolves to a non-public address")
		}
		result = append(result, ip)
	}
	return result, nil
}

func safeHeaderValue(value string) bool {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func safeProviderID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 512 || !safeHeaderValue(value) {
		return ""
	}
	return value
}
