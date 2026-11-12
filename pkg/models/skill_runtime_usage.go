package models

import (
	"context"
	"errors"
	"strings"
)

// SkillRuntimeUsageFilter selects one tenant's immutable executable version.
// The optional deployment/binding pair narrows the same check to one grant.
// No model arguments or action outputs participate in this query.
type SkillRuntimeUsageFilter struct {
	Scope        Scope  `json:"scope"`
	SkillID      string `json:"skillId"`
	SkillVersion string `json:"skillVersion"`
	DeploymentID string `json:"deploymentId,omitempty"`
	BindingID    string `json:"bindingId,omitempty"`
}

func (f SkillRuntimeUsageFilter) Validate() error {
	if err := f.Scope.Validate(); err != nil {
		return err
	}
	for _, value := range []string{f.SkillID, f.SkillVersion} {
		if value == "" || value != strings.TrimSpace(value) || len(value) > 256 || strings.ContainsAny(value, "\r\n\t") {
			return errors.New("an exact bounded Skill ID and version are required")
		}
	}
	if (f.DeploymentID == "") != (f.BindingID == "") {
		return errors.New("deployment and binding must be selected together")
	}
	for _, value := range []string{f.DeploymentID, f.BindingID} {
		if value != strings.TrimSpace(value) || len(value) > 256 || strings.ContainsAny(value, "\r\n\t") {
			return errors.New("deployment and binding must be exact bounded identities")
		}
	}
	return nil
}

// SkillRuntimeReference is a metadata-only retention identity. An empty source
// identity on a historical action means all installed publisher variants of
// that immutable version must be retained, rather than guessing its publisher.
type SkillRuntimeReference struct {
	SkillID        string `json:"skillId"`
	SkillVersion   string `json:"skillVersion"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
}

type SkillRuntimeUsageStore interface {
	HasSkillRuntimeUsage(context.Context, SkillRuntimeUsageFilter) (bool, error)
	ListReferencedSkillRuntimeVersions(context.Context, Scope) ([]SkillRuntimeReference, error)
}

func SkillBindingUpgradeUsageFilter(application *SkillReferenceUpgradeMutation, bindingID string) SkillRuntimeUsageFilter {
	return SkillRuntimeUsageFilter{Scope: application.Plan.Scope, SkillID: application.Plan.From.ID, SkillVersion: application.Plan.From.Version, DeploymentID: application.Plan.DeploymentID, BindingID: bindingID}
}
