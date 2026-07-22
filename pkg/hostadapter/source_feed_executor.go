package hostadapter

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	opensealsource "github.com/axiom-studio/openseal/pkg/source"
)

const (
	NodeTypeSourceFeed     = opensealsource.SkillID
	maximumFeedResponse    = 2 << 20
	maximumFeedRedirects   = 3
	defaultFeedObservation = 25
)

type SourceFeedExecutor struct {
	client   *http.Client
	validate func(context.Context, *url.URL) error
}

func NewSourceFeedExecutor() *SourceFeedExecutor {
	resolver := net.DefaultResolver
	validate := func(ctx context.Context, target *url.URL) error {
		return validatePublicFeedURL(ctx, resolver, target)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("invalid source address: %w", err)
			}
			ips, err := publicHostAddresses(ctx, resolver, host)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
		MaxIdleConns: 20, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	executor := &SourceFeedExecutor{validate: validate}
	executor.client = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	executor.client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > maximumFeedRedirects {
			return errors.New("source feed exceeded redirect limit")
		}
		return executor.validate(request.Context(), request.URL)
	}
	return executor
}

func (e *SourceFeedExecutor) Type() string { return NodeTypeSourceFeed }

func (e *SourceFeedExecutor) Execute(ctx context.Context, step *StepDefinition, _ TemplateResolver) (*StepResult, error) {
	if e == nil || e.client == nil || e.validate == nil || step == nil || step.Config == nil {
		return nil, errors.New("source feed observer is not configured")
	}
	rawURL, _ := step.Config["url"].(string)
	target, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, errors.New("source feed URL is invalid")
	}
	maxItems, err := feedMaximumItems(step.Config["maxItems"])
	if err != nil {
		return nil, err
	}
	decision, err := sourcePolicyDecision(step.Config[opensealsource.PolicyDecisionTransportKey])
	if err != nil {
		return nil, err
	}
	if err := decision.Authorize(target.String(), maxItems); err != nil {
		return nil, err
	}
	if err := e.validate(ctx, target); err != nil {
		return nil, err
	}
	cursor, _ := step.Config[opensealsource.CursorTransportKey].(string)
	cursor = strings.TrimSpace(cursor)
	if len(cursor) > 1000 {
		return nil, errors.New("trusted source cursor exceeds 1000 characters")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/atom+xml, application/rss+xml, application/xml, text/xml;q=0.9")
	request.Header.Set("User-Agent", "OpenSeal-Source-Observer/1.0")
	client := *e.client
	client.CheckRedirect = func(redirect *http.Request, via []*http.Request) error {
		if len(via) > maximumFeedRedirects {
			return errors.New("source feed exceeded redirect limit")
		}
		if err := decision.Authorize(redirect.URL.String(), maxItems); err != nil {
			return err
		}
		return e.validate(redirect.Context(), redirect.URL)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read source feed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("source feed returned HTTP %d", response.StatusCode)
	}
	if !supportedFeedMediaType(response.Header.Get("Content-Type")) {
		return nil, fmt.Errorf("source feed returned unsupported media type %q", response.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumFeedResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read source feed body: %w", err)
	}
	if len(body) > maximumFeedResponse {
		return nil, fmt.Errorf("source feed exceeds %d-byte limit", maximumFeedResponse)
	}
	parsed, err := opensealsource.ParseFeedSince(body, target.String(), maxItems, cursor)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return nil, err
	}
	var output map[string]interface{}
	if err := json.Unmarshal(encoded, &output); err != nil {
		return nil, err
	}
	return &StepResult{Output: output}, nil
}

func sourcePolicyDecision(value interface{}) (*opensealsource.PolicyDecision, error) {
	if value == nil {
		return nil, errors.New("trusted source policy decision is required")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("trusted source policy decision is invalid")
	}
	var decision opensealsource.PolicyDecision
	if err := json.Unmarshal(encoded, &decision); err != nil {
		return nil, errors.New("trusted source policy decision is invalid")
	}
	if err := decision.Authorize("https://"+decision.SourceHost+decision.PathPrefix, 1); err != nil {
		return nil, errors.New("trusted source policy decision is invalid")
	}
	return &decision, nil
}

func validatePublicFeedURL(ctx context.Context, resolver *net.Resolver, target *url.URL) error {
	if target == nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || target.Fragment != "" {
		return errors.New("source feed must be an absolute credential-free HTTPS URL")
	}
	if target.Port() != "" && target.Port() != "443" {
		return errors.New("source feed HTTPS port must be 443")
	}
	_, err := publicHostAddresses(ctx, resolver, target.Hostname())
	return err
}

func publicHostAddresses(ctx context.Context, resolver *net.Resolver, host string) ([]net.IP, error) {
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve source feed host: %w", err)
	}
	if len(addresses) == 0 {
		return nil, errors.New("source feed host has no addresses")
	}
	result := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		ip := address.IP
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return nil, errors.New("source feed host resolves to a non-public address")
		}
		result = append(result, ip)
	}
	return result, nil
}

func feedMaximumItems(value interface{}) (int, error) {
	if value == nil {
		return defaultFeedObservation, nil
	}
	var count int
	switch typed := value.(type) {
	case int:
		count = typed
	case int64:
		count = int(typed)
	case float64:
		if typed != float64(int(typed)) {
			return 0, errors.New("maxItems must be an integer")
		}
		count = int(typed)
	case string:
		parsed, err := strconv.Atoi(typed)
		if err != nil {
			return 0, errors.New("maxItems must be an integer")
		}
		count = parsed
	default:
		return 0, errors.New("maxItems must be an integer")
	}
	if count < 1 || count > opensealsource.MaximumItems {
		return 0, fmt.Errorf("maxItems must be between 1 and %d", opensealsource.MaximumItems)
	}
	return count, nil
}

func supportedFeedMediaType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	switch value {
	case "application/atom+xml", "application/rss+xml", "application/xml", "text/xml":
		return true
	default:
		return false
	}
}
