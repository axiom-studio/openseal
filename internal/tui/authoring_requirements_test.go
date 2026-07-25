package tui

import (
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func TestAuthoringRequirementActionUsesGuidedProductCopy(t *testing.T) {
	candidate := &authoring.WorkforceCandidate{
		Agents: []*kernelagent.AgentDefinition{{
			ID:          "market-research",
			DisplayName: "Market Research Agent",
		}},
		Team: &kernelteam.Definition{
			ID:          "gtm-research",
			DisplayName: "GTM Research Team",
		},
	}
	tests := []struct {
		name        string
		requirement authoring.MissingRequirement
		want        string
	}{
		{
			name:        "installation",
			requirement: authoring.MissingRequirement{Kind: "skill_installation", ID: "reddit-search", RequiredBy: "agent:market-research"},
			want:        "Install Reddit Search for Market Research Agent",
		},
		{
			name:        "binding",
			requirement: authoring.MissingRequirement{Kind: "skill_binding", ID: "source-observer", RequiredBy: "agent:market-research"},
			want:        "Configure Source Observer for Market Research Agent",
		},
		{
			name:        "credential",
			requirement: authoring.MissingRequirement{Kind: "credential", ID: "reddit_oauth", RequiredBy: "agent:market-research/skill:reddit-search"},
			want:        "Choose an authorized Reddit OAuth credential for Market Research Agent",
		},
		{
			name:        "source policy",
			requirement: authoring.MissingRequirement{Kind: "source_policy", ID: "approved-communities", RequiredBy: "team:gtm-research"},
			want:        "Approve source access for GTM Research Team",
		},
		{
			name:        "source scope",
			requirement: authoring.MissingRequirement{Kind: "source_scope", ID: "approved-communities", RequiredBy: "team:gtm-research"},
			want:        "Choose allowed sources for GTM Research Team",
		},
		{
			name:        "version",
			requirement: authoring.MissingRequirement{Kind: "version", ID: "document-renderer@1.0.2", RequiredBy: "agent:market-research"},
			want:        "Choose a compatible Document Renderer version for Market Research Agent",
		},
		{
			name:        "action",
			requirement: authoring.MissingRequirement{Kind: "action", ID: "reddit-search/searchRedditV1", RequiredBy: "agent:market-research"},
			want:        "Choose a Reddit Search version with the required action for Market Research Agent",
		},
		{
			name:        "prompt",
			requirement: authoring.MissingRequirement{Kind: "prompt", ID: "academic-writing-polisher", RequiredBy: "agent:market-research"},
			want:        "Add the required Academic Writing Polisher instructions for Market Research Agent",
		},
		{
			name:        "skill",
			requirement: authoring.MissingRequirement{Kind: "skill", ID: "openseal.delivery", RequiredBy: "team:gtm-research"},
			want:        "Add OpenSeal Delivery for GTM Research Team",
		},
		{
			name:        "future kind",
			requirement: authoring.MissingRequirement{Kind: "future_internal_kind", ID: "custom_capability", RequiredBy: "agent:future-worker"},
			want:        "Review Custom Capability setup for Future Worker",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := authoringRequirementAction(test.requirement, candidate)
			if got != test.want {
				t.Fatalf("action = %q, want %q", got, test.want)
			}
			if strings.Contains(got, test.requirement.RequiredBy) {
				t.Fatalf("primary copy exposed compiler internals: %q", got)
			}
		})
	}
}
