package authoring

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	DefaultSkillSearchLimit = 20
	MaximumSkillSearchLimit = 100
)

// SkillSearchOrigin describes where an exact authoring candidate currently
// lives. Catalog candidates are possibilities, not active Agent authority.
type SkillSearchOrigin string

const (
	SkillSearchOriginEnabled SkillSearchOrigin = "enabled"
	SkillSearchOriginCatalog SkillSearchOrigin = "catalog"
)

type SkillSearchVerification string

const (
	SkillSearchVerificationVerified SkillSearchVerification = "verified"
	SkillSearchVerificationRequired SkillSearchVerification = "required"
	SkillSearchVerificationFailed   SkillSearchVerification = "failed"
)

// SkillSearchTrust is a host-verified classification of the catalog boundary
// that supplied a Skill. It is independent of marketplace branding so clients
// never need to infer trust from a publisher name or registry URL.
type SkillSearchTrust string

const (
	SkillSearchTrustVerifiedPublisher SkillSearchTrust = "verified_publisher"
	SkillSearchTrustPrivate           SkillSearchTrust = "private"
	SkillSearchTrustCommunity         SkillSearchTrust = "community"
)

// SkillSearchRequest is the product-neutral query used while composing an
// Agent, Team, or Workforce. Cursor values are opaque, host-authored values.
type SkillSearchRequest struct {
	Scope           skill.ScopeReference `json:"scope"`
	Query           string               `json:"query"`
	RequiredActions []string             `json:"requiredActions,omitempty"`
	MaximumRisk     capability.RiskLevel `json:"maximumRisk,omitempty"`
	Cursor          string               `json:"cursor,omitempty"`
	Limit           int                  `json:"limit,omitempty"`
}

// SkillSearchProvenance contains only review-safe source facts. Reference is
// an opaque immutable receipt or registry identifier; it must never contain a
// credential or secret-bearing installation argument.
type SkillSearchProvenance struct {
	Registry  string           `json:"registry,omitempty"`
	Publisher string           `json:"publisher,omitempty"`
	Reference string           `json:"reference,omitempty"`
	Digest    string           `json:"digest,omitempty"`
	Trust     SkillSearchTrust `json:"trust,omitempty"`
}

// SkillSearchCandidate is an exact Skill possibility for a reviewable plan.
// Readiness and compatibility remain host facts. RequiresApproval means a
// governed lifecycle step is required before the Skill can become active; it
// does not imply that approval has already been requested or granted.
type SkillSearchCandidate struct {
	SkillCapability
	Origin           SkillSearchOrigin       `json:"origin"`
	Verification     SkillSearchVerification `json:"verification"`
	Provenance       SkillSearchProvenance   `json:"provenance,omitempty"`
	RequiresApproval bool                    `json:"requiresApproval,omitempty"`
}

type SkillSearchPage struct {
	Items       []SkillSearchCandidate `json:"items"`
	NextCursor  string                 `json:"nextCursor,omitempty"`
	Diagnostics []CatalogDiagnostic    `json:"diagnostics,omitempty"`
}

// SkillSearchIdentity is the client-visible exact identity of a discovered
// candidate. It carries no authority: an API host must resolve it again
// against its current provider before passing a TrustedSkill to the kernel.
type SkillSearchIdentity struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	SourceIdentity string `json:"sourceIdentity"`
}

type SkillSearchProvider interface {
	SearchAuthoringSkills(context.Context, SkillSearchRequest) (*SkillSearchPage, error)
}

type SkillSearchProviderFunc func(context.Context, SkillSearchRequest) (*SkillSearchPage, error)

func (f SkillSearchProviderFunc) SearchAuthoringSkills(ctx context.Context, request SkillSearchRequest) (*SkillSearchPage, error) {
	return f(ctx, request)
}

func NormalizeSkillSearchRequest(request SkillSearchRequest) (SkillSearchRequest, error) {
	request.Scope.ID = strings.TrimSpace(request.Scope.ID)
	request.Query = strings.TrimSpace(request.Query)
	request.Cursor = strings.TrimSpace(request.Cursor)
	if request.Scope.Kind == "" || request.Scope.ID == "" {
		return SkillSearchRequest{}, errors.New("Skill search scope is required")
	}
	if request.Query == "" {
		return SkillSearchRequest{}, errors.New("Skill search query is required")
	}
	if len(request.Query) > 512 || len(request.Cursor) > 2048 {
		return SkillSearchRequest{}, errors.New("Skill search query or cursor is too long")
	}
	request.RequiredActions = normalizedSkillSearchStrings(request.RequiredActions)
	if len(request.RequiredActions) > 32 {
		return SkillSearchRequest{}, errors.New("Skill search has too many required actions")
	}
	for _, action := range request.RequiredActions {
		if len(action) > 128 {
			return SkillSearchRequest{}, errors.New("Skill search action is too long")
		}
	}
	if request.MaximumRisk != "" && !validAuthoringSearchRisk(request.MaximumRisk) {
		return SkillSearchRequest{}, errors.New("Skill search maximum risk is invalid")
	}
	if request.Limit == 0 {
		request.Limit = DefaultSkillSearchLimit
	}
	if request.Limit < 1 || request.Limit > MaximumSkillSearchLimit {
		return SkillSearchRequest{}, errors.New("Skill search limit is outside the supported range")
	}
	return request, nil
}

