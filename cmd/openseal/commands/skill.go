package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/axiom-studio/openseal/pkg/delivery"
	"github.com/axiom-studio/openseal/pkg/document"
	"github.com/axiom-studio/openseal/pkg/outreach"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/source"
)

func skillCmd(args []string) {
	if err := runSkillCommand(os.Stdout, args); err != nil {
		fmt.Fprintf(os.Stderr, "skill: %v\n", err)
		return
	}
}

func runSkillCommand(output io.Writer, args []string) error {
	if len(args) != 2 || args[0] != "manifest" {
		return errors.New("usage: openseal skill manifest <skill-id>")
	}
	definitions := bundledSkillDefinitions()
	definition := definitions[args[1]]
	if definition == nil {
		ids := make([]string, 0, len(definitions))
		for id := range definitions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return fmt.Errorf("unknown bundled Skill %q; available: %v", args[1], ids)
	}
	manifest, err := skill.NewManifest(definition)
	if err != nil {
		return err
	}
	data, err := skill.EncodeManifestYAML(manifest)
	if err != nil {
		return err
	}
	_, err = output.Write(data)
	return err
}

func bundledSkillDefinitions() map[string]*skill.Definition {
	return map[string]*skill.Definition{
		source.SkillID:   source.SkillDefinition(),
		document.SkillID: document.SkillDefinition(),
		outreach.SkillID: outreach.SkillDefinition(),
		delivery.SkillID: delivery.SkillDefinition(),
	}
}
