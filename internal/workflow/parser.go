package workflow

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"
)

func evalNodeAttr(expr hclsyntax.Expression) (interface{}, error) {
	switch e := expr.(type) {
	case *hclsyntax.TemplateExpr:
		return evalTemplateExpr(e)
	case *hclsyntax.TemplateWrapExpr:
		return evalTemplateWrapExpr(e)
	case *hclsyntax.LiteralValueExpr:
		val := e.Val
		return ctyToGo(val), nil
	case *hclsyntax.ObjectConsExpr:
		result := make(map[string]interface{})
		for _, item := range e.Items {
			key, kd := item.KeyExpr.Value(nil)
			if kd.HasErrors() {
				return nil, fmt.Errorf("map key error: %w", kd)
			}
			val, err := evalNodeAttr(item.ValueExpr)
			if err != nil {
				return nil, err
			}
			result[key.AsString()] = val
		}
		return result, nil
	case *hclsyntax.TupleConsExpr:
		var result []interface{}
		for _, elem := range e.Exprs {
			val, err := evalNodeAttr(elem)
			if err != nil {
				return nil, err
			}
			result = append(result, val)
		}
		return result, nil
	default:
		val, diags := expr.Value(nil)
		if diags.HasErrors() {
			return nil, diags
		}
		return ctyToGo(val), nil
	}
}

func evalTemplateExpr(expr *hclsyntax.TemplateExpr) (string, error) {
	var parts []string
	for _, part := range expr.Parts {
		if lve, ok := part.(*hclsyntax.LiteralValueExpr); ok {
			val := lve.Val
			if val.Type() == cty.String {
				parts = append(parts, val.AsString())
			}
			continue
		}

		traversal, ok := exprToTraversal(part)
		if ok {
			parts = append(parts, traversalToString(traversal))
			continue
		}

		val, diags := part.Value(nil)
		if diags.HasErrors() {
			return "", fmt.Errorf("template part error: %w", diags)
		}
		if val.Type() == cty.String {
			parts = append(parts, val.AsString())
		}
	}
	return strings.Join(parts, ""), nil
}

func evalTemplateWrapExpr(expr *hclsyntax.TemplateWrapExpr) (string, error) {
	if traversal, ok := exprToTraversal(expr.Wrapped); ok {
		return "${" + traversalToString(traversal) + "}", nil
	}
	val, diags := expr.Value(nil)
	if diags.HasErrors() {
		return "", diags
	}
	return val.AsString(), nil
}

func exprToTraversal(expr hclsyntax.Expression) (hcl.Traversal, bool) {
	if st, ok := expr.(*hclsyntax.ScopeTraversalExpr); ok {
		return st.Traversal, true
	}
	return nil, false
}

func traversalToString(t hcl.Traversal) string {
	var parts []string
	for _, step := range t {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			parts = append(parts, s.Name)
		case hcl.TraverseAttr:
			parts = append(parts, s.Name)
		case hcl.TraverseIndex:
			idx := s.Key
			if idx.Type() == cty.Number {
				f, _ := idx.AsBigFloat().Float64()
				parts = append(parts, fmt.Sprintf("[%v]", f))
			} else {
				parts = append(parts, fmt.Sprintf("[%q]", idx.AsString()))
			}
		}
	}
	return strings.Join(parts, ".")
}

func ParseWorkflow(hclContent []byte, filename string) (*Workflow, error) {
	file, diags := hclsyntax.ParseConfig(hclContent, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse error: %w", diags)
	}

	content, _, schemaDiags := file.Body.PartialContent(workflowBlockSchema)
	if schemaDiags.HasErrors() {
		return nil, fmt.Errorf("schema error: %w", schemaDiags)
	}

	workflowBlocks := content.Blocks.OfType("workflow")
	if len(workflowBlocks) == 0 {
		return nil, fmt.Errorf("no workflow block found in %s", filename)
	}
	if len(workflowBlocks) > 1 {
		return nil, fmt.Errorf("multiple workflow blocks found in %s", filename)
	}

	block := workflowBlocks[0]
	wf := &Workflow{
		Name: block.Labels[0],
	}

	return wf, parseWorkflowContent(block.Body, wf)
}

