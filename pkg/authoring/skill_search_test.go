package authoring

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestNormalizeSkillSearchRequestPreservesOpaquePaginationAndIntent(t *testing.T) {
	request, err := NormalizeSkillSearchRequest(SkillSearchRequest{
		Scope:           skill.ScopeReference{Kind: "tenant", ID: " 7 "},
		Query:           "  reddit research  ",
		RequiredActions: []string{" search ", "search", "read"},
		MaximumRisk:     capability.RiskLevelRead,
		Cursor:          " opaque-page ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Scope.ID != "7" || request.Query != "reddit research" || request.Cursor != "opaque-page" ||
		request.Limit != DefaultSkillSearchLimit || len(request.RequiredActions) != 2 ||
		request.RequiredActions[0] != "search" || request.RequiredActions[1] != "read" {
		t.Fatalf("normalized request = %#v", request)
	}
}

func TestNormalizeSkillSearchPagePreservesProviderRankingAndLifecycleTruth(t *testing.T) {
	request, err := NormalizeSkillSearchRequest(SkillSearchRequest{
		Scope: skill.ScopeReference{Kind: "tenant", ID: "7"}, Query: "reddit", Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := NormalizeSkillSearchPage(request, &SkillSearchPage{
		Items: []SkillSearchCandidate{
			{
				SkillCapability: SkillCapability{
					ID: "reddit-observer", Version: "2.0.0", SourceIdentity: "clawhub::@example/reddit-observer",
					Name: "Reddit Observer", Actions: []string{"search", "search"}, Readiness: SkillReadinessNeedsInstallation,
					Compatibility: []SkillCompatibility{{
						Requirement: "installation", Compatible: false, Evidence: "Verified artifact is not enabled",
						Reference: "preview:sha256:abc",
					}, {
						Requirement: "source_digest", Compatible: true, Evidence: "Verified immutable compilation",
						Reference: "preview:sha256:abc",
					}},
				},
				Origin:       SkillSearchOriginCatalog,
				Verification: SkillSearchVerificationVerified,
				Provenance: SkillSearchProvenance{
					Registry: "clawhub", Publisher: "@example", Reference: "preview:sha256:abc", Digest: "sha256:abc",
				},
				RequiresApproval: true,
			},
			{
				SkillCapability: SkillCapability{
					ID: "openseal.source", Version: "1.0.4", Name: "Source observer",
					Actions: []string{"observe_feed"}, Readiness: SkillReadinessReady,
				},
				Origin:       SkillSearchOriginEnabled,
				Verification: SkillSearchVerificationVerified,
			},
		},
		NextCursor: " next ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "next" || page.Items[0].ID != "reddit-observer" || page.Items[1].ID != "openseal.source" ||
		len(page.Items[0].Actions) != 1 || !page.Items[0].RequiresApproval {
		t.Fatalf("normalized page = %#v", page)
	}
}

func TestNormalizeSkillSearchPageRejectsUnsafeCatalogClaims(t *testing.T) {
	request, err := NormalizeSkillSearchRequest(SkillSearchRequest{
		Scope: skill.ScopeReference{Kind: "tenant", ID: "7"}, Query: "reddit", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := SkillSearchCandidate{
		SkillCapability: SkillCapability{
			ID: "reddit", Version: "1", Name: "Reddit", Readiness: SkillReadinessNeedsInstallation,
			Compatibility: []SkillCompatibility{{
				Requirement: "installation", Compatible: false, Evidence: "Not installed", Reference: "preview:1",
			}, {
				Requirement: "source_digest", Compatible: true, Evidence: "Verified immutable compilation", Reference: "preview:1",
			}},
		},
		Origin:       SkillSearchOriginCatalog,
		Verification: SkillSearchVerificationVerified,
	}
	for name, mutate := range map[string]func(*SkillSearchCandidate){
		"missing exact source": func(candidate *SkillSearchCandidate) {},
		"missing receipt": func(candidate *SkillSearchCandidate) {
			candidate.SourceIdentity = "registry::reddit"
			candidate.Compatibility[1].Reference = ""
		},
		"enabled cannot need installation": func(candidate *SkillSearchCandidate) {
			candidate.SourceIdentity = "registry::reddit"
			candidate.Origin = SkillSearchOriginEnabled
		},
		"invalid readiness": func(candidate *SkillSearchCandidate) {
			candidate.SourceIdentity = "registry::reddit"
			candidate.Readiness = "magic"
		},
		"unverified cannot be installable": func(candidate *SkillSearchCandidate) {
			candidate.SourceIdentity = "registry::reddit"
			candidate.Verification = SkillSearchVerificationRequired
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.Compatibility = append([]SkillCompatibility(nil), base.Compatibility...)
			mutate(&candidate)
			_, err := NormalizeSkillSearchPage(request, &SkillSearchPage{Items: []SkillSearchCandidate{candidate}})
			if err == nil {
				t.Fatal("unsafe candidate was accepted")
			}
		})
	}
}

func TestNormalizeSkillSearchRejectsOversizedOrDuplicateResults(t *testing.T) {
	request, err := NormalizeSkillSearchRequest(SkillSearchRequest{
		Scope: skill.ScopeReference{Kind: "tenant", ID: "7"}, Query: "summary", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	enabled := SkillSearchCandidate{
		SkillCapability: SkillCapability{ID: "summarize", Version: "1", Name: "Summarize", Readiness: SkillReadinessReady},
		Origin:          SkillSearchOriginEnabled,
		Verification:    SkillSearchVerificationVerified,
	}
	if _, err := NormalizeSkillSearchPage(request, &SkillSearchPage{Items: []SkillSearchCandidate{enabled, enabled}}); err == nil ||
		!strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("oversized page error = %v", err)
	}
	request.Limit = 2
	if _, err := NormalizeSkillSearchPage(request, &SkillSearchPage{Items: []SkillSearchCandidate{enabled, enabled}}); err == nil ||
		!strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("duplicate page error = %v", err)
	}
}
