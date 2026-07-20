package clawhub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxCatalogPages       = 10000
	catalogPageMaxRetries = 2
	catalogRetryBaseDelay = 100 * time.Millisecond
	catalogRetryMaxDelay  = 30 * time.Second
)

type SkillReference struct {
	Owner string `json:"owner,omitempty"`
	Slug  string `json:"slug"`
}

func ParseSkillReference(value string) (SkillReference, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return SkillReference{}, fmt.Errorf("skill reference is required")
	}
	value = strings.TrimPrefix(value, "@")
	parts := strings.Split(value, "/")
	if len(parts) == 1 && validReferencePart(parts[0]) {
		return SkillReference{Slug: parts[0]}, nil
	}
	if len(parts) == 2 && validReferencePart(parts[0]) && validReferencePart(parts[1]) {
		return SkillReference{Owner: parts[0], Slug: parts[1]}, nil
	}
	return SkillReference{}, fmt.Errorf("invalid skill reference %q", value)
}

func validReferencePart(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for i, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		if i > 0 && (r == '-' || r == '_') {
			continue
		}
		return false
	}
	return true
}

func (r SkillReference) String() string {
	if r.Owner == "" {
		return r.Slug
	}
	return "@" + r.Owner + "/" + r.Slug
}

type SearchRequest struct {
	Query             string
	Limit             int
	Sort              string
	NonSuspiciousOnly bool
}

type ExploreRequest struct {
	Limit             int
	Sort              string
	Cursor            string
	NonSuspiciousOnly bool
}

type SkillPage struct {
	Items      []SkillSummary `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

// CatalogSnapshot is an authoritative, deduplicated traversal of ClawHub's
// cursor-paginated discovery catalog. Pages is the number of successfully
// retrieved pages, including an empty terminal page when the registry returns
// one.
type CatalogSnapshot struct {
	Items              []SkillSummary `json:"items"`
	Pages              int            `json:"pages"`
	DuplicateItemCount int            `json:"duplicateItemCount,omitempty"`
}

// CatalogTraversalError reports how far an incomplete catalog traversal got.
// Callers must not mistake the partial Items in CatalogSnapshot for an
// authoritative catalog replacement.
type CatalogTraversalError struct {
	CompletedPages int
	UniqueItems    int
	Cursor         string
	Err            error
}

func (e *CatalogTraversalError) Error() string {
	if e == nil {
		return "ClawHub catalog traversal failed"
	}
	return fmt.Sprintf("ClawHub catalog traversal failed after %d page(s) and %d unique skill(s) at cursor %q: %v", e.CompletedPages, e.UniqueItems, e.Cursor, e.Err)
}

func (e *CatalogTraversalError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type VersionSummary struct {
	Version         string `json:"version"`
	CreatedAt       int64  `json:"createdAt"`
	Changelog       string `json:"changelog,omitempty"`
	ChangelogSource string `json:"changelogSource,omitempty"`
}

type VersionPage struct {
	Items      []VersionSummary `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type FileEntry struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256,omitempty"`
	ContentType string `json:"contentType,omitempty"`
}

type SecurityStatus struct {
	Status      string `json:"status"`
	HasWarnings bool   `json:"hasWarnings"`
	CheckedAt   *int64 `json:"checkedAt,omitempty"`
	Model       string `json:"model,omitempty"`
}

type VersionDetail struct {
	Version         string          `json:"version"`
	CreatedAt       int64           `json:"createdAt"`
	Changelog       string          `json:"changelog,omitempty"`
	ChangelogSource string          `json:"changelogSource,omitempty"`
	License         string          `json:"license,omitempty"`
	Files           []FileEntry     `json:"files,omitempty"`
	Security        *SecurityStatus `json:"security,omitempty"`
}

