package expr

import (
	"strconv"
)

type tokenKind uint8

const (
	tokEOF tokenKind = iota
	tokNumber
	tokName
	tokOp
)

type token struct {
	kind tokenKind
	text string
	pos  int
	num  float64
}

var operators = []string{"<=", ">=", "==", "!=", "&&", "||", "+", "-", "*", "/", "^", "(", ")", ",", "<", ">", "!"}

func tokenize(src string) ([]token, error) {
	var tokens []token
	for pos := 0; pos < len(src); {
		c := src[pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			pos++
		case c >= '0' && c <= '9':
			start := pos
			for pos < len(src) && isDigit(src[pos]) {
				pos++
			}
			if pos < len(src) && src[pos] == '.' {
				pos++
				if pos >= len(src) || !isDigit(src[pos]) {
					return nil, errorf(PhaseSyntax, pos, "a digit must follow the decimal point")
				}
				for pos < len(src) && isDigit(src[pos]) {
					pos++
				}
			}
			if pos < len(src) && (src[pos] == 'e' || src[pos] == 'E') {
				pos++
				if pos < len(src) && (src[pos] == '+' || src[pos] == '-') {
					pos++
				}
				if pos >= len(src) || !isDigit(src[pos]) {
					return nil, errorf(PhaseSyntax, pos, "an exponent needs digits")
				}
				for pos < len(src) && isDigit(src[pos]) {
					pos++
				}
			}
			if pos < len(src) && (isLetter(src[pos]) || src[pos] == '.') {
				return nil, errorf(PhaseSyntax, pos, "unexpected %q after a number", src[pos])
			}
			value, err := strconv.ParseFloat(src[start:pos], 64)
			if err != nil {
				return nil, errorf(PhaseSyntax, start, "number %q is out of range", src[start:pos])
			}
			tokens = append(tokens, token{kind: tokNumber, text: src[start:pos], pos: start, num: value})
		case isLetter(c):
			start := pos
			for pos < len(src) && (isLetter(src[pos]) || isDigit(src[pos])) {
				pos++
			}
			if pos-start > MaxNameBytes {
				return nil, errorf(PhaseSyntax, start, "name is longer than %d bytes", MaxNameBytes)
			}
			tokens = append(tokens, token{kind: tokName, text: src[start:pos], pos: start})
		default:
			matched := false
			for _, op := range operators {
				if len(src)-pos >= len(op) && src[pos:pos+len(op)] == op {
					tokens = append(tokens, token{kind: tokOp, text: op, pos: pos})
					pos += len(op)
					matched = true
					break
				}
			}
			if !matched {
				return nil, errorf(PhaseSyntax, pos, "unexpected character %q", c)
			}
		}
	}
	return append(tokens, token{kind: tokEOF, pos: len(src)}), nil
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

type nodeKind uint8

const (
	nodeNumber nodeKind = iota
	nodeName
	nodeUnary
	nodeBinary
	nodeCall
)

type node struct {
	kind nodeKind
	pos  int
	num  float64
	name string // nodeName; nodeCall function; nodeUnary/nodeBinary operator
	args []*node
}

type parser struct {
	tokens []token
	at     int
	nodes  int
	depth  int
}

func parse(src string) (*node, error) {
	if len(src) == 0 || len(src) > MaxSourceBytes {
		return nil, errorf(PhaseSyntax, -1, "expression must be 1-%d bytes", MaxSourceBytes)
	}
	tokens, err := tokenize(src)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}
	root, err := p.expression()
	if err != nil {
		return nil, err
	}
	if next := p.peek(); next.kind != tokEOF {
		return nil, errorf(PhaseSyntax, next.pos, "unexpected %q", next.text)
	}
	return root, nil
}

func (p *parser) peek() token { return p.tokens[p.at] }

func (p *parser) accept(op string) bool {
	if next := p.peek(); next.kind == tokOp && next.text == op {
		p.at++
		return true
	}
	return false
}

func (p *parser) expect(op string) error {
	if !p.accept(op) {
		next := p.peek()
		if next.kind == tokEOF {
			return errorf(PhaseSyntax, next.pos, "expected %q before the end", op)
		}
		return errorf(PhaseSyntax, next.pos, "expected %q, found %q", op, next.text)
	}
	return nil
}

