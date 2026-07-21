package capability

import "strings"

// SkillIdentity is the exact runtime identity of one immutable Skill
// definition variant. SourceIdentity is deliberately part of equality: two
// registries or publishers may provide the same declared id and version while
// resolving to different capabilities.
//
// Catalog-facing ids and declared versions are presentation and selection
// metadata. They must never be substituted for this identity at an authority
// boundary.
type SkillIdentity struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
}

func NewSkillIdentity(id, version, sourceIdentity string) SkillIdentity {
	return SkillIdentity{
		ID:             strings.TrimSpace(id),
		Version:        strings.TrimSpace(version),
		SourceIdentity: strings.TrimSpace(sourceIdentity),
	}
}

func (i SkillIdentity) Normalized() SkillIdentity {
	return NewSkillIdentity(i.ID, i.Version, i.SourceIdentity)
}

func (i SkillIdentity) Valid() bool {
	normalized := i.Normalized()
	return normalized.ID != "" && normalized.Version != ""
}

func (i SkillIdentity) Equal(other SkillIdentity) bool {
	left, right := i.Normalized(), other.Normalized()
	return left.Valid() && right.Valid() && left == right
}

// Key is an internal comparison key. It is not a user-facing identifier and
// must not be parsed or persisted as an alternative Skill identity.
func (i SkillIdentity) Key() string {
	normalized := i.Normalized()
	return normalized.ID + "\x00" + normalized.Version + "\x00" + normalized.SourceIdentity
}
