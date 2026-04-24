package workflow

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

type Workflow struct {
	Name        string
	Description string
	Nodes       []Node
	Edges       []Edge
	SourceFile  string
}

type Node struct {
	Type   string
	ID     string
	Config map[string]interface{}
	Range  hcl.Range
}

type Edge struct {
	From      string
	To        string
	Condition string
	Range     hcl.Range
}

var workflowBlockSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{
			Type:       "workflow",
			LabelNames: []string{"name"},
		},
	},
}

var workflowContentSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "description"},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{
			Type:       "node",
			LabelNames: []string{"type", "id"},
		},
		{
			Type:       "edge",
			LabelNames: []string{"from", "to"},
		},
	},
}

var edgeContentSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "condition"},
	},
}

func bodySyntax(body hcl.Body) (*hclsyntax.Body, bool) {
	syntaxBody, ok := body.(*hclsyntax.Body)
	return syntaxBody, ok
}