type Verification struct {
	Schema               string                 `json:"schema"`
	OK                   bool                   `json:"ok"`
	Decision             string                 `json:"decision"`
	Reasons              []string               `json:"reasons,omitempty"`
	Slug                 string                 `json:"slug"`
	DisplayName          string                 `json:"displayName"`
	PageURL              string                 `json:"pageUrl"`
	PublisherHandle      string                 `json:"publisherHandle,omitempty"`
	PublisherDisplayName string                 `json:"publisherDisplayName,omitempty"`
	PublisherProfileURL  string                 `json:"publisherProfileUrl,omitempty"`
	Version              string                 `json:"version"`
	ResolvedFrom         string                 `json:"resolvedFrom"`
	Tag                  string                 `json:"tag,omitempty"`
	CreatedAt            int64                  `json:"createdAt"`
	Card                 map[string]interface{} `json:"card,omitempty"`
	Artifact             map[string]interface{} `json:"artifact,omitempty"`
	Provenance           map[string]interface{} `json:"provenance,omitempty"`
	Security             map[string]interface{} `json:"security,omitempty"`
	Signature            map[string]interface{} `json:"signature,omitempty"`
}

type DownloadedArchive struct {
	Bytes  []byte `json:"-"`
	SHA256 string `json:"sha256"`
}

type Registry interface {
	SearchSkills(context.Context, SearchRequest) (*SkillPage, error)
	ExploreSkills(context.Context, ExploreRequest) (*SkillPage, error)
	InspectSkill(context.Context, SkillReference) (*SkillDetail, error)
	ListVersions(context.Context, SkillReference, int, string) (*VersionPage, error)
	GetVersion(context.Context, SkillReference, string) (*VersionDetail, error)
	GetFile(context.Context, SkillReference, string, string, string) ([]byte, error)
	VerifySkill(context.Context, SkillReference, string, string) (*Verification, error)
	DownloadArchive(context.Context, SkillReference, string, string) (*DownloadedArchive, error)
}

// ExploreCatalog traverses ExploreSkills until the registry authoritatively
// omits its next cursor. Overlapping pages are deduplicated by stable slug,
// preserving the first occurrence and discovery order. Cursor reuse fails
// closed so a faulty registry cannot make synchronization loop forever.
func ExploreCatalog(ctx context.Context, registry Registry, request ExploreRequest) (*CatalogSnapshot, error) {
	snapshot := &CatalogSnapshot{Items: make([]SkillSummary, 0)}
	if registry == nil {
		return snapshot, &CatalogTraversalError{Err: fmt.Errorf("ClawHub registry is required")}
	}

	cursor := strings.TrimSpace(request.Cursor)
	seenCursors := make(map[string]struct{})
	seenItems := make(map[string]struct{})
	for {
		if cursor != "" {
			if _, exists := seenCursors[cursor]; exists {
				return snapshot, &CatalogTraversalError{CompletedPages: snapshot.Pages, UniqueItems: len(snapshot.Items), Cursor: cursor, Err: fmt.Errorf("registry repeated a discovery cursor")}
			}
			seenCursors[cursor] = struct{}{}
		}

		request.Cursor = cursor
		page, err := exploreCatalogPage(ctx, registry, request)
		if err != nil {
			return snapshot, &CatalogTraversalError{CompletedPages: snapshot.Pages, UniqueItems: len(snapshot.Items), Cursor: cursor, Err: err}
		}
		if page == nil {
			return snapshot, &CatalogTraversalError{CompletedPages: snapshot.Pages, UniqueItems: len(snapshot.Items), Cursor: cursor, Err: fmt.Errorf("registry returned an empty discovery page envelope")}
		}
		snapshot.Pages++
		for _, item := range page.Items {
			identity := strings.ToLower(strings.TrimSpace(item.Slug))
			if identity == "" {
				continue
			}
			if _, exists := seenItems[identity]; exists {
				snapshot.DuplicateItemCount++
				continue
			}
			seenItems[identity] = struct{}{}
			snapshot.Items = append(snapshot.Items, item)
		}

		next := strings.TrimSpace(page.NextCursor)
		if next == "" {
			return snapshot, nil
		}
		if snapshot.Pages >= maxCatalogPages {
			return snapshot, &CatalogTraversalError{CompletedPages: snapshot.Pages, UniqueItems: len(snapshot.Items), Cursor: next, Err: fmt.Errorf("registry exceeded the %d-page discovery safety limit", maxCatalogPages)}
		}
		cursor = next
	}
}

