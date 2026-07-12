package source

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var policyHostname = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Policy is a portable, credential-free outbound source policy. An embedding
// host owns storage, tenancy, authorization, rate limiting, and audit; OpenSeal
// owns deterministic validation and URL decisions.
type Policy struct {
	ID             string         `json:"id"`
	Version        string         `json:"version"`
	Enabled        bool           `json:"enabled"`
	Sources        []PolicySource `json:"sources"`
	MaximumItems   int            `json:"maximumItems,omitempty"`
	RetentionDays  int            `json:"retentionDays,omitempty"`
	ApprovalPolicy string         `json:"approvalPolicy,omitempty"`
}

type PolicySource struct {
	Host         string   `json:"host"`
	PathPrefixes []string `json:"pathPrefixes,omitempty"`
}

type PolicyDecision struct {
	PolicyID      string `json:"policyId"`
	PolicyVersion string `json:"policyVersion"`
	SourceHost    string `json:"sourceHost"`
	PathPrefix    string `json:"pathPrefix"`
	MaximumItems  int    `json:"maximumItems"`
}

// Authorize constrains host execution and redirects to the exact source scope
// selected by the control-plane policy decision.
func (d PolicyDecision) Authorize(rawURL string, requestedItems int) error {
	if strings.TrimSpace(d.PolicyID) == "" || strings.TrimSpace(d.PolicyVersion) == "" ||
		validatePolicyHost(strings.ToLower(strings.TrimSpace(d.SourceHost))) != nil ||
		validatePathPrefix(d.PathPrefix) != nil || d.MaximumItems < 1 || d.MaximumItems > MaximumItems {
		return errors.New("source policy decision is invalid")
	}
	target, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || target.Scheme != "https" || target.User != nil || target.Fragment != "" ||
		strings.ToLower(target.Hostname()) != strings.ToLower(d.SourceHost) ||
		(target.Port() != "" && target.Port() != "443") {
		return errors.New("source URL is outside the authorized policy decision")
	}
	if requestedItems < 1 || requestedItems > d.MaximumItems {
		return errors.New("source request exceeds the authorized policy decision")
	}
	if _, allowed := authorizedPathPrefix([]string{d.PathPrefix}, target.EscapedPath()); !allowed {
		return errors.New("source URL path is outside the authorized policy decision")
	}
	return nil
}

func (p Policy) Validate() error {
	if strings.TrimSpace(p.ID) == "" || len(p.ID) > 128 || strings.TrimSpace(p.Version) == "" || len(p.Version) > 128 {
		return errors.New("source policy id and version are required")
	}
	if len(p.Sources) == 0 || len(p.Sources) > 100 {
		return errors.New("source policy requires between 1 and 100 sources")
	}
	if p.MaximumItems < 1 || p.MaximumItems > MaximumItems {
		return fmt.Errorf("source policy maximumItems must be between 1 and %d", MaximumItems)
	}
	if p.RetentionDays < 0 || p.RetentionDays > 3650 {
		return errors.New("source policy retentionDays must be between 0 and 3650")
	}
	seen := make(map[string]bool)
	for _, source := range p.Sources {
		host := strings.ToLower(strings.TrimSpace(source.Host))
		if err := validatePolicyHost(host); err != nil {
			return err
		}
		if seen[host] {
			return fmt.Errorf("source policy host %q is duplicated", host)
		}
		seen[host] = true
		for _, prefix := range source.PathPrefixes {
			if err := validatePathPrefix(prefix); err != nil {
				return fmt.Errorf("source policy host %q: %w", host, err)
			}
		}
	}
	return nil
}

// Authorize returns the exact policy facts a host must carry through dispatch
// and redirect checks. It never performs network I/O or DNS resolution.
func (p Policy) Authorize(rawURL string, requestedItems int) (*PolicyDecision, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if !p.Enabled {
		return nil, errors.New("source policy is disabled")
	}
	target, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || target.Fragment != "" {
		return nil, errors.New("source URL must be an absolute credential-free HTTPS URL")
	}
	if target.Port() != "" && target.Port() != "443" {
		return nil, errors.New("source URL HTTPS port must be 443")
	}
	if requestedItems < 1 || requestedItems > p.MaximumItems {
		return nil, fmt.Errorf("source request exceeds policy maximumItems %d", p.MaximumItems)
	}
	host := strings.ToLower(target.Hostname())
	for _, candidate := range p.Sources {
		if !policyHostMatches(strings.ToLower(strings.TrimSpace(candidate.Host)), host) {
			continue
		}
		prefix, ok := authorizedPathPrefix(candidate.PathPrefixes, target.EscapedPath())
		if !ok {
			continue
		}
		return &PolicyDecision{PolicyID: p.ID, PolicyVersion: p.Version, SourceHost: host, PathPrefix: prefix, MaximumItems: p.MaximumItems}, nil
	}
	return nil, errors.New("source URL is not allowed by policy")
}

func validatePolicyHost(host string) error {
	if host == "" || strings.ContainsAny(host, "/:@?#") || strings.HasSuffix(host, ".") {
		return errors.New("source policy host is invalid")
	}
	name := host
	if strings.HasPrefix(name, "*.") {
		name = strings.TrimPrefix(name, "*.")
	}
	if name == "" || strings.Contains(name, "*") || net.ParseIP(name) != nil || !policyHostname.MatchString(name) {
		return errors.New("source policy host must be an exact hostname or leftmost wildcard hostname")
	}
	return nil
}

func validatePathPrefix(prefix string) error {
	if prefix == "" || !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "?#") || strings.Contains(prefix, "..") {
		return errors.New("source policy path prefix must be an absolute normalized path")
	}
	return nil
}

func policyHostMatches(pattern, host string) bool {
	if strings.HasPrefix(pattern, "*.") {
		suffix := strings.TrimPrefix(pattern, "*")
		return strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".")
	}
	return host == pattern
}

func authorizedPathPrefix(prefixes []string, escapedPath string) (string, bool) {
	if escapedPath == "" {
		escapedPath = "/"
	}
	if len(prefixes) == 0 {
		return "/", true
	}
	ordered := append([]string(nil), prefixes...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, prefix := range ordered {
		if escapedPath == prefix || strings.HasPrefix(escapedPath, strings.TrimSuffix(prefix, "/")+"/") {
			return prefix, true
		}
	}
	return "", false
}