// NormalizeSkillSearchPage validates identity and lifecycle truth while
// preserving provider order. Provider order is the relevance ranking and must
// not be replaced by client-side alphabetical sorting.
func NormalizeSkillSearchPage(request SkillSearchRequest, page *SkillSearchPage) (*SkillSearchPage, error) {
	if page == nil {
		return nil, errors.New("Skill search provider returned no page")
	}
	if len(page.Items) > request.Limit {
		return nil, errors.New("Skill search provider exceeded the requested limit")
	}
	nextCursor := strings.TrimSpace(page.NextCursor)
	if len(nextCursor) > 2048 {
		return nil, errors.New("Skill search provider cursor is too long")
	}
	if err := ValidateCapabilityCatalog(CapabilityCatalog{Diagnostics: page.Diagnostics}); err != nil {
		return nil, fmt.Errorf("Skill search diagnostics are invalid: %w", err)
	}
	result := &SkillSearchPage{
		Items:       make([]SkillSearchCandidate, 0, len(page.Items)),
		NextCursor:  nextCursor,
		Diagnostics: append([]CatalogDiagnostic(nil), page.Diagnostics...),
	}
	seen := make(map[string]struct{}, len(page.Items))
	for index, candidate := range page.Items {
		candidate.ID = strings.TrimSpace(candidate.ID)
		candidate.Version = strings.TrimSpace(candidate.Version)
		candidate.SourceIdentity = strings.TrimSpace(candidate.SourceIdentity)
		candidate.Name = strings.TrimSpace(candidate.Name)
		candidate.Description = strings.TrimSpace(candidate.Description)
		candidate.Provenance.Registry = strings.TrimSpace(candidate.Provenance.Registry)
		candidate.Provenance.Publisher = strings.TrimSpace(candidate.Provenance.Publisher)
		candidate.Provenance.Reference = strings.TrimSpace(candidate.Provenance.Reference)
		candidate.Provenance.Digest = strings.TrimSpace(candidate.Provenance.Digest)
		if candidate.ID == "" || candidate.Version == "" || candidate.Name == "" {
			return nil, fmt.Errorf("Skill search candidate %d has incomplete exact identity", index)
		}
		if len(candidate.ID) > 256 || len(candidate.Version) > 256 || len(candidate.SourceIdentity) > 1024 ||
			len(candidate.Name) > 256 || len(candidate.Description) > 2048 ||
			len(candidate.Provenance.Registry) > 256 || len(candidate.Provenance.Publisher) > 256 ||
			len(candidate.Provenance.Reference) > 2048 || len(candidate.Provenance.Digest) > 256 {
			return nil, fmt.Errorf("Skill search candidate %d exceeds metadata limits", index)
		}
		switch candidate.Provenance.Trust {
		case "", SkillSearchTrustVerifiedPublisher, SkillSearchTrustPrivate, SkillSearchTrustCommunity:
		default:
			return nil, fmt.Errorf("Skill search candidate %d has invalid provenance trust", index)
		}
		switch candidate.Origin {
		case SkillSearchOriginEnabled, SkillSearchOriginCatalog:
		default:
			return nil, fmt.Errorf("Skill search candidate %d has invalid origin", index)
		}
		switch candidate.Verification {
		case SkillSearchVerificationVerified, SkillSearchVerificationRequired, SkillSearchVerificationFailed:
		default:
			return nil, fmt.Errorf("Skill search candidate %d has invalid verification", index)
		}
		if candidate.Origin == SkillSearchOriginEnabled && candidate.Verification != SkillSearchVerificationVerified {
			return nil, fmt.Errorf("enabled Skill search candidate %d must be verified", index)
		}
		switch candidate.Readiness {
		case SkillReadinessReady, SkillReadinessNeedsBinding, SkillReadinessNeedsInstallation, SkillReadinessUnavailable:
		default:
			return nil, fmt.Errorf("Skill search candidate %d has invalid readiness", index)
		}
		if candidate.Origin == SkillSearchOriginCatalog && candidate.Verification == SkillSearchVerificationVerified && candidate.SourceIdentity == "" {
			return nil, fmt.Errorf("verified catalog Skill search candidate %d requires exact source identity", index)
		}
		if candidate.Readiness == SkillReadinessNeedsInstallation && candidate.Origin != SkillSearchOriginCatalog {
			return nil, fmt.Errorf("installable Skill search candidate %d must come from a catalog", index)
		}
		if candidate.Readiness == SkillReadinessNeedsInstallation && candidate.Verification != SkillSearchVerificationVerified {
			return nil, fmt.Errorf("installable Skill search candidate %d must be verified", index)
		}
		if candidate.Readiness == SkillReadinessNeedsInstallation && !hasReceiptBackedInstallation(candidate.Compatibility) {
			return nil, fmt.Errorf("installable Skill search candidate %d lacks receipt-backed compatibility evidence", index)
		}
		if candidate.MaximumRisk != "" && !validAuthoringSearchRisk(candidate.MaximumRisk) {
			return nil, fmt.Errorf("Skill search candidate %d has invalid maximum risk", index)
		}
		candidate.Actions = normalizedSkillSearchStrings(candidate.Actions)
		candidate.CredentialKinds = normalizedSkillSearchStrings(candidate.CredentialKinds)
		identity := candidate.ID + "\x00" + candidate.Version + "\x00" + candidate.SourceIdentity
		if _, exists := seen[identity]; exists {
			return nil, fmt.Errorf("Skill search candidate %d duplicates an exact identity", index)
		}
		seen[identity] = struct{}{}
		result.Items = append(result.Items, candidate)
	}
	return result, nil
}

func normalizedSkillSearchStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func hasReceiptBackedInstallation(values []SkillCompatibility) bool {
	for _, value := range values {
		if value.Compatible && strings.TrimSpace(value.Reference) != "" {
			return true
		}
	}
	return false
}

func validAuthoringSearchRisk(value capability.RiskLevel) bool {
	switch value {
	case capability.RiskLevelRead, capability.RiskLevelWrite, capability.RiskLevelExternal, capability.RiskLevelDestructive, capability.RiskLevelProduction:
		return true
	default:
		return false
	}
}
