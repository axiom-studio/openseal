// Package expr implements the explorer expression language: a tiny, safe,
// deterministic language over numbers. A TypeScript evaluator in the
// frontend must produce the same results; both are tested against
// testdata/fixtures.json.
//
// Syntax. Numbers are decimal literals (digits, optional fraction, optional
// exponent; no leading dot). Names are [A-Za-z_][A-Za-z0-9_]*. Operators by
// increasing precedence: ||, &&, comparisons (< <= > >= == !=, not
// chainable), + -, * /, prefix - and !, then ^ (right-associative, binding
// tighter than prefix minus: -2^2 is -4, 2^-1 is 0.5). Functions: min and max
// (2-8 arguments), round(x) and round(x, digits) with digits an integer
// literal 0-10, floor, ceil, abs, sqrt, log (natural), exp, if(cond, a, b)
// and sum(i, from, to, body). There are no strings, property access, loops,
// assignments or function definitions.
//
// Types. Expressions are Number or Bool. Arithmetic and functions take
// Numbers; comparisons give Bool; && || ! take Bool; == and != need equal
// types; if needs a Bool condition and branches of one type. Inputs are
// declared with a type; a Bool input is true when its value is non-zero. A
// Bool result evaluates to 1 or 0.
//
// Evaluation. Values are IEEE-754 binary64. && and || short-circuit and if
// evaluates only the chosen branch. Every arithmetic result and function
// result must be finite, otherwise evaluation fails (division by zero,
// sqrt(-1), log(0), overflow). sum evaluates from and to once; both must be
// integers; it adds body for i = from, from+1, ..., to in that order (zero
// when to < from) and fails beyond 600 iterations; sums do not nest.
// x^y with an integer y, |y| <= 1024, is computed by binary exponentiation:
// r = 1, b = x, e = |y|; loop { if e is odd { r *= b }; e >>= 1; if e == 0
// { break }; b *= b }; if y < 0 { r = 1/r }. Other powers use the platform
// pow. round(x) rounds half away from zero; round(x, d) is
// round(x * 10^d) / 10^d with 10^d taken from the exact literals 1, 10, ...,
// 1e10. min and max keep the first argument unless a later one is strictly
// smaller (larger). A result of -0 is returned as 0.
//
// Exactness. + - * /, comparisons, floor, ceil, abs, sqrt, round, min, max
// and integer powers are bit-identical across conforming implementations.
// exp, log and non-integer powers come from the platform math library and
// may differ in the last bits, so fixtures are compared with a relative
// tolerance (fixtures.json "tolerance": |got-want| <= tol * max(1, |want|)).
// Fixture numbers are JSON written in the shortest round-trip form, which is
// what JavaScript's Number#toString produces. Formatting results for display
// (currency, percent, digits) is the renderer's job, not this package's.
package expr

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Limits.
const (
	MaxSourceBytes   = 500
	MaxNodes         = 256
	MaxNesting       = 32
	MaxSumIterations = 600
	MaxNameBytes     = 64
)

// Type is the static type of an expression or input.
type Type uint8

const (
	Number Type = iota + 1
	Bool
)

func (t Type) String() string {
	switch t {
	case Number:
		return "number"
	case Bool:
		return "bool"
	default:
		return "invalid"
	}
}

// Phase says where an expression failed.
type Phase string

const (
	PhaseSyntax Phase = "syntax"
	PhaseType   Phase = "type"
	PhaseEval   Phase = "eval"
)

// Error is a compile or evaluation failure. Pos is a byte offset into the
// source, or -1.
type Error struct {
	Phase   Phase
	Pos     int
	Message string
}

func (e *Error) Error() string {
	if e.Pos >= 0 {
		return fmt.Sprintf("%s error at %d: %s", e.Phase, e.Pos, e.Message)
	}
	return fmt.Sprintf("%s error: %s", e.Phase, e.Message)
}

func errorf(phase Phase, pos int, format string, args ...interface{}) *Error {
	return &Error{Phase: phase, Pos: pos, Message: fmt.Sprintf(format, args...)}
}

// Program is a parsed and type-checked expression.
type Program struct {
	root   *node
	typ    Type
	inputs map[string]Type
}

// Type is the result type.
func (p *Program) Type() Type { return p.typ }

// Compile parses src and checks it against the declared inputs.
func Compile(src string, inputs map[string]Type) (*Program, error) {
	for name, typ := range inputs {
		if !validName(name) {
			return nil, errorf(PhaseType, -1, "input name %q must match [A-Za-z_][A-Za-z0-9_]* and be at most %d bytes", name, MaxNameBytes)
		}
		if _, reserved := functions[name]; reserved {
			return nil, errorf(PhaseType, -1, "input name %q is a function name", name)
		}
		if typ != Number && typ != Bool {
			return nil, errorf(PhaseType, -1, "input %q has no type", name)
		}
	}
	root, err := parse(src)
	if err != nil {
		return nil, err
	}
	c := checker{inputs: inputs, scope: map[string]bool{}}
	typ, err := c.check(root)
	if err != nil {
		return nil, err
	}
	return &Program{root: root, typ: typ, inputs: inputs}, nil
}

// Eval evaluates the program. env must hold a finite value for every
// declared input.
func (p *Program) Eval(env map[string]float64) (float64, error) {
	names := make([]string, 0, len(p.inputs))
	for name := range p.inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value, ok := env[name]
		if !ok {
			return 0, errorf(PhaseEval, -1, "input %q has no value", name)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, errorf(PhaseEval, -1, "input %q is not a finite number", name)
		}
	}
	e := evaluator{env: env, inputs: p.inputs, locals: map[string]float64{}}
	result, err := e.eval(p.root)
	if err != nil {
		return 0, err
	}
	if result.isBool {
		if result.b {
			return 1, nil
		}
		return 0, nil
	}
	if result.n == 0 {
		return 0, nil // normalise -0
	}
	return result.n, nil
}

func validName(name string) bool {
	if name == "" || len(name) > MaxNameBytes {
		return false
	}
	for index, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || index > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

type function struct {
	minArgs, maxArgs int
}

var functions = map[string]function{
	"min": {2, 8}, "max": {2, 8}, "round": {1, 2}, "floor": {1, 1}, "ceil": {1, 1}, "abs": {1, 1},
	"sqrt": {1, 1}, "log": {1, 1}, "exp": {1, 1}, "if": {3, 3}, "sum": {4, 4},
}

// FunctionNames lists the built-in functions.
func FunctionNames() []string {
	names := make([]string, 0, len(functions))
	for name := range functions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func quoteNames(names map[string]Type) string {
	list := make([]string, 0, len(names))
	for name := range names {
		list = append(list, name)
	}
	sort.Strings(list)
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}

// Uses lists the declared inputs the expression reads, sorted.
func (p *Program) Uses() []string {
	seen := map[string]bool{}
	var visit func(n *node)
	visit = func(n *node) {
		if n.kind == nodeName {
			if _, input := p.inputs[n.name]; input {
				seen[n.name] = true
			}
		}
		for _, arg := range n.args {
			visit(arg)
		}
	}
	visit(p.root)
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