func exploreCatalogPage(ctx context.Context, registry Registry, request ExploreRequest) (*SkillPage, error) {
	var lastErr error
	for attempt := 0; attempt <= catalogPageMaxRetries; attempt++ {
		page, err := registry.ExploreSkills(ctx, request)
		if err == nil {
			return page, nil
		}
		lastErr = err
		if attempt == catalogPageMaxRetries || !catalogPageErrorRetryable(err) {
			return nil, err
		}
		delay := catalogRetryBaseDelay * time.Duration(1<<attempt)
		var registryErr *ClawHubError
		if errors.As(err, &registryErr) && registryErr.RetryAfter > 0 {
			delay = time.Duration(registryErr.RetryAfter) * time.Second
			if delay > catalogRetryMaxDelay {
				delay = catalogRetryMaxDelay
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func catalogPageErrorRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var registryErr *ClawHubError
	if !errors.As(err, &registryErr) {
		return true
	}
	return registryErr.StatusCode == http.StatusTooManyRequests || registryErr.StatusCode >= http.StatusInternalServerError
}

func (c *ClawHubClient) SearchSkills(ctx context.Context, request SearchRequest) (*SkillPage, error) {
	query := url.Values{"q": []string{request.Query}}
	applyDiscoveryOptions(query, request.Limit, request.Sort, "", request.NonSuspiciousOnly)
	var raw json.RawMessage
	if err := c.getJSON(ctx, "/search", query, &raw); err != nil {
		return nil, err
	}
	return decodeSkillPage(raw)
}

func (c *ClawHubClient) ExploreSkills(ctx context.Context, request ExploreRequest) (*SkillPage, error) {
	query := url.Values{}
	applyDiscoveryOptions(query, request.Limit, request.Sort, request.Cursor, request.NonSuspiciousOnly)
	var raw json.RawMessage
	if err := c.getJSON(ctx, "/skills", query, &raw); err != nil {
		return nil, err
	}
	return decodeSkillPage(raw)
}

func (c *ClawHubClient) InspectSkill(ctx context.Context, ref SkillReference) (*SkillDetail, error) {
	query := ownerQuery(ref)
	var detail SkillDetail
	if err := c.getJSON(ctx, "/skills/"+url.PathEscape(ref.Slug), query, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

func (c *ClawHubClient) ListVersions(ctx context.Context, ref SkillReference, limit int, cursor string) (*VersionPage, error) {
	query := ownerQuery(ref)
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	var page VersionPage
	if err := c.getJSON(ctx, "/skills/"+url.PathEscape(ref.Slug)+"/versions", query, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

func (c *ClawHubClient) GetVersion(ctx context.Context, ref SkillReference, version string) (*VersionDetail, error) {
	if strings.TrimSpace(version) == "" {
		return nil, fmt.Errorf("version is required")
	}
	var response struct {
		Version *VersionDetail `json:"version"`
	}
	if err := c.getJSON(ctx, "/skills/"+url.PathEscape(ref.Slug)+"/versions/"+url.PathEscape(version), ownerQuery(ref), &response); err != nil {
		return nil, err
	}
	if response.Version == nil {
		return nil, ErrNotFound
	}
	return response.Version, nil
}

func (c *ClawHubClient) GetFile(ctx context.Context, ref SkillReference, path, version, tag string) ([]byte, error) {
	if strings.TrimSpace(path) == "" || version != "" && tag != "" {
		return nil, fmt.Errorf("path is required and version and tag are mutually exclusive")
	}
	query := ownerQuery(ref)
	query.Set("path", path)
	if version != "" {
		query.Set("version", version)
	}
	if tag != "" {
		query.Set("tag", tag)
	}
	return c.getBytes(ctx, "/skills/"+url.PathEscape(ref.Slug)+"/file", query, 200*1024)
}

func (c *ClawHubClient) VerifySkill(ctx context.Context, ref SkillReference, version, tag string) (*Verification, error) {
	if version != "" && tag != "" {
		return nil, fmt.Errorf("version and tag are mutually exclusive")
	}
	query := ownerQuery(ref)
	if version != "" {
		query.Set("version", version)
	}
	if tag != "" {
		query.Set("tag", tag)
	}
	var result Verification
	if err := c.getJSON(ctx, "/skills/"+url.PathEscape(ref.Slug)+"/verify", query, &result); err != nil {
		return nil, err
	}
	if result.Schema != "clawhub.skill.verify.v1" {
		return nil, fmt.Errorf("unsupported verification schema %q", result.Schema)
	}
	return &result, nil
}

func (c *ClawHubClient) DownloadArchive(ctx context.Context, ref SkillReference, version, tag string) (*DownloadedArchive, error) {
	if version != "" && tag != "" {
		return nil, fmt.Errorf("version and tag are mutually exclusive")
	}
	query := ownerQuery(ref)
	query.Set("slug", ref.Slug)
	if version != "" {
		query.Set("version", version)
	}
	if tag != "" {
		query.Set("tag", tag)
	}
	archive, err := c.getBytes(ctx, "/download", query, 100*1024*1024)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(archive)
	return &DownloadedArchive{Bytes: archive, SHA256: hex.EncodeToString(digest[:])}, nil
}

func (c *ClawHubClient) getJSON(ctx context.Context, path string, query url.Values, target interface{}) error {
	body, err := c.getBytes(ctx, path, query, 4*1024*1024)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode ClawHub response: %w", err)
	}
	return nil
}

func (c *ClawHubClient) getBytes(ctx context.Context, path string, query url.Values, limit int64) ([]byte, error) {
	requestURL := c.baseURL + path
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if attempt < maxRetries && waitForRetry(ctx, attempt) == nil {
				continue
			}
			return nil, err
		}
		if resp.StatusCode < 400 {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
			resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			if int64(len(body)) > limit {
				return nil, fmt.Errorf("ClawHub response exceeds %d bytes", limit)
			}
			return body, nil
		}
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		registryErr := &ClawHubError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(message))}
		if resp.StatusCode == http.StatusTooManyRequests {
			if seconds, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil {
				registryErr.RetryAfter = seconds
			}
			return nil, registryErr
		}
		lastErr = registryErr
		if resp.StatusCode >= 500 && attempt < maxRetries {
			if err := waitForRetry(ctx, attempt); err != nil {
				return nil, err
			}
			continue
		}
		return nil, registryErr
	}
	return nil, lastErr
}

func waitForRetry(ctx context.Context, attempt int) error {
	delay := 500 * time.Millisecond * time.Duration(1<<attempt)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func decodeSkillPage(raw json.RawMessage) (*SkillPage, error) {
	var direct []SkillSummary
	if err := json.Unmarshal(raw, &direct); err == nil {
		return &SkillPage{Items: direct}, nil
	}
	var page struct {
		Items      []SkillSummary `json:"items"`
		Results    []SkillSummary `json:"results"`
		NextCursor string         `json:"nextCursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("decode skill page: %w", err)
	}
	if page.Items == nil {
		page.Items = page.Results
	}
	return &SkillPage{Items: page.Items, NextCursor: page.NextCursor}, nil
}

func applyDiscoveryOptions(query url.Values, limit int, sort, cursor string, safeOnly bool) {
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if sort != "" {
		query.Set("sort", sort)
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if safeOnly {
		query.Set("nonSuspiciousOnly", "true")
	}
}

func ownerQuery(ref SkillReference) url.Values {
	query := url.Values{}
	if ref.Owner != "" {
		query.Set("ownerHandle", ref.Owner)
	}
	return query
}
