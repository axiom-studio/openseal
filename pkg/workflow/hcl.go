package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// ToHCL renders a Workflow as an HCL string.
func (w *Workflow) ToHCL() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`workflow "%s" {`+"\n", w.Name))
	if w.Description != "" {
		b.WriteString(fmt.Sprintf("  description = %q\n", w.Description))
	}
	for _, n := range w.Nodes {
		b.WriteString(fmt.Sprintf("\n  node %s %q {\n", n.Type, n.ID))
		keys := make([]string, 0, len(n.Config))
		for k := range n.Config {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(fmt.Sprintf("    %s = %s\n", k, formatHCLValue(n.Config[k])))
		}
		b.WriteString("  }\n")
	}
	for _, e := range w.Edges {
		b.WriteString(fmt.Sprintf("\n  edge %q %q", e.From, e.To))
		if e.Condition != "" {
			b.WriteString(fmt.Sprintf(" {\n    condition = %q\n  }", e.Condition))
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.String()
}
