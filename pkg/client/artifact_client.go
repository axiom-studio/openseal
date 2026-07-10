package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

type ArtifactClient interface {
	RegisterArtifact(context.Context, runtime.RegisterArtifactRequest) (*runtime.ArtifactRegistrationResult, error)
	GetArtifact(context.Context, runtime.Scope, string, int64) (*runtime.Artifact, error)
	ListArtifacts(context.Context, runtime.ArtifactFilter) ([]*runtime.Artifact, error)
	UploadArtifactContent(context.Context, runtime.Scope, string, string, int64, io.Reader) (*runtime.ArtifactStoredContent, error)
	DownloadArtifactContent(context.Context, runtime.Scope, string, int64) (*ArtifactDownload, error)
	ResolveArtifactContent(context.Context, runtime.Scope, string, int64, kernelapi.ResolveArtifactContentRequest) (*runtime.ArtifactContentResolution, error)
}

type ArtifactDownload struct {
	Body        io.ReadCloser
	MediaType   string
	Disposition string
	Digest      string
	SizeBytes   int64
}

func (c *KernelHTTPClient) RegisterArtifact(ctx context.Context, request runtime.RegisterArtifactRequest) (*runtime.ArtifactRegistrationResult, error) {
	var result runtime.ArtifactRegistrationResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/artifacts", request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) GetArtifact(ctx context.Context, scope runtime.Scope, artifactID string, version int64) (*runtime.Artifact, error) {
	query := scopeQuery(scope)
	if version > 0 {
		query.Set("version", strconv.FormatInt(version, 10))
	}
	var artifact runtime.Artifact
	path := "/api/v1/artifacts/" + url.PathEscape(strings.TrimSpace(artifactID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

func (c *KernelHTTPClient) ListArtifacts(ctx context.Context, filter runtime.ArtifactFilter) ([]*runtime.Artifact, error) {
	query := scopeQuery(filter.Scope)
	setIfPresent(query, "id", filter.ID)
	if filter.Owner != nil {
		query.Set("ownerType", string(filter.Owner.Type))
		query.Set("ownerId", filter.Owner.ID)
	}
	for _, value := range filter.Types {
		query.Add("type", value)
	}
	for _, value := range filter.MediaTypes {
		query.Add("mediaType", value)
	}
	for _, value := range filter.Classifications {
		query.Add("classification", string(value))
	}
	setIfPresent(query, "producerRunId", filter.ProducerRunID)
	setIfPresent(query, "producerRequestId", filter.ProducerRequestID)
	setIfPresent(query, "evidenceTarget", filter.EvidenceTarget)
	query.Set("latestOnly", strconv.FormatBool(filter.LatestOnly))
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var artifacts []*runtime.Artifact
	if err := c.do(ctx, http.MethodGet, "/api/v1/artifacts?"+query.Encode(), nil, "", &artifacts); err != nil {
		return nil, err
	}
	return artifacts, nil
}

func (c *KernelHTTPClient) UploadArtifactContent(ctx context.Context, scope runtime.Scope, mediaType, digest string, sizeBytes int64, reader io.Reader) (*runtime.ArtifactStoredContent, error) {
	query := scopeQuery(scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/artifact-content?"+query.Encode(), reader)
	if err != nil {
		return nil, fmt.Errorf("build artifact upload request: %w", err)
	}
	if mediaType = strings.TrimSpace(mediaType); mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	if digest = strings.TrimSpace(digest); digest != "" {
		req.Header.Set("X-Content-SHA256", digest)
	}
	if sizeBytes >= 0 {
		req.ContentLength = sizeBytes
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload artifact content: %w", err)
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, decodeAPIError(resp.StatusCode, decoder)
	}
	var stored runtime.ArtifactStoredContent
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode artifact upload response: %w", err)
	}
	return &stored, nil
}

func (c *KernelHTTPClient) DownloadArtifactContent(ctx context.Context, scope runtime.Scope, artifactID string, version int64) (*ArtifactDownload, error) {
	query := scopeQuery(scope)
	if version > 0 {
		query.Set("version", strconv.FormatInt(version, 10))
	}
	path := "/api/v1/artifacts/" + url.PathEscape(strings.TrimSpace(artifactID)) + "/content?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build artifact download request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download artifact content: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer resp.Body.Close()
		return nil, decodeAPIError(resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)))
	}
	return &ArtifactDownload{
		Body: resp.Body, MediaType: resp.Header.Get("Content-Type"), Disposition: resp.Header.Get("Content-Disposition"),
		Digest: resp.Header.Get("X-Content-SHA256"), SizeBytes: resp.ContentLength,
	}, nil
}

func (c *KernelHTTPClient) ResolveArtifactContent(ctx context.Context, scope runtime.Scope, artifactID string, version int64, request kernelapi.ResolveArtifactContentRequest) (*runtime.ArtifactContentResolution, error) {
	query := scopeQuery(scope)
	if version > 0 {
		query.Set("version", strconv.FormatInt(version, 10))
	}
	path := "/api/v1/artifacts/" + url.PathEscape(strings.TrimSpace(artifactID)) + "/resolve?" + query.Encode()
	var resolution runtime.ArtifactContentResolution
	if err := c.do(ctx, http.MethodPost, path, request, "", &resolution); err != nil {
		return nil, err
	}
	return &resolution, nil
}

var _ ArtifactClient = (*KernelHTTPClient)(nil)
