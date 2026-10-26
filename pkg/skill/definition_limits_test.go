package skill

import (
	"strings"
	"testing"
)

// A definition at every metadata limit is accepted and discoverable; one byte
// past any limit is refused at registration, so discovery never meets it.
func TestDefinitionMetadataLimitsMatchDiscovery(t *testing.T) {
	atLimit := func() *Definition {
		definition := testSkillDefinition()
		definition.ID = strings.Repeat("i", MaxSkillIdentityBytes)
		definition.Version = strings.Repeat("v", MaxSkillIdentityBytes)
		definition.Name = strings.Repeat("n", MaxSkillNameBytes)
		definition.Description = strings.Repeat("d", MaxSkillDescriptionBytes)
		action := definition.Actions["deploy"]
		delete(definition.Actions, "deploy")
		action.Name = strings.Repeat("a", MaxActionNameBytes)
		action.Description = strings.Repeat("x", MaxActionDescriptionBytes)
		action.Credentials = []CredentialRequirement{{Name: strings.Repeat("c", MaxCredentialFieldBytes), Kind: strings.Repeat("k", MaxCredentialFieldBytes)}}
		definition.Actions[action.Name] = action
		return definition
	}
	definition := atLimit()
	if err := validateDefinition(definition); err != nil {
		t.Fatalf("definition at the limits refused: %v", err)
	}
	candidate := DiscoveryCandidate{ID: definition.ID, Version: definition.Version, Name: definition.Name, Description: definition.Description, Readiness: DiscoveryReadinessBindable}
	for name, action := range definition.Actions {
		candidate.Actions = append(candidate.Actions, DiscoveryAction{Name: name, Description: action.Description, Risk: action.Risk})
		for _, credential := range action.Credentials {
			candidate.Credentials = append(candidate.Credentials, DiscoveryCredential{Name: credential.Name, Kind: credential.Kind})
		}
	}
	if _, err := NormalizeDiscoveryPage(DiscoveryRequest{Limit: 1}, &DiscoveryPage{Items: []DiscoveryCandidate{candidate}}); err != nil {
		t.Fatalf("accepted definition is not discoverable: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*Definition)
	}{
		{"id", func(d *Definition) { d.ID += "i" }},
		{"version", func(d *Definition) { d.Version += "v" }},
		{"name", func(d *Definition) { d.Name += "n" }},
		{"description", func(d *Definition) { d.Description += "d" }},
		{"action description", func(d *Definition) {
			for name, action := range d.Actions {
				action.Description += "x"
				d.Actions[name] = action
			}
		}},
		{"action name", func(d *Definition) {
			for name, action := range d.Actions {
				delete(d.Actions, name)
				action.Name += "a"
				d.Actions[action.Name] = action
			}
		}},
		{"credential", func(d *Definition) {
			for name, action := range d.Actions {
				action.Credentials[0].Kind += "k"
				d.Actions[name] = action
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := atLimit()
			test.mutate(definition)
			if err := validateDefinition(definition); err == nil {
				t.Fatal("definition past a discovery limit was accepted")
			}
		})
	}
}