func (p *parser) make(n *node) (*node, error) {
	p.nodes++
	if p.nodes > MaxNodes {
		return nil, errorf(PhaseSyntax, n.pos, "expression has more than %d parts", MaxNodes)
	}
	return n, nil
}

func (p *parser) enter(pos int) error {
	p.depth++
	if p.depth > MaxNesting {
		return errorf(PhaseSyntax, pos, "expression nests deeper than %d levels", MaxNesting)
	}
	return nil
}

func (p *parser) expression() (*node, error) {
	if err := p.enter(p.peek().pos); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	return p.binaryLevel(0)
}

// levels lists binary operators from lowest to highest precedence.
var levels = [][]string{{"||"}, {"&&"}, {"<", "<=", ">", ">=", "==", "!="}, {"+", "-"}, {"*", "/"}}

const comparisonLevel = 2

func (p *parser) binaryLevel(level int) (*node, error) {
	if level == len(levels) {
		return p.unary()
	}
	left, err := p.binaryLevel(level + 1)
	if err != nil {
		return nil, err
	}
	for {
		next := p.peek()
		if next.kind != tokOp || !containsOp(levels[level], next.text) {
			return left, nil
		}
		p.at++
		right, err := p.binaryLevel(level + 1)
		if err != nil {
			return nil, err
		}
		left, err = p.make(&node{kind: nodeBinary, pos: next.pos, name: next.text, args: []*node{left, right}})
		if err != nil {
			return nil, err
		}
		if level == comparisonLevel {
			if after := p.peek(); after.kind == tokOp && containsOp(levels[level], after.text) {
				return nil, errorf(PhaseSyntax, after.pos, "comparisons cannot be chained; join them with &&")
			}
			return left, nil
		}
	}
}

func (p *parser) unary() (*node, error) {
	next := p.peek()
	if next.kind == tokOp && (next.text == "-" || next.text == "!") {
		p.at++
		if err := p.enter(next.pos); err != nil {
			return nil, err
		}
		operand, err := p.unary()
		p.depth--
		if err != nil {
			return nil, err
		}
		return p.make(&node{kind: nodeUnary, pos: next.pos, name: next.text, args: []*node{operand}})
	}
	return p.power()
}

func (p *parser) power() (*node, error) {
	base, err := p.primary()
	if err != nil {
		return nil, err
	}
	next := p.peek()
	if !p.accept("^") {
		return base, nil
	}
	if err := p.enter(next.pos); err != nil {
		return nil, err
	}
	exponent, err := p.unary()
	p.depth--
	if err != nil {
		return nil, err
	}
	return p.make(&node{kind: nodeBinary, pos: next.pos, name: "^", args: []*node{base, exponent}})
}

func (p *parser) primary() (*node, error) {
	next := p.peek()
	switch next.kind {
	case tokNumber:
		p.at++
		return p.make(&node{kind: nodeNumber, pos: next.pos, num: next.num})
	case tokName:
		p.at++
		if !p.accept("(") {
			return p.make(&node{kind: nodeName, pos: next.pos, name: next.text})
		}
		call := &node{kind: nodeCall, pos: next.pos, name: next.text}
		if next.text == "sum" {
			index := p.peek()
			if index.kind != tokName {
				return nil, errorf(PhaseSyntax, index.pos, "sum's first argument must be an index name, e.g. sum(i, 1, 10, i)")
			}
			p.at++
			name, err := p.make(&node{kind: nodeName, pos: index.pos, name: index.text})
			if err != nil {
				return nil, err
			}
			call.args = append(call.args, name)
			if err := p.expect(","); err != nil {
				return nil, err
			}
		}
		if !p.accept(")") {
			for {
				arg, err := p.expression()
				if err != nil {
					return nil, err
				}
				call.args = append(call.args, arg)
				if p.accept(")") {
					break
				}
				if err := p.expect(","); err != nil {
					return nil, err
				}
			}
		}
		return p.make(call)
	case tokOp:
		if next.text == "(" {
			p.at++
			inner, err := p.expression()
			if err != nil {
				return nil, err
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			return inner, nil
		}
		return nil, errorf(PhaseSyntax, next.pos, "unexpected %q", next.text)
	default:
		return nil, errorf(PhaseSyntax, next.pos, "expression ends too early")
	}
}

func containsOp(ops []string, op string) bool {
	for _, candidate := range ops {
		if candidate == op {
			return true
		}
	}
	return false
}
