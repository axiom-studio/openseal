// Package skillmd preserves source compatibility for embedders while the
// canonical AgentSkills/OpenClaw parser lives under pkg/skill/skillmd.
//
// Deprecated: import github.com/axiom-studio/openseal/pkg/skill/skillmd.
package skillmd

import canonical "github.com/axiom-studio/openseal/pkg/skill/skillmd"

type ParsedSkill = canonical.ParsedSkill

func ParseSkillMD(content []byte) (*ParsedSkill, error) {
	return canonical.ParseSkillMD(content)
}
