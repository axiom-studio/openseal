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
	if err := runSkillCommand(os.Stdout, args, os.WriteFile); err != nil {
		fmt.Fprintf(os.Stderr, "skill: %v\n", err)
		return
	}
}

type skillManifestWriter func(string, []byte, os.FileMode) error

func runSkillCommand(output io.Writer, args []string, writeFile skillManifestWriter) error {
	if len(args) == 2 && args[0] == "validate" {
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		manifest, err := skill.DecodeManifestYAML(data)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "valid %s@%s\n", manifest.Definition.ID, manifest.Definition.Version)
		return err
	}
	if (len(args) != 2 && len(args) != 4) || args[0] != "manifest" || (len(args) == 4 && args[2] != "--output") {
		return errors.New("usage: openseal skill manifest <skill-id> [--output <path>] | openseal skill validate <path>")
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
	if len(args) == 4 {
		if args[3] == "" || writeFile == nil {
			return errors.New("Skill manifest output path is required")
		}
		return writeFile(args[3], data, 0o644)
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
