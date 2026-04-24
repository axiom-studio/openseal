package clawhub

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL    = "https://clawhub.ai/api/v1"
	defaultTimeout    = 30 * time.Second
	maxRetries        = 2
	initialBackoff    = 500 * time.Millisecond
	backoffMultiplier = 2
	envRegistryURL    = "CLAWHUB_REGISTRY"
)

type resultCache struct {
	mu      sync.RWMutex
	search  cachedSearch
	explore cachedExplore
	inspect cachedInspect
}

type cachedSearch struct {
	query     string
	limit     int
	sort      string
	results   []SkillSummary
	timestamp time.Time
}

type cachedExplore struct {
	limit     int
	sort      string
	results   []SkillSummary
	timestamp time.Time
}

type cachedInspect struct {
	slug      string
	version   string
	result    *SkillDetail
	timestamp time.Time
}

type ClawHubClient struct {
	baseURL    string
	httpClient *http.Client
	cache      resultCache
}

func NewClawHubClient(baseURL string) *ClawHubClient {
	if baseURL == "" {
		baseURL = os.Getenv(envRegistryURL)
		if baseURL == "" {
			baseURL = defaultBaseURL
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")

	return &ClawHubClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

func (c *ClawHubClient) Search(query string, limit int, sort string) ([]SkillSummary, error) {
	params := url.Values{}
	params.Set("q", query)
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if sort != "" {
		params.Set("sort", sort)
	}

	resp, err := c.doRequest("GET", "/search", params, nil)
	if err != nil {
		if IsRateLimitError(err) {
			results, cacheErr := c.cachedSearch(query, limit, sort)
			if cacheErr == nil {
				return results, nil
			}
		}
		return nil, err
	}
	defer resp.Body.Close()

	results, err := decodeSkillSummaryArray(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	c.cache.mu.Lock()
	c.cache.search = cachedSearch{
		query:     query,
		limit:     limit,
		sort:      sort,
		results:   results,
		timestamp: time.Now(),
	}
	c.cache.mu.Unlock()

	return results, nil
}

func (c *ClawHubClient) Inspect(slug string, version string) (*SkillDetail, error) {
	params := url.Values{}
	if version != "" {
		params.Set("version", version)
	}

	resp, err := c.doRequest("GET", "/skills/"+slug, params, nil)
	if err != nil {
		if IsRateLimitError(err) {
			detail, cacheErr := c.cachedInspect(slug, version)
			if cacheErr == nil {
				return detail, nil
			}
		}
		return nil, err
	}
	defer resp.Body.Close()

	var detail SkillDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, fmt.Errorf("decode inspect response: %w", err)
	}

	c.cache.mu.Lock()
	c.cache.inspect = cachedInspect{
		slug:      slug,
		version:   version,
		result:    &detail,
		timestamp: time.Now(),
	}
	c.cache.mu.Unlock()

	return &detail, nil
}

func (c *ClawHubClient) Explore(limit int, sort string) ([]SkillSummary, error) {
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if sort != "" {
		params.Set("sort", sort)
	}

	resp, err := c.doRequest("GET", "/skills", params, nil)
	if err != nil {
		if IsRateLimitError(err) {
			results, cacheErr := c.cachedExplore(limit, sort)
			if cacheErr == nil {
				return results, nil
			}
		}
		return nil, err
	}
	defer resp.Body.Close()

	results, err := decodeSkillSummaryArray(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("decode explore response: %w", err)
	}

	c.cache.mu.Lock()
	c.cache.explore = cachedExplore{
		limit:     limit,
		sort:      sort,
		results:   results,
		timestamp: time.Now(),
	}
	c.cache.mu.Unlock()

	return results, nil
}

func (c *ClawHubClient) Download(slug string, destDir string) (string, error) {
	params := url.Values{}
	params.Set("slug", slug)

	resp, err := c.doRequest("GET", "/download", params, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	tmpFile, err := os.CreateTemp("", "clawhub-*.zip")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("download zip: %w", err)
	}
	tmpFile.Close()

	extractDir, err := os.MkdirTemp(destDir, "clawhub-"+slug+"-*")
	if err != nil {
		return "", fmt.Errorf("create extract dir: %w", err)
	}

	if err := extractZip(tmpPath, extractDir); err != nil {
		os.RemoveAll(extractDir)
		return "", fmt.Errorf("extract zip: %w", err)
	}

	return extractDir, nil
}

func (c *ClawHubClient) doRequest(method, path string, params url.Values, body io.Reader) (*http.Response, error) {
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	var lastErr error
	backoff := initialBackoff

	for attempt := 0; attempt <= maxRetries; attempt++ {
		var req *http.Request
		var err error
		if body != nil && attempt == 0 {
			req, err = http.NewRequest(method, u, body)
		} else {
			req, err = http.NewRequest(method, u, nil)
		}
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode < 400 {
			return resp, nil
		}

		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()

		clawErr := &ClawHubError{
			StatusCode: resp.StatusCode,
			Message:    strings.TrimSpace(string(errBody)),
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
				if seconds, parseErr := strconv.Atoi(retryAfter); parseErr == nil {
					clawErr.RetryAfter = seconds
				}
			}
			return nil, clawErr
		}

		if resp.StatusCode == http.StatusNotFound {
			return nil, clawErr
		}

		if resp.StatusCode >= 500 && attempt < maxRetries {
			lastErr = clawErr
			time.Sleep(backoff)
			backoff *= backoffMultiplier
			continue
		}

		if resp.StatusCode >= 500 {
			return nil, &ClawHubError{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("server error after %d retries: %s", maxRetries, clawErr.Message),
			}
		}
		return nil, clawErr
	}

	return nil, lastErr
}

func (c *ClawHubClient) cachedSearch(query string, limit int, sort string) ([]SkillSummary, error) {
	c.cache.mu.RLock()
	defer c.cache.mu.RUnlock()
	cs := c.cache.search
	if cs.query == query && cs.limit == limit && cs.sort == sort && len(cs.results) > 0 {
		results := make([]SkillSummary, len(cs.results))
		for i, r := range cs.results {
			r.IsStale = true
			results[i] = r
		}
		return results, nil
	}
	return nil, &ClawHubError{StatusCode: http.StatusTooManyRequests, Message: "rate limited and no cached search results available"}
}

func (c *ClawHubClient) cachedExplore(limit int, sort string) ([]SkillSummary, error) {
	c.cache.mu.RLock()
	defer c.cache.mu.RUnlock()
	ce := c.cache.explore
	if ce.limit == limit && ce.sort == sort && len(ce.results) > 0 {
		results := make([]SkillSummary, len(ce.results))
		for i, r := range ce.results {
			r.IsStale = true
			results[i] = r
		}
		return results, nil
	}
	return nil, &ClawHubError{StatusCode: http.StatusTooManyRequests, Message: "rate limited and no cached explore results available"}
}

func (c *ClawHubClient) cachedInspect(slug string, version string) (*SkillDetail, error) {
	c.cache.mu.RLock()
	defer c.cache.mu.RUnlock()
	ci := c.cache.inspect
	if ci.slug == slug && ci.version == version && ci.result != nil {
		detail := *ci.result
		detail.IsStale = true
		return &detail, nil
	}
	return nil, &ClawHubError{StatusCode: http.StatusTooManyRequests, Message: "rate limited and no cached inspect results available"}
}

func extractZip(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		fpath := filepath.Join(destDir, f.Name)

		if !strings.HasPrefix(fpath, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path in zip: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, 0755)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
			return err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func decodeSkillSummaryArray(r io.Reader) ([]SkillSummary, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	var rawArray []SkillSummary
	if err := json.Unmarshal(body, &rawArray); err == nil {
		if rawArray == nil {
			rawArray = []SkillSummary{}
		}
		return rawArray, nil
	}

	var wrapper map[string]interface{}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("response is neither array nor object: %w", err)
	}

	for _, key := range []string{"skills", "data", "results", "items"} {
		if val, ok := wrapper[key]; ok {
			bytes, err := json.Marshal(val)
			if err != nil {
				continue
			}
			var results []SkillSummary
			if err := json.Unmarshal(bytes, &results); err == nil {
				if results == nil {
					results = []SkillSummary{}
				}
				return results, nil
			}
		}
	}

	return []SkillSummary{}, nil
}