func parseWorkflowContent(body hcl.Body, wf *Workflow) error {
	content, _, diags := body.PartialContent(workflowContentSchema)
	if diags.HasErrors() {
		return fmt.Errorf("workflow content error: %w", diags)
	}

	if desc, exists := content.Attributes["description"]; exists {
		val, valDiags := desc.Expr.Value(nil)
		if valDiags.HasErrors() {
			return fmt.Errorf("description parse error: %w", valDiags)
		}
		wf.Description = val.AsString()
	}

	seenNodes := make(map[string]string)

	for _, block := range content.Blocks {
		switch block.Type {
		case "node":
			node, err := parseNode(block, &seenNodes)
			if err != nil {
				return err
			}
			wf.Nodes = append(wf.Nodes, *node)

		case "edge":
			edge, err := parseEdge(block, &seenNodes)
			if err != nil {
				return err
			}
			wf.Edges = append(wf.Edges, *edge)
		}
	}

	return nil
}

func parseNode(block *hcl.Block, seenNodes *map[string]string) (*Node, error) {
	nodeType := block.Labels[0]
	nodeID := block.Labels[1]

	if existing, exists := (*seenNodes)[nodeID]; exists {
		return nil, fmt.Errorf("duplicate node id %q at %s (first defined at %s)", nodeID, block.DefRange.String(), existing)
	}
	(*seenNodes)[nodeID] = block.DefRange.String()

	syntaxBody, ok := bodySyntax(block.Body)
	if !ok {
		return nil, fmt.Errorf("unexpected body type for node %q", nodeID)
	}

	config := make(map[string]interface{})
	for name, attr := range syntaxBody.Attributes {
		val, err := evalNodeAttr(attr.Expr)
		if err != nil {
			return nil, fmt.Errorf("error parsing node %q attribute %q: %w", nodeID, name, err)
		}
		config[name] = val
	}

	return &Node{
		Type:   nodeType,
		ID:     nodeID,
		Config: config,
		Range:  block.DefRange,
	}, nil
}

func parseEdge(block *hcl.Block, _ *map[string]string) (*Edge, error) {
	from := block.Labels[0]
	to := block.Labels[1]

	content, _, diags := block.Body.PartialContent(edgeContentSchema)
	if diags.HasErrors() {
		return nil, fmt.Errorf("edge content error for %s -> %s: %w", from, to, diags)
	}

	edge := &Edge{
		From:  from,
		To:    to,
		Range: block.DefRange,
	}

	if cond, exists := content.Attributes["condition"]; exists {
		syntaxExpr, ok := cond.Expr.(hclsyntax.Expression)
		if !ok {
			val, valDiags := cond.Expr.Value(nil)
			if valDiags.HasErrors() {
				return nil, fmt.Errorf("condition parse error for edge %s -> %s: %w", from, to, valDiags)
			}
			edge.Condition = val.AsString()
		} else {
			val, err := evalNodeAttr(syntaxExpr)
			if err != nil {
				return nil, fmt.Errorf("condition parse error for edge %s -> %s: %v", from, to, err)
			}
			if s, ok := val.(string); ok {
				edge.Condition = s
			}
		}
	}

	return edge, nil
}

func ctyToGo(val cty.Value) interface{} {
	if !val.IsKnown() || val.IsNull() {
		return nil
	}

	ty := val.Type()
	switch {
	case ty == cty.String:
		return val.AsString()
	case ty == cty.Number:
		f, _ := val.AsBigFloat().Float64()
		return f
	case ty == cty.Bool:
		return val.True()
	case ty.IsListType() || ty.IsTupleType():
		count := 0
		it := val.ElementIterator()
		for it.Next() {
			count++
		}
		result := make([]interface{}, 0, count)
		it = val.ElementIterator()
		for it.Next() {
			_, v := it.Element()
			result = append(result, ctyToGo(v))
		}
		return result
	case ty.IsMapType() || ty.IsObjectType():
		result := make(map[string]interface{})
		it := val.ElementIterator()
		for it.Next() {
			k, v := it.Element()
			result[k.AsString()] = ctyToGo(v)
		}
		return result
	default:
		var fallback interface{}
		_ = gocty.FromCtyValue(val, &fallback)
		return fallback
	}
}


func ContainsInterpolation(s string) bool {
	for i := 0; i < len(s)-2; i++ {
		if s[i] == '$' && s[i+1] == '{' {
			return true
		}
	}
	return false
}


func InterpolationVars(s string) []string {
	var vars []string
	i := 0
	for i < len(s)-2 {
		if s[i] == '$' && s[i+1] == '{' {
			end := i + 2
			depth := 1
			for end < len(s) && depth > 0 {
				if s[end] == '{' {
					depth++
				} else if s[end] == '}' {
					depth--
				}
				end++
			}
			if depth == 0 {
				varName := s[i+2 : end-1]
				vars = append(vars, varName)
				i = end
			} else {
				i++
			}
		} else {
			i++
		}
	}
	return vars
}
