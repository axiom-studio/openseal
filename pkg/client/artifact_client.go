package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

type ArtifactClient interface {
	RegisterArtifact(context.Context, runtime.RegisterArtifactRequest) (*runtime.ArtifactRegistrationResult, error)
	GetArtifact(context.Context, runtime.Scope, string, int64) (*runtime.Artifact, error)
	ListArtifacts(context.Context, runtime.ArtifactFilter) ([]*runtime.Artifact, error)
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

var _ ArtifactClient = (*KernelHTTPClient)(nil)
